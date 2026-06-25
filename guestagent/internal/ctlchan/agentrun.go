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

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// AT1 headless agent runner. agent.run dispatches a non-interactive coding-agent
// run (Claude Code in print mode, or pi.dev in --mode json) in a project's working
// tree, parses its structured JSON event stream, and reports the outcome back as
// agent.done. The run only ever produces a dirty working tree — it never commits;
// shipping is the separate, explicit GR flow. Each headless-capable provider has
// its own argv (headlessArgv) and stream parser (parseHeadlessStream); both emit
// the same normalized AgentEventPayload steps and agentResult so the CP/UI never
// see a provider-specific wire format.

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
// one JSON object each: a system/init event, assistant/user message events (whose
// content blocks carry text + tool_use / tool_result), then a final "result".
type streamEvent struct {
	Type      string      `json:"type"`
	Subtype   string      `json:"subtype"`
	SessionID string      `json:"session_id"`
	IsError   bool        `json:"is_error"`
	Result    string      `json:"result"`
	TotalCost float64     `json:"total_cost_usd"`
	NumTurns  int         `json:"num_turns"`
	Duration  int         `json:"duration_ms"`
	Message   *rawMessage `json:"message"`
}

// rawMessage is the assistant/user message envelope of a stream-json event; its
// content is an array of typed blocks.
type rawMessage struct {
	Content []rawBlock `json:"content"`
}

// rawBlock is one content block of a message: text, a tool_use (assistant calling
// a tool), or a tool_result (user channel returning a tool's output).
type rawBlock struct {
	Type      string          `json:"type"` // text | tool_use | tool_result
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool_use tool name
	Input     json.RawMessage `json:"input"`       // tool_use input
	ToolUseID string          `json:"tool_use_id"` // tool_result back-reference
	Content   json.RawMessage `json:"content"`     // tool_result output (string or block array)
	IsError   bool            `json:"is_error"`    // tool_result error flag
}

