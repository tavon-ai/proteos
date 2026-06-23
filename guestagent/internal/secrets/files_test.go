package secrets

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

// sessionUser is a non-root identity rooted at a temp $HOME whose uid/gid are the
// test process's own — so Chown to it succeeds (chown-to-self is allowed without
// privilege) and ownership assertions are meaningful.
func sessionUser(t *testing.T) (runas.Identity, string) {
	t.Helper()
	home := t.TempDir()
	return runas.Identity{Name: "dev", UID: os.Getuid(), GID: os.Getgid(), Home: home}, home
}

func newFileStore(t *testing.T) (*Store, string) {
	t.Helper()
	id, home := sessionUser(t)
	s, err := New(t.TempDir(), id)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s, home
}

func putFiles(t *testing.T, s *Store, files ...guestwire.FileDef) {
	t.Helper()
	if err := s.Replace(guestwire.SecretsRequest{Files: files}); err != nil {
		t.Fatalf("replace: %v", err)
	}
}

// TestFileMaterializedWithModeAndOwner proves a file-kind item lands at its
// $HOME-relative path with the requested mode, the session user as owner, and the
// exact content.
func TestFileMaterializedWithModeAndOwner(t *testing.T) {
	s, home := newFileStore(t)
	putFiles(t, s, guestwire.FileDef{Path: ".gitconfig", Mode: 0o640, Content: "[user]\n\tname = Ada\n"})

	full := filepath.Join(home, ".gitconfig")
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
	b, _ := os.ReadFile(full)
	if string(b) != "[user]\n\tname = Ada\n" {
		t.Fatalf("content = %q", b)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Getuid() {
			t.Fatalf("uid = %d, want %d", st.Uid, os.Getuid())
		}
	}
}

// TestFileDefaultModeAndNestedDir proves a zero mode defaults to 0600 and a nested
// path creates its parent dir 0700 owned by the session user (the ~/.ssh shape).
func TestFileDefaultModeAndNestedDir(t *testing.T) {
	s, home := newFileStore(t)
	putFiles(t, s, guestwire.FileDef{Path: ".ssh/id_ed25519", Content: "PRIVATE-KEY-BODY"})

	full := filepath.Join(home, ".ssh", "id_ed25519")
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("default mode = %o, want 600", got)
	}
	dir, err := os.Stat(filepath.Join(home, ".ssh"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
}

// TestFileReplaceAllDrop proves a push that omits a previously-written file removes
// it (replace-all), while a still-present file is kept.
func TestFileReplaceAllDrop(t *testing.T) {
	s, home := newFileStore(t)
	putFiles(t, s,
		guestwire.FileDef{Path: ".gitconfig", Content: "a"},
		guestwire.FileDef{Path: ".tool/conf", Content: "b"},
	)
	// Re-push with only .gitconfig: .tool/conf must be removed.
	putFiles(t, s, guestwire.FileDef{Path: ".gitconfig", Content: "a"})

	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatalf(".gitconfig should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".tool", "conf")); !os.IsNotExist(err) {
		t.Fatalf(".tool/conf should be dropped, stat err = %v", err)
	}

	// And an empty push removes everything.
	putFiles(t, s)
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf(".gitconfig should be dropped on empty push, err = %v", err)
	}
}

// TestFileDropAcrossReboot proves the on-disk manifest lets a fresh Store (a guest
// reboot) still remove a file the user deleted while the machine was off.
func TestFileDropAcrossReboot(t *testing.T) {
	id, home := sessionUser(t)
	envA := t.TempDir()

	a, err := New(envA, id)
	if err != nil {
		t.Fatal(err)
	}
	putFiles(t, a, guestwire.FileDef{Path: ".gitconfig", Content: "x"})
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatalf("file missing after first push: %v", err)
	}

	// Reboot: a brand-new Store over the same $HOME (fresh tmpfs env dir). It must
	// load the manifest so a later push without the file removes it.
	b, err := New(t.TempDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	putFiles(t, b) // empty set
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("file should be dropped after reboot+empty push, err = %v", err)
	}
}

// TestFileRejectsEscapingPath proves the guest refuses paths that escape $HOME
// (absolute or via "..") and writes nothing.
func TestFileRejectsEscapingPath(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", ".ssh/../../escape", ""} {
		s, home := newFileStore(t)
		err := s.Replace(guestwire.SecretsRequest{Files: []guestwire.FileDef{{Path: bad, Content: "x"}}})
		if err == nil {
			t.Fatalf("path %q should be rejected", bad)
		}
		// Nothing should have been written outside (or inside) $HOME for it.
		if entries, _ := os.ReadDir(home); len(entries) != 0 {
			t.Fatalf("path %q wrote files: %v", bad, entries)
		}
	}
}

// TestFilesAndProvidersCoexist proves env-kind providers and file-kind items are
// installed by the same push without interfering.
func TestFilesAndProvidersCoexist(t *testing.T) {
	s, home := newFileStore(t)
	err := s.Replace(guestwire.SecretsRequest{
		Providers: map[string]guestwire.ProviderDef{
			"claude": {Command: "claude", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}},
		},
		Files: []guestwire.FileDef{{Path: ".gitconfig", Content: "g"}},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, ok := s.Get("claude"); !ok {
		t.Fatal("claude provider not installed")
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatalf(".gitconfig not written: %v", err)
	}
}
