package ctlchan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

func TestParseStreamJSON_Success(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-123"}`,
		`{"type":"assistant","message":{"role":"assistant"}}`,
		``, // tolerate a blank line
		`not json — tolerate noise`,
		`{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"sess-123","total_cost_usd":0.0123,"num_turns":3,"duration_ms":4567}`,
	}, "\n")

	res, saw, err := parseStreamJSON(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !saw {
		t.Fatal("expected a result event")
	}
	if res.IsError || res.SessionID != "sess-123" || res.Summary != "all done" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.CostUSD != 0.0123 || res.NumTurns != 3 || res.DurationMS != 4567 {
		t.Fatalf("usage not parsed: %+v", res)
	}
}

func TestParseStreamJSON_Error(t *testing.T) {
	stream := `{"type":"system","session_id":"s1"}` + "\n" +
		`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s1"}`
	res, saw, err := parseStreamJSON(strings.NewReader(stream), nil)
	if err != nil || !saw {
		t.Fatalf("parse: saw=%v err=%v", saw, err)
	}
	if !res.IsError || res.Subtype != "error_max_turns" {
		t.Fatalf("expected error result, got %+v", res)
	}
}

func TestParseStreamJSON_NoResult(t *testing.T) {
	stream := `{"type":"system","session_id":"s1"}` + "\n" + `{"type":"assistant"}`
	_, saw, err := parseStreamJSON(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if saw {
		t.Fatal("did not expect a result event")
	}
}

func TestParseStreamJSON_EmitsNormalizedEvents(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.txt\nb.txt","is_error":false}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"   "}]}}`, // blank text dropped
		`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}`,
	}, "\n")

	var got []guestwire.AgentEventPayload
	res, saw, err := parseStreamJSON(strings.NewReader(stream), func(ev guestwire.AgentEventPayload) {
		got = append(got, ev)
	})
	if err != nil || !saw {
		t.Fatalf("parse: saw=%v err=%v", saw, err)
	}
	// The terminal result is NOT emitted as an agent.event.
	if len(got) != 3 {
		t.Fatalf("want 3 normalized events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != guestwire.AgentEventAssistantText || got[0].Text != "Let me look." {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].Kind != guestwire.AgentEventToolUse || got[1].Tool != "Bash" || got[1].ToolID != "t1" {
		t.Errorf("event 1 = %+v", got[1])
	}
	if string(got[1].Input) != `{"command":"ls"}` {
		t.Errorf("tool input not relayed: %s", got[1].Input)
	}
	if got[2].Kind != guestwire.AgentEventToolResult || got[2].ToolID != "t1" || got[2].Output != "a.txt\nb.txt" {
		t.Errorf("event 2 = %+v", got[2])
	}
	if res.Summary != "ok" {
		t.Errorf("result summary = %q", res.Summary)
	}
}

