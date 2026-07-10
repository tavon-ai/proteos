package client

import (
	"context"
	"net/url"
	"strconv"
)

// PRAuthor is a pull request's author, as returned within PRDetail.
type PRAuthor struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// PRDetail is the GET .../pulls/{number} response: a pull request's summary.
// State is one of open, draft, merged, closed.
type PRDetail struct {
	Number       int      `json:"number"`
	State        string   `json:"state"`
	Title        string   `json:"title"`
	Body         string   `json:"body,omitempty"`
	HTMLURL      string   `json:"html_url"`
	Head         string   `json:"head"`
	Base         string   `json:"base"`
	HeadSHA      string   `json:"head_sha"`
	Author       PRAuthor `json:"author"`
	Additions    int      `json:"additions"`
	Deletions    int      `json:"deletions"`
	ChangedFiles int      `json:"changed_files"`
}

// PRFile is one changed file in a pull request. Status is a single letter: A
// added, M modified, D deleted, R renamed. PrevPath is set on renames; Patch
// may be empty for binary or oversized files.
type PRFile struct {
	Path      string `json:"path"`
	PrevPath  string `json:"prev_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
}

// PRFiles is the GET .../pulls/{number}/files response.
type PRFiles struct {
	Files []PRFile `json:"files"`
}

// PRCheckRun is one check run on a pull request's head commit.
type PRCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// PRChecks is the GET .../pulls/{number}/checks response: the head commit's
// check runs plus the counts by outcome.
type PRChecks struct {
	Total   int          `json:"total"`
	Passed  int          `json:"passed"`
	Failed  int          `json:"failed"`
	Pending int          `json:"pending"`
	Runs    []PRCheckRun `json:"runs"`
}

// PRMerge is the POST .../pulls/{number}/merge response.
type PRMerge struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
}

// PRComment is the POST .../pulls/{number}/comments response.
type PRComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// prPath builds a /api/git/repos/{owner}/{repo}/pulls/{number}<suffix> path.
func prPath(owner, repo string, number int, suffix string) string {
	return "/api/git/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" + strconv.Itoa(number) + suffix
}

// GetPR returns one pull request's summary.
func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (PRDetail, error) {
	var pr PRDetail
	err := c.Do(ctx, "GET", prPath(owner, repo, number, ""), nil, &pr)
	return pr, err
}

// ListPRFiles returns a pull request's changed files.
func (c *Client) ListPRFiles(ctx context.Context, owner, repo string, number int) (PRFiles, error) {
	var r PRFiles
	err := c.Do(ctx, "GET", prPath(owner, repo, number, "/files"), nil, &r)
	return r, err
}

// ListPRChecks returns a pull request's check-run summary.
func (c *Client) ListPRChecks(ctx context.Context, owner, repo string, number int) (PRChecks, error) {
	var r PRChecks
	err := c.Do(ctx, "GET", prPath(owner, repo, number, "/checks"), nil, &r)
	return r, err
}

// prMergeRequest is the POST .../pulls/{number}/merge body.
type prMergeRequest struct {
	Method string `json:"method"`
}

// MergePR merges a pull request using method (merge, squash, or rebase; empty
// defaults to merge).
func (c *Client) MergePR(ctx context.Context, owner, repo string, number int, method string) (PRMerge, error) {
	var r PRMerge
	err := c.Do(ctx, "POST", prPath(owner, repo, number, "/merge"), prMergeRequest{Method: method}, &r)
	return r, err
}

// prCommentRequest is the POST .../pulls/{number}/comments body.
type prCommentRequest struct {
	Body string `json:"body"`
}

// CommentPR posts a plain comment on a pull request.
func (c *Client) CommentPR(ctx context.Context, owner, repo string, number int, body string) (PRComment, error) {
	var r PRComment
	err := c.Do(ctx, "POST", prPath(owner, repo, number, "/comments"), prCommentRequest{Body: body}, &r)
	return r, err
}
