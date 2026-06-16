package ctlchan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// projectsScanTimeout bounds a full workspace scan so a wedged git invocation in
// one repo cannot stall the control channel indefinitely.
const projectsScanTimeout = 15 * time.Second

// listProjects scans the workspace for git repositories and returns one Project
// per repo with its remote/branch/dirty/last-commit metadata. The filesystem is
// the source of truth (decision #4): a repo deleted from a terminal disappears
// here, a non-git directory is excluded. git runs as the unprivileged owner (the
// user that owns the checked-out trees), matching the clone path.
func (m *Manager) listProjects(ctx context.Context) ([]guestwire.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, projectsScanTimeout)
	defer cancel()

	entries, err := os.ReadDir(m.workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []guestwire.Project{}, nil // no workspace yet ⇒ no projects
		}
		return nil, err
	}

	projects := make([]guestwire.Project, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.workDir, e.Name())
		if !isGitRepo(path) {
			continue // a plain directory under /workspace is not a project
		}
		projects = append(projects, m.describeRepo(ctx, e.Name(), path))
	}
	return projects, nil
}

// isGitRepo reports whether path contains a .git entry (dir or, for worktrees,
// a file). It is a cheap stat that gates the more expensive git invocations.
func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// describeRepo reads a repo's metadata via a handful of read-only git commands.
// Any individual command failing (e.g. an empty repo with no commits) leaves its
// field zero rather than failing the whole scan.
func (m *Manager) describeRepo(ctx context.Context, name, path string) guestwire.Project {
	p := guestwire.Project{Name: name, Path: path}
	p.Remote = m.git(ctx, path, "remote", "get-url", "origin")

	branch := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "HEAD" { // detached: report the short sha instead
		branch = m.git(ctx, path, "rev-parse", "--short", "HEAD")
	}
	p.Branch = branch

	// --porcelain prints one line per change; any output ⇒ dirty.
	p.Dirty = m.git(ctx, path, "status", "--porcelain") != ""

	// HEAD commit: committer date (ISO-8601/RFC3339-compatible) then subject, as
	// two lines so a subject containing the separator cannot confuse the parse.
	if out := m.git(ctx, path, "log", "-1", "--format=%cI%n%s"); out != "" {
		if at, msg, ok := strings.Cut(out, "\n"); ok {
			p.LastCommitAt = strings.TrimSpace(at)
			p.LastCommitMsg = strings.TrimSpace(msg)
		}
	}
	return p
}

// git runs one read-only git command in repo dir and returns its trimmed stdout,
// or "" on any error. It runs as the unprivileged owner so it can read trees that
// user owns (the agent is root) without tripping git's dubious-ownership guard.
func (m *Manager) git(ctx context.Context, dir string, args ...string) string {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0")
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
