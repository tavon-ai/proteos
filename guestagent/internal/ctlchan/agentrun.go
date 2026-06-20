package ctlchan

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// AT1 headless agent runner. agent.run dispatches a non-interactive coding-agent
// run (Claude Code in print mode) in a project's working tree, parses its
// structured stream-json output, and reports the outcome back as agent.done. The
// run only ever produces a dirty working tree — it never commits; shipping is the
// separate, explicit GR flow.

const (
	// agentTaskTimeout bounds a single headless run. Coding tasks can be long, so
	// this is generous; the CP can also cancel (AT3).
	agentTaskTimeout = 30 * time.Minute
	// maxAgentSummary caps the result text relayed back to the CP.
	maxAgentSummary = 8 << 10 // 8 KiB
	// maxAgentStderr caps captured stderr used for a no-result failure detail.
	maxAgentStderr = 4 << 10
)

// agentResult is what the guest extracts from a headless run's stream-json.
type agentResult struct {
	SessionID  string
	IsError    bool
	Subtype    string
	Summary    string
	CostUSD    float64
	NumTurns   int
	DurationMS int
}

// streamEvent is the subset of a Claude Code stream-json line we read. Lines are
// one JSON object each: system/assistant/user events, then a final "result".
type streamEvent struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	TotalCost float64 `json:"total_cost_usd"`
	NumTurns  int     `json:"num_turns"`
	Duration  int     `json:"duration_ms"`
}

// runAgentTask runs the headless agent and reports the outcome (agent.done).
func (m *Manager) runAgentTask(p guestwire.AgentRunPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), agentTaskTimeout)
	defer cancel()

	res, err := m.runHeadless(ctx, p.Provider, p.Prompt, p.Path)
	if err != nil {
		slog.Warn("control: agent.run failed", "task", p.TaskID, "err", err)
		m.reportAgentDone(guestwire.AgentDonePayload{TaskID: p.TaskID, OK: false, Error: sanitizeAgentErr(err.Error())})
		return
	}
	done := guestwire.AgentDonePayload{
		TaskID:     p.TaskID,
		OK:         !res.IsError,
		SessionID:  res.SessionID,
		Summary:    res.Summary,
		CostUSD:    res.CostUSD,
		NumTurns:   res.NumTurns,
		DurationMS: res.DurationMS,
	}
	if res.IsError {
		done.Error = res.Subtype
		if done.Error == "" {
			done.Error = "agent reported an error"
		}
	}
	m.reportAgentDone(done)
}

// runHeadless resolves the provider's injected command + env, spawns the agent
// in print mode in dir with the prompt on stdin, and parses its stream-json.
func (m *Manager) runHeadless(ctx context.Context, provider, prompt, dir string) (agentResult, error) {
	if m.sec == nil {
		return agentResult{}, fmt.Errorf("no provider secrets injected")
	}
	def, ok := m.sec.Get(provider)
	if !ok {
		return agentResult{}, fmt.Errorf("provider %q not injected", provider)
	}
	argv, err := headlessArgv(def)
	if err != nil {
		return agentResult{}, err
	}
	if err := m.validateRepoPath(dir); err != nil {
		return agentResult{}, err
	}
	env, _ := m.sec.EnvList(provider)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0"), env...)
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	cmd.Stdin = strings.NewReader(prompt)
	stderr := &cappedBuffer{max: maxAgentStderr}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agentResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return agentResult{}, err
	}
	res, sawResult, perr := parseStreamJSON(stdout)
	waitErr := cmd.Wait()

	// A parsed result event is authoritative (it carries is_error), even if the
	// process then exited non-zero. With no result event, the run failed.
	if !sawResult {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		if detail == "" && perr != nil {
			detail = perr.Error()
		}
		if detail == "" {
			detail = "agent produced no result"
		}
		return agentResult{}, fmt.Errorf("%s", detail)
	}
	return res, nil
}

// headlessArgv builds the Claude Code print-mode command. Only Claude Code is
// supported on the headless lane (AT1); the flags are claude-specific. The prompt
// is delivered on stdin (not an argument) to avoid quoting issues. Permissions
// are bypassed because the microVM is itself the sandbox the permission system
// would otherwise stand in for.
func headlessArgv(def guestwire.ProviderDef) ([]string, error) {
	fields := strings.Fields(def.Command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("provider has no launch command")
	}
	if filepath.Base(fields[0]) != "claude" {
		return nil, fmt.Errorf("provider command %q is not headless-capable", fields[0])
	}
	argv := append([]string{}, fields...)
	argv = append(argv, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
	return argv, nil
}

// parseStreamJSON reads a headless run's stream-json (one JSON object per line),
// tracking the session id and capturing the final "result" event. It tolerates
// non-JSON noise lines. sawResult is false when no result event appeared.
func parseStreamJSON(r io.Reader) (res agentResult, sawResult bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool outputs can be large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev streamEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		if ev.Type == "result" {
			sawResult = true
			res.IsError = ev.IsError
			res.Subtype = ev.Subtype
			res.Summary = truncate(ev.Result, maxAgentSummary)
			res.CostUSD = ev.TotalCost
			res.NumTurns = ev.NumTurns
			res.DurationMS = ev.Duration
		}
	}
	return res, sawResult, sc.Err()
}

// reportAgentDone notifies the CP of an agent run completion (agent.done).
func (m *Manager) reportAgentDone(done guestwire.AgentDonePayload) {
	c := m.currentConn()
	if c == nil {
		slog.Warn("control: cannot report agent.done — no channel", "task", done.TaskID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.notify(ctx, guestwire.OpAgentDone, done); err != nil {
		slog.Warn("control: report agent.done failed", "task", done.TaskID, "err", err)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}

func sanitizeAgentErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// cappedBuffer is a bounded io.Writer for capturing a short stderr tail without
// letting a chatty agent balloon memory.
type cappedBuffer struct {
	max int
	buf []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return string(b.buf) }
