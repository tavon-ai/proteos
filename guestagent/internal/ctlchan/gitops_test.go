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

func TestGitBranch(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	// Create without checkout: branch exists, current branch stays main.
	if _, cerr := m.gitBranch(context.Background(), repo, "feature/x", false, ""); cerr != nil {
		t.Fatalf("create branch: %+v", cerr)
	}
	st, _ := m.gitStatus(context.Background(), repo)
	if st.Branch != "main" {
		t.Errorf("after create-only, branch = %q, want main", st.Branch)
	}

	// Create + checkout: current branch switches.
	res, cerr := m.gitBranch(context.Background(), repo, "feature/y", true, "")
	if cerr != nil {
		t.Fatalf("create+checkout: %+v", cerr)
	}
	if res.Branch != "feature/y" {
		t.Errorf("response branch = %q, want feature/y", res.Branch)
	}
	st, _ = m.gitStatus(context.Background(), repo)
	if st.Branch != "feature/y" {
		t.Errorf("after checkout, branch = %q, want feature/y", st.Branch)
	}

	// Duplicate name is rejected with a typed error.
	if _, cerr := m.gitBranch(context.Background(), repo, "feature/x", false, ""); cerr == nil || cerr.Code != guestwire.ErrCodeBranchExists {
		t.Errorf("duplicate branch = %+v, want branch_exists", cerr)
	}

	// Invalid name is rejected before any git runs.
	if _, cerr := m.gitBranch(context.Background(), repo, "-nope", false, ""); cerr == nil || cerr.Code != guestwire.ErrCodeInvalidBranch {
		t.Errorf("invalid name = %+v, want invalid_branch", cerr)
	}
}

func TestGitBranch_From(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	run := gitRunner(t, repo)

	// Make a second commit on main, then branch from the first commit (HEAD~1).
	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "second.txt")
	run("commit", "-q", "-m", "second")

	res, cerr := m.gitBranch(context.Background(), repo, "from-first", true, "HEAD~1")
	if cerr != nil {
		t.Fatalf("branch from HEAD~1: %+v", cerr)
	}
	if res.Branch != "from-first" {
		t.Errorf("branch = %q, want from-first", res.Branch)
	}
	// The new branch tip is HEAD~1 (before "second"), so checkout removed the
	// committed second.txt from the working tree and the tree is clean.
	if _, err := os.Stat(filepath.Join(repo, "second.txt")); !os.IsNotExist(err) {
		t.Errorf("second.txt should be absent on from-first (branched before it), stat err=%v", err)
	}
	st, _ := m.gitStatus(context.Background(), repo)
	if len(st.Files) != 0 {
		t.Errorf("from-first tree should be clean, got %+v", st.Files)
	}
}

// newCommitManager builds a Manager with an isolated HOME holding a committer
// identity, so commits have an author (git.configure does this in production).
func newCommitManager(t *testing.T, work string) *Manager {
	t.Helper()
	home := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work, "HOME=" + home}, runas.Root(), nil)
	if err := m.writeGitConfig(guestwire.GitConfigurePayload{Name: "Tester", Email: "tester@example.com"}); err != nil {
		t.Fatalf("writeGitConfig: %v", err)
	}
	return m
}

func TestGitCommit(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	// Modify a tracked file and add a new one, then commit everything.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, cerr := m.gitCommit(context.Background(), repo, "my commit", nil)
	if cerr != nil {
		t.Fatalf("commit: %+v", cerr)
	}
	if res.Subject != "my commit" {
		t.Errorf("subject = %q, want 'my commit'", res.Subject)
	}
	if res.Sha == "" {
		t.Errorf("empty sha")
	}
	st, _ := m.gitStatus(context.Background(), repo)
	if len(st.Files) != 0 {
		t.Errorf("tree should be clean after full commit, got %+v", st.Files)
	}
}

func TestGitCommit_Partial(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Commit only a.txt; b.txt must remain untracked.
	if _, cerr := m.gitCommit(context.Background(), repo, "add a", []string{"a.txt"}); cerr != nil {
		t.Fatalf("partial commit: %+v", cerr)
	}
	st, _ := m.gitStatus(context.Background(), repo)
	got := byPath(st.Files)
	if _, ok := got["a.txt"]; ok {
		t.Errorf("a.txt should be committed, still in status")
	}
	if f, ok := got["b.txt"]; !ok || f.Worktree != "?" {
		t.Errorf("b.txt should remain untracked, got %+v (ok=%v)", f, ok)
	}
}

func TestGitCommit_EmptyMessage(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	if err := os.WriteFile(filepath.Join(repo, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cerr := m.gitCommit(context.Background(), repo, "   ", nil); cerr == nil || cerr.Code != guestwire.ErrCodeEmptyMessage {
		t.Errorf("empty message = %+v, want empty_message", cerr)
	}
}

func TestGitCommit_NothingToCommit(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	// Clean tree: nothing staged ⇒ nothing_to_commit.
	if _, cerr := m.gitCommit(context.Background(), repo, "noop", nil); cerr == nil || cerr.Code != guestwire.ErrCodeNothingToCommit {
		t.Errorf("clean-tree commit = %+v, want nothing_to_commit", cerr)
	}
}

// initBareRemote creates a bare repo to serve as a local push target (a file
// path remote needs no credential helper).
func initBareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", "-q", dir)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	return dir
}

func remoteHasBranch(remote, branch string) bool {
	cmd := exec.Command("git", "--git-dir="+remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func TestPushBranch(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	remote := initBareRemote(t)
	gitRunner(t, repo)("remote", "add", "origin", remote)

	// First push with -u: the bare remote gains the branch.
	out, err := m.pushBranch(context.Background(), repo, "main", true)
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !remoteHasBranch(remote, "main") {
		t.Errorf("remote should have main after push")
	}

	// A second push (no -u) of new work still lands.
	if err := os.WriteFile(filepath.Join(repo, "two.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cerr := m.gitCommit(context.Background(), repo, "second", nil); cerr != nil {
		t.Fatalf("commit: %+v", cerr)
	}
	if out, err := m.pushBranch(context.Background(), repo, "main", false); err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
}

func TestPushBranch_Failure(t *testing.T) {
	work := t.TempDir()
	m := newCommitManager(t, work)
	repo := filepath.Join(work, "alpha")
	gitInit(t, repo, "")
	gitRunner(t, repo)("remote", "add", "origin", initBareRemote(t))

	// Pushing a branch that does not exist locally fails (no such ref).
	if _, err := m.pushBranch(context.Background(), repo, "ghost", false); err == nil {
		t.Errorf("pushing a nonexistent branch should fail")
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
