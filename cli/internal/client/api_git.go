package client

import (
	"context"
	"net/url"
)

// GitFileStatus is one changed path in a git status. Index is the staged
// (index-vs-HEAD) code and Worktree the unstaged (worktree-vs-index) code; each
// is a porcelain-v1 character (M, A, D, R, ?, space). Orig is set on renames.
type GitFileStatus struct {
	Path     string `json:"path"`
	Orig     string `json:"orig,omitempty"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}

// GitStatus is the GET .../git/status response: the current branch and the
// working-tree change set (empty on a clean tree).
type GitStatus struct {
	Branch string          `json:"branch,omitempty"`
	Files  []GitFileStatus `json:"files"`
}

// GitDiff is the GET .../git/diff response. Truncated reports the diff exceeded
// the server's byte budget.
type GitDiff struct {
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

// GitStatusOf returns a project's working-tree change set.
func (c *Client) GitStatusOf(ctx context.Context, machineID, project string) (GitStatus, error) {
	var st GitStatus
	path := "/api/machines/" + machineID + "/git/status?project=" + url.QueryEscape(project)
	err := c.Do(ctx, "GET", path, nil, &st)
	return st, err
}

// GitDiffOf returns a project's unified diff. staged selects the index diff over
// the worktree diff.
func (c *Client) GitDiffOf(ctx context.Context, machineID, project string, staged bool) (GitDiff, error) {
	var d GitDiff
	path := "/api/machines/" + machineID + "/git/diff?project=" + url.QueryEscape(project)
	if staged {
		path += "&staged=true"
	}
	err := c.Do(ctx, "GET", path, nil, &d)
	return d, err
}

// GitBranchRequest is the POST .../git/branch body.
type GitBranchRequest struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Checkout bool   `json:"checkout"`
	From     string `json:"from,omitempty"`
}

type gitBranchResponse struct {
	Branch string `json:"branch"`
}

// GitBranch creates (and optionally checks out) a branch in a project, returning
// the current branch after the operation.
func (c *Client) GitBranch(ctx context.Context, machineID string, req GitBranchRequest) (string, error) {
	var r gitBranchResponse
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/git/branch", req, &r)
	return r.Branch, err
}

// GitCommitRequest is the POST .../git/commit body. Empty Paths commits all
// changes; otherwise the commit is limited to the given repo-relative paths.
type GitCommitRequest struct {
	Project string   `json:"project"`
	Message string   `json:"message"`
	Paths   []string `json:"paths,omitempty"`
}

// GitCommit is the POST .../git/commit response: the new HEAD short sha and its
// subject line.
type GitCommit struct {
	Sha     string `json:"sha"`
	Subject string `json:"subject"`
}

// GitCommit stages and commits a project's changes (the explicit review gate;
// the agent never commits on its own).
func (c *Client) GitCommit(ctx context.Context, machineID string, req GitCommitRequest) (GitCommit, error) {
	var r GitCommit
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/git/commit", req, &r)
	return r, err
}

// GitPushRequest is the POST .../git/push body.
type GitPushRequest struct {
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	SetUpstream bool   `json:"set_upstream"`
}

type gitPushResponse struct {
	OpID string `json:"op_id"`
}

// GitPush dispatches an asynchronous push of a branch to origin, returning the op
// id that correlates the later completion event.
func (c *Client) GitPush(ctx context.Context, machineID string, req GitPushRequest) (string, error) {
	var r gitPushResponse
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/git/push", req, &r)
	return r.OpID, err
}

// GitPRRequest is the POST .../git/pr body. Base defaults to the repo's default
// branch when empty.
type GitPRRequest struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
	Head    string `json:"head"`
	Base    string `json:"base,omitempty"`
}

// GitPR is the POST .../git/pr response: the opened PR's URL and number.
type GitPR struct {
	PRURL  string `json:"pr_url"`
	Number int    `json:"number"`
}

// GitPR opens a pull request from Head into Base.
func (c *Client) GitPR(ctx context.Context, machineID string, req GitPRRequest) (GitPR, error) {
	var r GitPR
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/git/pr", req, &r)
	return r, err
}
