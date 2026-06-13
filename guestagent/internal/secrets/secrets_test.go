package secrets

import (
	"os"
	"path/filepath"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

func TestReplaceWritesEnvFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
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
	s, err := New(dir)
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
	s, _ := New(dir)
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
