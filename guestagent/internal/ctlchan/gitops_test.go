package ctlchan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

// gitRunner returns a function that runs git in dir with an isolated config
// (throwaway HOME + a fixed identity), so the developer's global gitconfig
// (signing, hooks) cannot perturb the test.
func gitRunner(t *testing.T, dir string) func(args ...string) {
	t.Helper()
	gitHome := t.TempDir()
	return func(args ...string) {
		full := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"HOME="+gitHome, "XDG_CONFIG_HOME="+gitHome, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestGitStatus(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "https://github.com/tavon/alpha.git")
	run := gitRunner(t, repo)

	// Clean tree: branch reported, no files.
	st, err := m.gitStatus(context.Background(), repo)
	if err != nil {
		t.Fatalf("gitStatus clean: %v", err)
	}
	if st.Branch != "main" {
		t.Errorf("branch = %q, want main", st.Branch)
	}
	if len(st.Files) != 0 {
		t.Errorf("clean tree should have no files, got %+v", st.Files)
	}

	// Untracked file: both status codes are '?'.
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ = m.gitStatus(context.Background(), repo)
	got := byPath(st.Files)
	if f, ok := got["new.txt"]; !ok || f.Index != "?" || f.Worktree != "?" {
		t.Errorf("untracked new.txt = %+v, want index/worktree '?'", f)
	}

	// Unstaged modification: worktree 'M', index unchanged.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ = m.gitStatus(context.Background(), repo)
	got = byPath(st.Files)
	if f := got["README.md"]; f.Worktree != "M" || f.Index != " " {
		t.Errorf("unstaged README.md = %+v, want worktree 'M', index ' '", f)
	}

	// Stage it: index 'M', worktree clean.
	run("add", "README.md")
	st, _ = m.gitStatus(context.Background(), repo)
	got = byPath(st.Files)
	if f := got["README.md"]; f.Index != "M" || f.Worktree != " " {
		t.Errorf("staged README.md = %+v, want index 'M', worktree ' '", f)
	}
}

func TestGitStatus_Rename(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	run := gitRunner(t, repo)

	// git mv stages a rename README.md -> DOC.md.
	run("mv", "README.md", "DOC.md")

	st, err := m.gitStatus(context.Background(), repo)
	if err != nil {
		t.Fatalf("gitStatus rename: %v", err)
	}
	if len(st.Files) != 1 {
		t.Fatalf("want 1 changed file, got %+v", st.Files)
	}
	f := st.Files[0]
	if f.Index != "R" {
		t.Errorf("index = %q, want 'R' (rename)", f.Index)
	}
	// Path is the new name, Orig the original.
	if f.Path != "DOC.md" {
		t.Errorf("path = %q, want DOC.md (new name)", f.Path)
	}
	if f.Orig != "README.md" {
		t.Errorf("orig = %q, want README.md (original name)", f.Orig)
	}
}

func TestGitDiff(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	run := gitRunner(t, repo)

	// Unstaged change shows in the worktree diff, not the staged diff.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := m.gitDiff(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("gitDiff worktree: %v", err)
	}
	if !strings.Contains(d.Diff, "+changed") || !strings.Contains(d.Diff, "README.md") {
		t.Errorf("worktree diff missing change:\n%s", d.Diff)
	}
	if d.Truncated {
		t.Errorf("small diff should not be truncated")
	}
	// The staged diff is empty until the change is added.
	if d2, _ := m.gitDiff(context.Background(), repo, true); d2.Diff != "" {
		t.Errorf("staged diff should be empty before add, got:\n%s", d2.Diff)
	}

	// After staging, the change moves to the staged diff.
	run("add", "README.md")
	if d3, _ := m.gitDiff(context.Background(), repo, true); !strings.Contains(d3.Diff, "+changed") {
		t.Errorf("staged diff missing change after add:\n%s", d3.Diff)
	}
}

func TestGitDiff_Truncated(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	// Overwrite the tracked README with content larger than the diff cap, so the
	// unstaged diff exceeds maxDiffBytes.
	big := strings.Repeat("some line of changed content\n", (maxDiffBytes/29)+50000)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := m.gitDiff(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("gitDiff big: %v", err)
	}
	if !d.Truncated {
		t.Errorf("oversized diff should be truncated (len=%d)", len(d.Diff))
	}
	if len(d.Diff) > maxDiffBytes {
		t.Errorf("truncated diff len = %d, want <= %d", len(d.Diff), maxDiffBytes)
	}
}

func TestGitStatus_NotARepo(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	plain := filepath.Join(work, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.gitStatus(context.Background(), plain); err == nil {
		t.Errorf("gitStatus on a non-repo should error")
	}
}

func TestGitStatus_OutsideWorkspace(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	if _, err := m.gitStatus(context.Background(), "/etc"); err == nil {
		t.Errorf("gitStatus outside the workspace should error")
	}
}

func byPath(files []guestwire.GitFileStatus) map[string]guestwire.GitFileStatus {
	m := map[string]guestwire.GitFileStatus{}
	for _, f := range files {
		m[f.Path] = f
	}
	return m
}
