package persist

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

func setupDir(t *testing.T) *Persist {
	t.Helper()
	p, err := Setup(Config{Dir: t.TempDir(), Version: "test-1"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestDirModeInit proves dir mode initializes the DB, records a cold boot, and
// exposes home/workspace via the shell env.
func TestDirModeInit(t *testing.T) {
	p := setupDir(t)

	if p.Mode() != guestwire.PersistDir {
		t.Fatalf("mode=%q, want dir", p.Mode())
	}
	env := p.ShellEnv()
	if !slices.ContainsFunc(env, func(s string) bool { return len(s) > 5 && s[:5] == "HOME=" }) {
		t.Fatalf("shell env missing HOME: %v", env)
	}
	// The home dir is created on disk.
	if _, err := os.Stat(p.home); err != nil {
		t.Fatalf("home dir not created: %v", err)
	}

	info := p.Info()
	if info.Version != "test-1" {
		t.Fatalf("version=%q", info.Version)
	}
	if info.LastBoot == nil || info.LastBoot.Kind != guestwire.BootCold {
		t.Fatalf("last boot=%+v, want cold", info.LastBoot)
	}
}

// TestBootRows proves every boot/resume appends a row and Info reports the latest.
func TestBootRows(t *testing.T) {
	p := setupDir(t)

	// A resume records a "resumed" row (clock/entropy are no-ops off Linux).
	if _, err := p.Resume(0, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	info := p.Info()
	if info.LastBoot == nil || info.LastBoot.Kind != guestwire.BootResumed {
		t.Fatalf("after resume, last boot=%+v, want resumed", info.LastBoot)
	}

	var n int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM boots`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 { // cold (setup) + resumed
		t.Fatalf("boots count=%d, want 2", n)
	}
}

// TestKVRoundTrip proves the kv table survives a reopen of the same dir.
func TestKVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, err := Setup(Config{Dir: dir, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Set("layout", `{"x":1}`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, ok := p.Get("missing"); ok {
		t.Fatalf("missing key reported present")
	}
	_ = p.Close()

	// Reopen the same dir: the value persists in machine.db.
	p2, err := Setup(Config{Dir: dir, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p2.Close() })
	if v, ok := p2.Get("layout"); !ok || v != `{"x":1}` {
		t.Fatalf("kv reopen: got %q ok=%v", v, ok)
	}
	// The DB file is on the persist root.
	if _, err := os.Stat(filepath.Join(dir, "machine.db")); err != nil {
		t.Fatalf("machine.db missing: %v", err)
	}
}

// TestDegradedMode proves disk mode with no device degrades to ephemeral rather
// than failing — Setup returns a usable (no-DB) handle.
func TestDegradedMode(t *testing.T) {
	// No Dir + a device that does not exist ⇒ mount fails ⇒ degraded. A short
	// wait keeps the Linux device-wait from blocking 10s.
	p, err := Setup(Config{
		Device:      "/dev/proteos-does-not-exist",
		MountPoint:  filepath.Join(t.TempDir(), "mnt"),
		Version:     "v",
		WaitTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("degraded setup should not error: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if p.Mode() != guestwire.PersistNone {
		t.Fatalf("mode=%q, want none", p.Mode())
	}
	if p.ShellEnv() != nil {
		t.Fatalf("degraded mode should expose no shell env, got %v", p.ShellEnv())
	}
	// Info and kv are safe no-ops in degraded mode.
	if info := p.Info(); info.LastBoot != nil {
		t.Fatalf("degraded mode should have no boot row")
	}
	if err := p.Set("k", "v"); err != nil {
		t.Fatalf("degraded Set should be a no-op, got %v", err)
	}
	if _, err := p.Resume(0, nil); err != nil {
		t.Fatalf("degraded Resume should not error: %v", err)
	}
}
