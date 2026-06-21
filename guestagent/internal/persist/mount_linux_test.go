//go:build linux

package persist

import (
	"os"
	"path/filepath"
	"testing"
)

// stubClaudeBinary points claudeBinaryPath at a fixture file for the duration of a
// test and restores it afterwards, returning the fixture path.
func stubClaudeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	prev := claudeBinaryPath
	claudeBinaryPath = bin
	t.Cleanup(func() { claudeBinaryPath = prev })
	return bin
}

// TestEnsureClaudeLauncherCreatesSymlink proves a fresh home gets
// ~/.local/bin/claude pointing at the baked binary.
func TestEnsureClaudeLauncherCreatesSymlink(t *testing.T) {
	bin := stubClaudeBinary(t)
	home := t.TempDir()

	ensureClaudeLauncher(home, 0, 0) // uid 0 ⇒ skip chown (tests aren't root)

	link := filepath.Join(home, ".local", "bin", "claude")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected launcher symlink at %s: %v", link, err)
	}
	if target != bin {
		t.Fatalf("launcher points at %q, want %q", target, bin)
	}
}

// TestEnsureClaudeLauncherRepairsStale proves a wrong/stale entry at the launcher
// path is replaced — covering homes that predate the fix or a moved binary.
func TestEnsureClaudeLauncherRepairsStale(t *testing.T) {
	bin := stubClaudeBinary(t)
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "claude")

	// A stale symlink to an old location, then a plain file squatting the path.
	for _, seed := range []func(){
		func() { _ = os.Symlink("/old/claude", link) },
		func() { _ = os.WriteFile(link, []byte("broken"), 0o755) },
	} {
		_ = os.Remove(link)
		seed()
		ensureClaudeLauncher(home, 0, 0)
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("launcher not repaired to a symlink: %v", err)
		}
		if target != bin {
			t.Fatalf("launcher points at %q, want %q", target, bin)
		}
	}
}

// TestEnsureClaudeLauncherIdempotent proves a second call leaves an already-correct
// launcher untouched (and still present).
func TestEnsureClaudeLauncherIdempotent(t *testing.T) {
	bin := stubClaudeBinary(t)
	home := t.TempDir()

	ensureClaudeLauncher(home, 0, 0)
	ensureClaudeLauncher(home, 0, 0)

	link := filepath.Join(home, ".local", "bin", "claude")
	if target, err := os.Readlink(link); err != nil || target != bin {
		t.Fatalf("launcher missing or wrong after second call: target=%q err=%v", target, err)
	}
}

// TestEnsureClaudeLauncherNoBinary proves images without Claude baked in get no
// launcher (and no error/panic) — the binary-absent fast path.
func TestEnsureClaudeLauncherNoBinary(t *testing.T) {
	prev := claudeBinaryPath
	claudeBinaryPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { claudeBinaryPath = prev })
	home := t.TempDir()

	ensureClaudeLauncher(home, 0, 0)

	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "claude")); !os.IsNotExist(err) {
		t.Fatalf("expected no launcher when binary absent, got err=%v", err)
	}
}
