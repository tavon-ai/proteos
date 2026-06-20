package ctlchan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	res, saw, err := parseStreamJSON(strings.NewReader(stream))
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
	res, saw, err := parseStreamJSON(strings.NewReader(stream))
	if err != nil || !saw {
		t.Fatalf("parse: saw=%v err=%v", saw, err)
	}
	if !res.IsError || res.Subtype != "error_max_turns" {
		t.Fatalf("expected error result, got %+v", res)
	}
}

func TestParseStreamJSON_NoResult(t *testing.T) {
	stream := `{"type":"system","session_id":"s1"}` + "\n" + `{"type":"assistant"}`
	_, saw, err := parseStreamJSON(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if saw {
		t.Fatal("did not expect a result event")
	}
}

func TestHeadlessArgv(t *testing.T) {
	argv, err := headlessArgv(guestwire.ProviderDef{Command: "claude"})
	if err != nil {
		t.Fatalf("headlessArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"claude", "-p", "--output-format stream-json", "--verbose", "--dangerously-skip-permissions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
	// A non-claude provider is rejected on the headless lane.
	if _, err := headlessArgv(guestwire.ProviderDef{Command: "gemini"}); err == nil {
		t.Error("expected non-claude provider to be rejected")
	}
	if _, err := headlessArgv(guestwire.ProviderDef{Command: ""}); err == nil {
		t.Error("expected empty command to be rejected")
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

	res, err := m.runHeadless(context.Background(), "claude", "make it responsive", repo)
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

func TestRunHeadless_NoSecrets(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil, nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	if _, err := m.runHeadless(context.Background(), "claude", "x", repo); err == nil {
		t.Error("expected error when no secrets store is wired")
	}
}
