package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/runas"
	"github.com/tavon-ai/proteos/guestagent/internal/secrets"
	"github.com/tavon-ai/proteos/guestagent/internal/term"
)

// replaceProviders wraps a provider-only push in a SecretsRequest (Phase 3
// generalized the guest's Replace to take file-kind items too).
func replaceProviders(sec *secrets.Store, providers map[string]guestwire.ProviderDef) error {
	return sec.Replace(guestwire.SecretsRequest{Providers: providers})
}

// newAgentServer starts a server wired with a real secrets store over a temp env
// dir, plus a stub "claude" provider whose launch command is a script that
// echoes its ANTHROPIC_API_KEY then stays alive (so the attach catches output).
func newAgentServer(t *testing.T, key string) (*httptest.Server, *secrets.Store) {
	t.Helper()
	dir := t.TempDir()

	script := filepath.Join(dir, "stub-claude.sh")
	body := "#!/bin/sh\necho \"CLAUDE_SAW_KEY=$ANTHROPIC_API_KEY\"\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	sec, err := secrets.New(filepath.Join(dir, "env"), runas.Root())
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		if err := replaceProviders(sec, map[string]guestwire.ProviderDef{
			"claude": {Command: script, Env: map[string]string{"ANTHROPIC_API_KEY": key}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	mgr := term.NewManager(term.Config{Shell: "/bin/sh", ScrollbackKiB: 64})
	t.Cleanup(mgr.Shutdown)
	ts := httptest.NewServer(New(mgr, nil, sec, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, sec
}

// TestAgentSessionSpawnsProviderWithEnv proves an agent-<key> session runs the
// injected provider command and that the command sees its secret env.
func TestAgentSessionSpawnsProviderWithEnv(t *testing.T) {
	ts, _ := newAgentServer(t, "sk-test-123")
	c := dial(t, wsURL(ts, "agent-claude"))
	defer c.Close(websocket.StatusNormalClosure, "")

	readHello(t, c)
	out := readBinaryUntil(t, c, "CLAUDE_SAW_KEY=sk-test-123", 5*time.Second)
	if out == "" {
		t.Fatal("expected provider to echo its injected key")
	}
}

// TestAgentSessionUninjectedClosesProviderUnavailable proves an agent session
// for a provider that was never injected closes with CloseProviderUnavailable.
func TestAgentSessionUninjectedClosesProviderUnavailable(t *testing.T) {
	ts, _ := newAgentServer(t, "") // no provider injected
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts, "agent-claude"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusInternalError, "")

	// The next read should observe the server's close with code 4003.
	_, _, err = c.Read(ctx)
	if code := websocket.CloseStatus(err); code != guestwire.CloseProviderUnavailable {
		t.Fatalf("close code = %d, want %d (err=%v)", code, guestwire.CloseProviderUnavailable, err)
	}
}

// TestAgentRepushReplacesDefinition proves a second push swaps the provider's
// env (a fresh agent session sees the new key).
func TestAgentRepushReplacesDefinition(t *testing.T) {
	ts, sec := newAgentServer(t, "sk-old")

	// Re-push with a new key, reusing the same stub script path from the first
	// injection so the command still exists.
	def, _ := sec.Get("claude")
	if err := replaceProviders(sec, map[string]guestwire.ProviderDef{
		"claude": {Command: def.Command, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-new"}},
	}); err != nil {
		t.Fatal(err)
	}

	c := dial(t, wsURL(ts, "agent-claude"))
	defer c.Close(websocket.StatusNormalClosure, "")
	readHello(t, c)
	readBinaryUntil(t, c, "CLAUDE_SAW_KEY=sk-new", 5*time.Second)
}

// TestPlainShellSessionUntouched proves the agent dispatch does not affect
// ordinary shell sessions when a secret store is present.
func TestPlainShellSessionUntouched(t *testing.T) {
	ts, _ := newAgentServer(t, "sk-test")
	c := dial(t, wsURL(ts, "main"))
	defer c.Close(websocket.StatusNormalClosure, "")
	readHello(t, c)
	sendInput(t, c, "echo plain-shell-ok\n")
	readBinaryUntil(t, c, "plain-shell-ok", 5*time.Second)
}

// TestAgentSessionDegradedClosesSetupFailed proves that when a provider's
// setup_command failed (it is injected but degraded), launching its agent
// session closes with CloseProviderUnavailable and the setup_failed reason —
// distinct from the not-injected case (Phase 6 decision #3).
func TestAgentSessionDegradedClosesSetupFailed(t *testing.T) {
	dir := t.TempDir()
	sec, err := secrets.New(filepath.Join(dir, "env"), runas.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceProviders(sec, map[string]guestwire.ProviderDef{
		"openai": {Command: "/bin/sh", SetupCommand: "exit 3", Env: map[string]string{"OPENAI_API_KEY": "sk"}},
	}); err != nil {
		t.Fatal(err)
	}
	sec.AwaitSetup()
	if !sec.Degraded("openai") {
		t.Fatal("precondition: openai should be degraded")
	}

	mgr := term.NewManager(term.Config{Shell: "/bin/sh", ScrollbackKiB: 64})
	t.Cleanup(mgr.Shutdown)
	ts := httptest.NewServer(New(mgr, nil, sec, nil).Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts, "agent-openai"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusInternalError, "")

	_, _, err = c.Read(ctx)
	if code := websocket.CloseStatus(err); code != guestwire.CloseProviderUnavailable {
		t.Fatalf("close code = %d, want %d (err=%v)", code, guestwire.CloseProviderUnavailable, err)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != guestwire.CloseReasonSetupFailed {
		t.Fatalf("close reason = %q, want %q (err=%v)", ce.Reason, guestwire.CloseReasonSetupFailed, err)
	}
}