// piEvent is the subset of a pi.dev `--mode json` line we read. pi emits one
// JSON object per line: first a session header ({"type":"session","id":...}),
// then a stream of AgentSessionEvents — message_start/end (whose content carries
// text + toolCall blocks), tool_execution_start/end, turn_end, and a terminal
// agent_end. There is no single Claude-style "result" event; agent_end is the
// terminal marker and per-message usage carries cost.
type piEvent struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`         // session header session id
	Message    *piMessage      `json:"message"`    // message_start/end, turn_end
	ToolCallID string          `json:"toolCallId"` // tool_execution_start/end
	ToolName   string          `json:"toolName"`   // tool_execution_start/end
	Args       json.RawMessage `json:"args"`       // tool_execution_start input
	Result     json.RawMessage `json:"result"`     // tool_execution_end output
	IsError    bool            `json:"isError"`    // tool_execution_end error flag
}

// piMessage is the assistant/user message envelope of a pi event; content is an
// array of typed blocks and the assistant message carries usage + stopReason.
type piMessage struct {
	Role         string    `json:"role"`
	Content      []piBlock `json:"content"`
	Usage        *piUsage  `json:"usage"`
	StopReason   string    `json:"stopReason"`   // stop | length | toolUse | error | aborted
	ErrorMessage string    `json:"errorMessage"` // set when stopReason is error/aborted
}

// piBlock is one content block of a pi message. We only read text here; tool calls
// are relayed from the dedicated tool_execution_* events (which also carry results).
type piBlock struct {
	Type string `json:"type"` // text | thinking | toolCall
	Text string `json:"text"`
}

// piUsage is the per-message usage; cost.total is the message's dollar cost, which
// we sum across the run.
type piUsage struct {
	Cost struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// agentRun tracks one in-flight headless run so agent.cancel (AT3) can stop it.
// canceled records that a cancel was requested, so the terminated run reports
// `canceled` rather than `failed`. Its fields are guarded by Manager.runMu.
type agentRun struct {
	cancel   context.CancelFunc
	canceled bool
}

// runAgentTask runs the headless agent and reports the outcome (agent.done). ctx
// (with its cancel held in run) bounds the run and is the cancellation handle;
// run is already registered by the dispatcher before this goroutine starts, so a
// cancel arriving immediately after the ack still lands on a live run.
func (m *Manager) runAgentTask(ctx context.Context, run *agentRun, p guestwire.AgentRunPayload) {
	defer run.cancel()
	defer m.unregisterRun(p.TaskID)

	emit := func(ev guestwire.AgentEventPayload) {
		ev.TaskID = p.TaskID
		m.emitAgentEvent(ev)
	}
	res, err := m.runHeadless(ctx, p.Provider, p.Prompt, p.Path, p.SessionID, emit)
	if err != nil && !m.runWasCanceled(p.TaskID) {
		slog.Warn("control: agent.run failed", "task", p.TaskID, "err", err)
	}
	m.reportAgentDone(agentDonePayload(p.TaskID, res, err, m.runWasCanceled(p.TaskID)))
}

// agentDonePayload maps a run's outcome to the agent.done completion frame. A
// canceled run is reported as canceled (not failed); otherwise a process/parse
// error is a failure, and a parsed result carries its is_error + usage.
func agentDonePayload(taskID string, res agentResult, err error, canceled bool) guestwire.AgentDonePayload {
	switch {
	case canceled:
		return guestwire.AgentDonePayload{TaskID: taskID, OK: false, Canceled: true, Error: "canceled"}
	case err != nil:
		return guestwire.AgentDonePayload{TaskID: taskID, OK: false, Error: sanitizeAgentErr(err.Error())}
	}
	done := guestwire.AgentDonePayload{
		TaskID:     taskID,
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
	return done
}

// registerRun records an in-flight run before its goroutine starts.
func (m *Manager) registerRun(taskID string, run *agentRun) {
	m.runMu.Lock()
	m.runs[taskID] = run
	m.runMu.Unlock()
}

// unregisterRun drops a finished run from the registry.
func (m *Manager) unregisterRun(taskID string) {
	m.runMu.Lock()
	delete(m.runs, taskID)
	m.runMu.Unlock()
}

// cancelRun marks a running task canceled and signals its context (which kills
// the agent's process group). It is idempotent and a no-op if the task is not
// running on this guest. Returns whether a live run was found.
func (m *Manager) cancelRun(taskID string) bool {
	m.runMu.Lock()
	run, ok := m.runs[taskID]
	if ok {
		run.canceled = true
	}
	m.runMu.Unlock()
	if ok {
		run.cancel()
	}
	return ok
}

// runWasCanceled reports whether the named run was asked to cancel.
func (m *Manager) runWasCanceled(taskID string) bool {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	run, ok := m.runs[taskID]
	return ok && run.canceled
}

// runHeadless resolves the provider's injected command + env, spawns the agent
// in print mode in dir with the prompt on stdin, and parses its stream-json,
// relaying each normalized step to emit as it arrives (AT2). A non-empty
// resumeID continues a prior agent session (AT4: claude --resume <id>).
func (m *Manager) runHeadless(ctx context.Context, provider, prompt, dir, resumeID string, emit func(guestwire.AgentEventPayload)) (agentResult, error) {
	if m.sec == nil {
		return agentResult{}, fmt.Errorf("no provider secrets injected")
	}
	def, ok := m.sec.Get(provider)
	if !ok {
		return agentResult{}, fmt.Errorf("provider %q not injected", provider)
	}
	argv, err := headlessArgv(def, resumeID)
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
	// Run the agent in its own process group so a cancel (AT3) or timeout kills the
	// whole tree — claude spawns child processes (tool calls) that would otherwise
	// be orphaned. Setpgid makes the child the group leader (pgid == pid).
	sysProc := &syscall.SysProcAttr{Setpgid: true}
	if cred := m.owner.Credential(); cred != nil {
		sysProc.Credential = cred
	}
	cmd.SysProcAttr = sysProc
	// On ctx cancel (cancel or timeout) signal the whole group, not just the
	// leader; WaitDelay bounds how long Wait lingers for the group to die.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
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
	res, sawResult, perr := parseHeadlessStream(headlessBinary(def), stdout, emit)
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

// headlessBinary returns the base name of a provider's launch command (e.g.
// "claude", "pi"), which selects the argv flags and the stream parser. It is empty
// when the provider has no command.
func headlessBinary(def guestwire.ProviderDef) string {
	fields := strings.Fields(def.Command)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

// headlessArgv builds a provider's non-interactive (print/JSON-stream) command.
// Only the headless-capable providers are supported on this lane (AT1); the flags
// are per-binary. The prompt is delivered on stdin (not an argument) to avoid
// quoting issues. Tool permissions are not gated because the microVM is itself the
// sandbox the permission system would otherwise stand in for. A non-empty resumeID
// continues a prior session (AT4): claude via --resume, pi via --session.
func headlessArgv(def guestwire.ProviderDef, resumeID string) ([]string, error) {
	fields := strings.Fields(def.Command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("provider has no launch command")
	}
	argv := append([]string{}, fields...)
	switch headlessBinary(def) {
	case "claude":
		argv = append(argv, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
		if resumeID != "" {
			argv = append(argv, "--resume", resumeID)
		}
	case "pi":
		// pi.dev emits a JSON event stream in --mode json. Resume targets a stored
		// session by id via --session (not --resume, which is an interactive picker
		// that would hang headless).
		argv = append(argv, "--mode", "json")
		if resumeID != "" {
			argv = append(argv, "--session", resumeID)
		}
	default:
		return nil, fmt.Errorf("provider command %q is not headless-capable", fields[0])
	}
	return argv, nil
}

// parseHeadlessStream routes a headless run's stdout to the parser matching its
// binary: pi.dev's AgentSessionEvent stream, or the default Claude Code stream-json.
func parseHeadlessStream(bin string, r io.Reader, emit func(guestwire.AgentEventPayload)) (agentResult, bool, error) {
	if bin == "pi" {
		return parsePiStream(r, emit)
	}
	return parseStreamJSON(r, emit)
}

// parseStreamJSON reads a headless run's stream-json (one JSON object per line),
// tracking the session id, relaying each normalized assistant/tool step to emit
// (AT2), and capturing the final "result" event. It tolerates non-JSON noise
// lines. sawResult is false when no result event appeared. emit may be nil (AT1
// callers that only want the final result).
func parseStreamJSON(r io.Reader, emit func(guestwire.AgentEventPayload)) (res agentResult, sawResult bool, err error) {
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
			continue // the terminal result is reported via agent.done, not agent.event
		}
		if emit != nil && ev.Message != nil {
			for _, ne := range normalizeBlocks(ev.Type, ev.Message.Content) {
				emit(ne)
			}
		}
	}
	return res, sawResult, sc.Err()
}

// normalizeBlocks turns one message's content blocks into normalized agent
// events (AT2). assistant messages yield assistant_text / tool_use; user messages
// yield tool_result. Empty text blocks are skipped. Text fields are bounded.
func normalizeBlocks(msgType string, blocks []rawBlock) []guestwire.AgentEventPayload {
	var out []guestwire.AgentEventPayload
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				out = append(out, guestwire.AgentEventPayload{
					Kind: guestwire.AgentEventAssistantText,
					Text: truncate(b.Text, guestwire.AgentEventTextCap),
				})
			}
		case "tool_use":
			out = append(out, guestwire.AgentEventPayload{
				Kind:   guestwire.AgentEventToolUse,
				Tool:   b.Name,
				ToolID: b.ID,
				Input:  boundedJSON(b.Input, guestwire.AgentEventToolCap),
			})
		case "tool_result":
			out = append(out, guestwire.AgentEventPayload{
				Kind:    guestwire.AgentEventToolResult,
				ToolID:  b.ToolUseID,
				Output:  truncate(toolResultText(b.Content), guestwire.AgentEventToolCap),
				IsError: b.IsError,
			})
		}
	}
	return out
}

// toolResultText flattens a tool_result content value, which is either a JSON
// string or an array of {type:"text",text:...} blocks, into plain text.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []rawBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Text != "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return string(raw) // last resort: the raw JSON, still bounded by the caller
}

// parsePiStream reads a pi.dev `--mode json` event stream (one JSON object per
// line), tracking the session id from the header, relaying each normalized
// assistant/tool step to emit (AT2), and treating agent_end as the terminal
// marker. Cost is summed from per-message usage and the summary is the last
// assistant message's text. It tolerates non-JSON noise lines; sawResult is false
// when no agent_end appeared (the run failed without completing). emit may be nil.
func parsePiStream(r io.Reader, emit func(guestwire.AgentEventPayload)) (res agentResult, sawResult bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool outputs can be large
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev piEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "session":
			if ev.ID != "" {
				res.SessionID = ev.ID
			}
		case "tool_execution_start":
			if emit != nil {
				emit(guestwire.AgentEventPayload{
					Kind:   guestwire.AgentEventToolUse,
					Tool:   ev.ToolName,
					ToolID: ev.ToolCallID,
					Input:  boundedJSON(ev.Args, guestwire.AgentEventToolCap),
				})
			}
		case "tool_execution_end":
			if emit != nil {
				emit(guestwire.AgentEventPayload{
					Kind:    guestwire.AgentEventToolResult,
					ToolID:  ev.ToolCallID,
					Output:  truncate(piToolResultText(ev.Result), guestwire.AgentEventToolCap),
					IsError: ev.IsError,
				})
			}
		case "message_end":
			if ev.Message == nil || ev.Message.Role != "assistant" {
				continue
			}
			if ev.Message.Usage != nil {
				res.CostUSD += ev.Message.Usage.Cost.Total
			}
			var summary strings.Builder
			for _, b := range ev.Message.Content {
				if b.Type != "text" {
					continue
				}
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				if emit != nil {
					emit(guestwire.AgentEventPayload{
						Kind: guestwire.AgentEventAssistantText,
						Text: truncate(b.Text, guestwire.AgentEventTextCap),
					})
				}
				if summary.Len() > 0 {
					summary.WriteString("\n")
				}
				summary.WriteString(b.Text)
			}
			// The last assistant message's text is the run summary (its content
			// overwrites earlier turns, mirroring claude's terminal result string).
			if summary.Len() > 0 {
				res.Summary = truncate(summary.String(), maxAgentSummary)
			}
			if ev.Message.StopReason == "error" || ev.Message.StopReason == "aborted" {
				res.IsError = true
				res.Subtype = ev.Message.ErrorMessage
			}
		case "turn_end":
			res.NumTurns++
		case "agent_end":
			sawResult = true // pi's terminal marker; reported via agent.done
		}
	}
	return res, sawResult, sc.Err()
}

// piToolResultText flattens a pi tool_execution_end result value (an arbitrary
// JSON value) into text: a JSON string is unquoted, anything else is its raw JSON.
// The caller bounds the length.
func piToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// boundedJSON returns raw if it fits the cap, else a small JSON placeholder so a
// huge tool input never balloons a frame (and the truncation is never invalid
// JSON the CP would choke on).
func boundedJSON(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) == 0 || len(raw) <= limit {
		return raw
	}
	return json.RawMessage(`{"_truncated":true}`)
}

// emitAgentEvent relays one normalized event to the CP over the live channel
// (AT2). Best-effort: a missing channel or write error drops the event — the SSE
// client recovers the run's outcome from the authoritative agent.done.
func (m *Manager) emitAgentEvent(ev guestwire.AgentEventPayload) {
	c := m.currentConn()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.notify(ctx, guestwire.OpAgentEvent, ev); err != nil {
		slog.Debug("control: emit agent.event failed", "task", ev.TaskID, "err", err)
	}
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
