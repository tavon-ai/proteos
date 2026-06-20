package ctlchan

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// GR1 worktree-review surface: read a repo's status and unified diff over the
// control channel. Both run read-only git as the unprivileged owner (the user
// that owns the checked-out trees), exactly like the projects scan. The CP has
// already matched the requested project to a listable path; the guest re-checks
// the path is inside the workspace and is a git repo (defence in depth).

const (
	statusTimeout = 15 * time.Second
	diffTimeout   = 30 * time.Second
	branchTimeout = 30 * time.Second
	// maxDiffBytes caps a single git.diff payload so an enormous (e.g. generated
	// or binary-churn) diff cannot blow the control-channel frame or the browser.
	// Hitting the cap sets Truncated; the UI then tells the user to inspect the
	// rest in the terminal/editor.
	maxDiffBytes = 1 << 20 // 1 MiB
)

// errNotRepo is returned when a resolved path is not (or no longer) a git repo.
var errNotRepo = fmt.Errorf("not a git repository")

// gitStatus returns the working-tree change set for repoPath: the current branch
// plus one entry per changed path, parsed from porcelain v1 -z output.
func (m *Manager) gitStatus(ctx context.Context, repoPath string) (guestwire.GitStatusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	resp := guestwire.GitStatusResponse{Files: []guestwire.GitFileStatus{}}
	if err := m.validateRepoPath(repoPath); err != nil {
		return resp, err
	}

	branch := m.git(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "HEAD" { // detached: report the short sha instead
		branch = m.git(ctx, repoPath, "rev-parse", "--short", "HEAD")
	}
	resp.Branch = branch

	// -z is NUL-delimited and never quotes paths, so spaces/renames parse without
	// ambiguity; core.quotePath=false keeps non-ASCII paths literal too.
	out, err := m.gitBytes(ctx, repoPath, "-c", "core.quotePath=false", "status", "--porcelain=v1", "-z")
	if err != nil {
		return resp, err
	}
	resp.Files = parsePorcelainZ(out)
	return resp, nil
}

// gitDiff returns a unified diff of repoPath, capped at maxDiffBytes. Staged
// selects the index diff (git diff --cached) over the worktree diff. Tracked
// changes only — untracked files surface in gitStatus.
func (m *Manager) gitDiff(ctx context.Context, repoPath string, staged bool) (guestwire.GitDiffResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	var resp guestwire.GitDiffResponse
	if err := m.validateRepoPath(repoPath); err != nil {
		return resp, err
	}

	args := []string{"-C", repoPath, "-c", "core.quotePath=false", "diff"}
	if staged {
		args = append(args, "--cached")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0")
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return resp, err
	}
	if err := cmd.Start(); err != nil {
		return resp, err
	}

	// Read one byte past the cap so we can tell "exactly at cap" from "over cap".
	data, _ := io.ReadAll(io.LimitReader(stdout, maxDiffBytes+1))
	if len(data) > maxDiffBytes {
		resp.Truncated = true
		data = data[:maxDiffBytes]
		cancel() // stop git rather than drain a huge remaining diff
	}
	_ = cmd.Wait()

	// A mid-rune cut at the cap would be invalid UTF-8; scrub it so the JSON
	// payload stays clean.
	resp.Diff = strings.ToValidUTF8(string(data), "")
	return resp, nil
}

// gitBranch creates (and optionally checks out) a branch in repoPath (GR2). It
// returns a typed control error so the CP can map duplicates / bad names to
// distinct HTTP statuses. Runs as the unprivileged owner.
func (m *Manager) gitBranch(ctx context.Context, repoPath, name string, checkout bool, from string) (guestwire.GitBranchResponse, *guestwire.ControlErrorPayload) {
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()

	var resp guestwire.GitBranchResponse
	if err := m.validateRepoPath(repoPath); err != nil {
		return resp, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeGitFailed, Message: "invalid repo"}
	}
	if !guestwire.ValidBranchName(name) {
		return resp, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeInvalidBranch}
	}
	if !guestwire.ValidStartPoint(from) {
		return resp, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeGitFailed, Message: "bad start point"}
	}
	if m.branchExists(ctx, repoPath, name) {
		return resp, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeBranchExists}
	}

	// checkout -b creates and switches; branch creates without switching. Both
	// fail if the branch already exists (we pre-checked) or the start point is
	// unknown. The validated name/from cannot be read as options.
	var args []string
	if checkout {
		args = []string{"checkout", "-b", name}
	} else {
		args = []string{"branch", name}
	}
	if from != "" {
		args = append(args, from)
	}
	if out, err := m.runGit(ctx, repoPath, args...); err != nil {
		return resp, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeGitFailed, Message: sanitizeGitErr(out, err)}
	}

	resp.Branch = m.git(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	return resp, nil
}

// branchExists reports whether refs/heads/<name> already exists.
func (m *Manager) branchExists(ctx context.Context, repoPath, name string) bool {
	_, err := m.runGit(ctx, repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// runGit runs a (possibly mutating) git command and returns its combined output
// and error, so callers can surface a sanitized failure detail. Runs as owner.
func (m *Manager) runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0")
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	return cmd.CombinedOutput()
}

// sanitizeGitErr returns a short, non-sensitive failure detail (last line, size
// capped). git ops here carry no token in their output, but we cap anyway.
func sanitizeGitErr(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return err.Error()
	}
	lines := strings.Split(s, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 200 {
		last = last[:200]
	}
	return last
}

// validateRepoPath re-checks (defence in depth) that path is inside the
// workspace and is a git repo before any git runs against it.
func (m *Manager) validateRepoPath(path string) error {
	if err := m.validateDest(path); err != nil {
		return err
	}
	if !isGitRepo(path) {
		return errNotRepo
	}
	return nil
}

// gitBytes runs a git command in repo dir and returns its raw stdout (untrimmed,
// so NUL-delimited output survives). It runs as the unprivileged owner.
func (m *Manager) gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0")
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	return cmd.Output()
}

// parsePorcelainZ parses `git status --porcelain=v1 -z` output into file
// entries. Records are NUL-terminated; a rename/copy record is followed by a
// second NUL-terminated token carrying the source path.
func parsePorcelainZ(out []byte) []guestwire.GitFileStatus {
	files := []guestwire.GitFileStatus{}
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 { // need "XY" + space + at least one path char
			continue
		}
		f := guestwire.GitFileStatus{
			Index:    string(tok[0]),
			Worktree: string(tok[1]),
			Path:     tok[3:],
		}
		if isRenameCopy(tok[0]) || isRenameCopy(tok[1]) {
			// The source path is the next NUL-terminated token.
			if i+1 < len(tokens) {
				f.Orig = tokens[i+1]
				i++
			}
		}
		files = append(files, f)
	}
	return files
}

func isRenameCopy(c byte) bool { return c == 'R' || c == 'C' }