func TestToolResultText_StringAndBlocks(t *testing.T) {
	if got := toolResultText([]byte(`"plain string"`)); got != "plain string" {
		t.Errorf("string form = %q", got)
	}
	if got := toolResultText([]byte(`[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]`)); got != "line1line2" {
		t.Errorf("block form = %q", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestBoundedJSON_Truncates(t *testing.T) {
	small := json.RawMessage(`{"a":1}`)
	if string(boundedJSON(small, 100)) != `{"a":1}` {
		t.Error("small JSON should pass through")
	}
	big := json.RawMessage(`{"x":"` + strings.Repeat("y", 200) + `"}`)
	out := boundedJSON(big, 50)
	if string(out) != `{"_truncated":true}` {
		t.Errorf("oversized JSON should be a placeholder, got %s", out)
	}
	// The placeholder must itself be valid JSON.
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Errorf("placeholder not valid JSON: %v", err)
	}
}

func TestHeadlessArgv(t *testing.T) {
	argv, err := headlessArgv(guestwire.ProviderDef{Command: "claude"}, "")
	if err != nil {
		t.Fatalf("headlessArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"claude", "-p", "--output-format stream-json", "--verbose", "--dangerously-skip-permissions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
	// A fresh run carries no --resume.
	if strings.Contains(joined, "--resume") {
		t.Errorf("fresh argv should not resume: %q", joined)
	}
	// A non-claude provider is rejected on the headless lane.
	if _, err := headlessArgv(guestwire.ProviderDef{Command: "gemini"}, ""); err == nil {
		t.Error("expected non-claude provider to be rejected")
	}
	if _, err := headlessArgv(guestwire.ProviderDef{Command: ""}, ""); err == nil {
		t.Error("expected empty command to be rejected")
	}
}

func TestHeadlessArgv_Resume(t *testing.T) {
	argv, err := headlessArgv(guestwire.ProviderDef{Command: "claude"}, "sess-abc")
	if err != nil {
		t.Fatalf("headlessArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--resume sess-abc") {
		t.Errorf("resume argv missing --resume: %q", joined)
	}
}

// fakeSecrets serves a single provider's command + env to the headless runner.
type fakeSecrets struct {
	cmd string
	env []string
}

func (f fakeSecrets) Get(string) (guestwire.ProviderDef, bool) {
	return guestwire.ProviderDef{Command: f.cmd}, f.cmd != ""
}
func (f fakeSecrets) EnvList(string) ([]string, bool) { return f.env, true }

func TestRunHeadless(t *testing.T) {
	// A fake `claude` on PATH that consumes the prompt on stdin and emits a
	// minimal stream-json transcript (mocking the real CLI).
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '%s\n' '{"type":"system","session_id":"sess-xyz"}'` + "\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done it","session_id":"sess-xyz","total_cost_usd":0.5,"num_turns":2}'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work, "HOME=" + t.TempDir()}, runas.Root(), nil, fakeSecrets{cmd: "claude"})
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	res, err := m.runHeadless(context.Background(), "claude", "make it responsive", repo, "", nil)
	if err != nil {
		t.Fatalf("runHeadless: %v", err)
	}
	if res.SessionID != "sess-xyz" || res.Summary != "done it" || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.CostUSD != 0.5 || res.NumTurns != 2 {
		t.Fatalf("usage not captured: %+v", res)
	}
}

func TestAgentDonePayload(t *testing.T) {
	// Canceled outranks an error: the run is reported as canceled, not failed.
	d := agentDonePayload("t1", agentResult{}, errors.New("boom"), true)
	if d.OK || !d.Canceled || d.Error != "canceled" {
		t.Fatalf("canceled mapping = %+v", d)
	}
	// A process/parse error with no cancel → failed (OK=false, Canceled=false).
	d = agentDonePayload("t1", agentResult{}, errors.New("boom"), false)
	if d.OK || d.Canceled || d.Error == "" {
		t.Fatalf("error mapping = %+v", d)
	}
	// A clean result → OK with session id + usage.
	d = agentDonePayload("t1", agentResult{SessionID: "s", Summary: "ok", CostUSD: 0.2}, nil, false)
	if !d.OK || d.Canceled || d.SessionID != "s" || d.Summary != "ok" {
		t.Fatalf("success mapping = %+v", d)
	}
	// An is_error result → not OK, error taken from the subtype.
	d = agentDonePayload("t1", agentResult{IsError: true, Subtype: "error_max_turns"}, nil, false)
	if d.OK || d.Error != "error_max_turns" {
		t.Fatalf("is_error mapping = %+v", d)
	}
}

func TestCancelRun_Registry(t *testing.T) {
	m := New([]string{"PROTEOS_WORKSPACE=" + t.TempDir()}, runas.Root(), nil, nil)
	// Canceling an unknown task is a no-op (idempotent).
	if m.cancelRun("nope") {
		t.Error("cancelRun on unknown task should be false")
	}
	called := false
	m.registerRun("t1", &agentRun{cancel: func() { called = true }})
	if !m.cancelRun("t1") {
		t.Fatal("cancelRun should find the registered run")
	}
	if !called {
		t.Error("cancelRun should invoke the run's cancel func")
	}
	if !m.runWasCanceled("t1") {
		t.Error("runWasCanceled should reflect the cancel")
	}
	m.unregisterRun("t1")
	if m.runWasCanceled("t1") {
		t.Error("an unregistered run should not report canceled")
	}
}

func TestRunHeadless_CanceledKillsProcessGroup(t *testing.T) {
	// A fake `claude` that emits one event then sleeps far longer than the test —
	// canceling the context must terminate it (and its group) promptly.
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '%s\n' '{"type":"system","session_id":"s1"}'` + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work, "HOME=" + t.TempDir()}, runas.Root(), nil, fakeSecrets{cmd: "claude"})
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = m.runHeadless(ctx, "claude", "x", repo, "", nil)
		close(done)
	}()
	// Let it spawn + emit, then cancel.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// returned promptly — the kill worked
	case <-time.After(10 * time.Second):
		t.Fatal("runHeadless did not return promptly after cancel (process not killed)")
	}
}

func TestRunHeadless_NoSecrets(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil, nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	if _, err := m.runHeadless(context.Background(), "claude", "x", repo, "", nil); err == nil {
		t.Error("expected error when no secrets store is wired")
	}
}
