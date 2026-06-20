package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tavon/proteos/cli/internal/config"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	want := config.Credentials{BaseURL: "http://localhost:8080", Token: "proteos_pat_abc", Login: "octocat"}
	if err := config.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	// File must be 0600.
	p, _ := config.Path()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	if got := filepath.Base(p); got != "credentials.json" {
		t.Fatalf("filename = %q", got)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := config.Load()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if got != (config.Credentials{}) {
		t.Fatalf("missing file gave %+v, want zero", got)
	}
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := config.Save(config.Credentials{BaseURL: "http://file", Token: "file-tok", Login: "octocat"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// File only.
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	r, err := config.Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.BaseURL != "http://file" || r.URLSource != "file" || r.Token != "file-tok" || r.TokSource != "file" {
		t.Fatalf("file precedence wrong: %+v", r)
	}

	// Env overrides file.
	t.Setenv(config.EnvURL, "http://env")
	t.Setenv(config.EnvToken, "env-tok")
	r, _ = config.Resolve("")
	if r.BaseURL != "http://env" || r.URLSource != "env" || r.Token != "env-tok" || r.TokSource != "env" {
		t.Fatalf("env precedence wrong: %+v", r)
	}

	// Flag overrides env for the URL.
	r, _ = config.Resolve("http://flag")
	if r.BaseURL != "http://flag" || r.URLSource != "flag" {
		t.Fatalf("flag precedence wrong: %+v", r)
	}
}

func TestDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_ = config.Save(config.Credentials{BaseURL: "u", Token: "t"})
	if err := config.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting again is a no-op.
	if err := config.Delete(); err != nil {
		t.Fatalf("delete again: %v", err)
	}
}
