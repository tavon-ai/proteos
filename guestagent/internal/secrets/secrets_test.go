package secrets

import (
	"os"
	"path/filepath"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

func TestReplaceWritesEnvFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, runas.Root())
	if err != nil {
		t.Fatal(err)
	}

	err = s.Replace(map[string]guestwire.ProviderDef{
		"claude": {Command: "claude", Env: map[string]string{"ANTHROPIC_API_KEY": "sk-abc"}},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	path := filepath.Join(dir, "claude.env")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("env file perm = %o, want 600", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "export ANTHROPIC_API_KEY='sk-abc'\n"
	if string(b) != want {
		t.Fatalf("env file = %q, want %q", b, want)
	}

	env, ok := s.EnvList("claude")
	if !ok || len(env) != 1 || env[0] != "ANTHROPIC_API_KEY=sk-abc" {
		t.Fatalf("EnvList = %v, ok=%v", env, ok)
	}
}

func TestReplaceIsReplaceAll(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, runas.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(map[string]guestwire.ProviderDef{
		"claude": {Command: "claude", Env: map[string]string{"ANTHROPIC_API_KEY": "v1"}},
		"gemini": {Command: "gemini", Env: map[string]string{"GEMINI_API_KEY": "g1"}},
	}); err != nil {
		t.Fatal(err)
	}

	// A second push that drops gemini must delete gemini.env and update claude.
	if err := s.Replace(map[string]guestwire.ProviderDef{
		"claude": {Command: "claude", Env: map[string]string{"ANTHROPIC_API_KEY": "v2"}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "gemini.env")); !os.IsNotExist(err) {
		t.Fatalf("gemini.env should be removed, stat err = %v", err)
	}
	if _, ok := s.Get("gemini"); ok {
		t.Fatal("gemini should be gone from the map")
	}
	env, _ := s.EnvList("claude")
	if len(env) != 1 || env[0] != "ANTHROPIC_API_KEY=v2" {
		t.Fatalf("claude env not replaced: %v", env)
	}
}

func TestSingleQuoteEscaping(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, runas.Root())
	if err := s.Replace(map[string]guestwire.ProviderDef{
		"x": {Command: "x", Env: map[string]string{"K": "a'b"}},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "x.env"))
	want := `export K='a'\''b'` + "\n"
	if string(b) != want {
		t.Fatalf("escaping = %q, want %q", b, want)
	}
}

// TestSetupCommandRunsAfterEnvFile proves a provider's setup_command runs after
// its env file exists (so the command can read the injected secrets) and that a
// clean run leaves the provider un-degraded (Phase 6 decision #3).
func TestSetupCommandRunsAfterEnvFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, runas.Root())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "setup-ran")
	// The setup command records the env file's existence and the injected key it
	// can see — proving ordering (env file first) and env overlay.
	setup := "test -f " + filepath.Join(dir, "openai.env") +
		" && printf '%s' \"$OPENAI_API_KEY\" > " + marker

	if err := s.Replace(map[string]guestwire.ProviderDef{
		"openai": {Command: "codex", SetupCommand: setup, Env: map[string]string{"OPENAI_API_KEY": "sk-codex-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.AwaitSetup()

	if s.Degraded("openai") {
		t.Fatal("provider degraded after a clean setup run")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("setup marker not written (env file missing or setup failed?): %v", err)
	}
	if string(got) != "sk-codex-1" {
		t.Fatalf("setup saw key %q, want sk-codex-1", got)
	}
}

// TestSetupFailureMarksDegraded proves a non-zero setup_command exit flips the
// provider's degraded flag.
func TestSetupFailureMarksDegraded(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, runas.Root())
	if err := s.Replace(map[string]guestwire.ProviderDef{
		"openai": {Command: "codex", SetupCommand: "exit 7", Env: map[string]string{"OPENAI_API_KEY": "sk"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.AwaitSetup()
	if !s.Degraded("openai") {
		t.Fatal("provider should be degraded after a failing setup command")
	}
}

// TestSuccessfulRepushClearsDegraded proves a later push whose setup succeeds
// clears a previously degraded provider (key rotation re-login).
func TestSuccessfulRepushClearsDegraded(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, runas.Root())

	if err := s.Replace(map[string]guestwire.ProviderDef{
		"openai": {Command: "codex", SetupCommand: "exit 1", Env: map[string]string{"OPENAI_API_KEY": "bad"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.AwaitSetup()
	if !s.Degraded("openai") {
		t.Fatal("expected degraded after failing setup")
	}

	if err := s.Replace(map[string]guestwire.ProviderDef{
		"openai": {Command: "codex", SetupCommand: "true", Env: map[string]string{"OPENAI_API_KEY": "good"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.AwaitSetup()
	if s.Degraded("openai") {
		t.Fatal("degraded flag should clear after a successful re-push")
	}
}

// TestProvidersWithoutSetupAreNeverDegraded proves the setup machinery leaves
// plain (no setup_command) providers untouched.
func TestProvidersWithoutSetupAreNeverDegraded(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, runas.Root())
	if err := s.Replace(map[string]guestwire.ProviderDef{
		"claude": {Command: "claude", Env: map[string]string{"ANTHROPIC_API_KEY": "sk"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.AwaitSetup()
	if s.Degraded("claude") {
		t.Fatal("a provider without a setup_command must never be degraded")
	}
}
