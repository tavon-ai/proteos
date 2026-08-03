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

// stubSSHDir points sshDir at a fresh fixture directory for the duration of a
// test and restores it afterwards, returning the fixture path.
func stubSSHDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := sshDir
	sshDir = dir
	t.Cleanup(func() { sshDir = prev })
	return dir
}

// TestEnsureSSHHostKeysSymlinksFreshDir proves a fresh persist disk gets every
// host key filename symlinked from /etc/ssh into mount/ssh_host_keys (nothing
// generated yet — that's ssh-keygen -A's job, run by the sshd unit).
func TestEnsureSSHHostKeysSymlinksFreshDir(t *testing.T) {
	etcSSH := stubSSHDir(t)
	mount := t.TempDir()

	ensureSSHHostKeys(mount)

	for _, name := range sshHostKeyFiles {
		link := filepath.Join(etcSSH, name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("%s: expected a symlink: %v", name, err)
		}
		want := filepath.Join(mount, "ssh_host_keys", name)
		if target != want {
			t.Fatalf("%s: symlink target = %q, want %q", name, target, want)
		}
	}
}

// TestEnsureSSHHostKeysRescuesExistingFile proves a real key file already sitting
// at /etc/ssh/<name> (e.g. a boot that raced ahead of this step, or generated
// before persistence existed) is moved onto the persist disk — not discarded —
// and replaced with a symlink pointing at its new home, preserving its content.
func TestEnsureSSHHostKeysRescuesExistingFile(t *testing.T) {
	etcSSH := stubSSHDir(t)
	mount := t.TempDir()
	name := sshHostKeyFiles[0]
	path := filepath.Join(etcSSH, name)
	if err := os.WriteFile(path, []byte("existing-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}

	ensureSSHHostKeys(mount)

	persisted := filepath.Join(mount, "ssh_host_keys", name)
	content, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("key not rescued onto disk: %v", err)
	}
	if string(content) != "existing-key-material" {
		t.Fatalf("rescued content = %q, want the original", content)
	}
	target, err := os.Readlink(path)
	if err != nil || target != persisted {
		t.Fatalf("etc/ssh path not replaced with a symlink to the rescued file: target=%q err=%v", target, err)
	}
}

// TestEnsureSSHHostKeysIdempotent proves a second call (a later boot) leaves an
// already-generated key on disk untouched — the whole point being that
// ssh-keygen -A sees the symlink target already exists and skips regenerating
// it, so the identity is stable across this machine's reboots.
func TestEnsureSSHHostKeysIdempotent(t *testing.T) {
	etcSSH := stubSSHDir(t)
	mount := t.TempDir()
	ensureSSHHostKeys(mount)

	// Simulate ssh-keygen -A writing through the symlink on first boot.
	name := sshHostKeyFiles[0]
	persisted := filepath.Join(mount, "ssh_host_keys", name)
	if err := os.WriteFile(persisted, []byte("generated-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	ensureSSHHostKeys(mount) // second boot

	content, err := os.ReadFile(filepath.Join(etcSSH, name))
	if err != nil || string(content) != "generated-key" {
		t.Fatalf("key content changed across the idempotent call: content=%q err=%v", content, err)
	}
}
