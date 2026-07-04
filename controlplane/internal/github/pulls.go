package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// PR-review outcomes the HTTP layer maps to distinct statuses.
var (
	// ErrPRNotFound: the PR (or repo) does not exist, or the token cannot see it.
	ErrPRNotFound = errors.New("github: pull request not found")
	// ErrPRNotMergeable: GitHub refused the merge (draft, conflicts, blocked by
	// branch protection) — surfaced as 405 on the merge call.
	ErrPRNotMergeable = errors.New("github: pull request not mergeable")
	// ErrPRHeadChanged: the head moved since the caller last looked (GitHub 409).
	ErrPRHeadChanged = errors.New("github: pull request head changed")
	// ErrPRMergeForbidden: the token lacks permission to merge (GitHub 403).
	ErrPRMergeForbidden = errors.New("github: merge forbidden")
)

// PullRequestDetail is the subset of GET /repos/{o}/{r}/pulls/{n} the review
// surface needs: identity, state, branch endpoints, author, and diff totals.
type PullRequestDetail struct {
	Number       int
	State        string // "open" | "closed" (raw GitHub state; Merged/Draft refine it)
	Merged       bool
	Draft        bool
	Title        string
	Body         string
	HTMLURL      string
	HeadRef      string
	HeadSHA      string
	BaseRef      string
	AuthorLogin  string
	AuthorAvatar string
	Additions    int
	Deletions    int
	ChangedFiles int
}

// prDetailWire mirrors GitHub's nested pull-request JSON.
type prDetailWire struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// GetPR fetches one pull request. A 404 (unknown repo/PR, or a token without
// access) surfaces as ErrPRNotFound.
func (c *Client) GetPR(ctx context.Context, accessToken, owner, repo string, number int) (*PullRequestDetail, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	status, body, err := c.do(ctx, accessToken, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("get pr: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, ErrPRNotFound
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get pr: status %d", status)
	}
	var w prDetailWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("decode pr: %w", err)
	}
	if w.Number == 0 {
		return nil, fmt.Errorf("get pr: missing number")
	}
	return &PullRequestDetail{
		Number: w.Number, State: w.State, Merged: w.Merged, Draft: w.Draft,
		Title: w.Title, Body: w.Body, HTMLURL: w.HTMLURL,
		HeadRef: w.Head.Ref, HeadSHA: w.Head.SHA, BaseRef: w.Base.Ref,
		AuthorLogin: w.User.Login, AuthorAvatar: w.User.AvatarURL,
		Additions: w.Additions, Deletions: w.Deletions, ChangedFiles: w.ChangedFiles,
	}, nil
}

// PRFile is one row of GET /pulls/{n}/files. Patch is empty for binary or
// oversized files (GitHub omits it). Status is GitHub's word (added, removed,
// modified, renamed, copied, changed, unchanged).
type PRFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

// ListPRFiles lists every changed file of a pull request, following pagination
// (GitHub caps the listing at 3000 files).
func (c *Client) ListPRFiles(ctx context.Context, accessToken, owner, repo string, number int) ([]PRFile, error) {
	var out []PRFile
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), number, repoPageSize, page)
		status, body, err := c.do(ctx, accessToken, http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("list pr files: %w", err)
		}
		if status == http.StatusNotFound {
			return nil, ErrPRNotFound
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("list pr files: status %d", status)
		}
		var files []PRFile
		if err := json.Unmarshal(body, &files); err != nil {
			return nil, fmt.Errorf("decode pr files: %w", err)
		}
		out = append(out, files...)
		if len(files) < repoPageSize {
			return out, nil
		}
	}
}

// CheckRun is one row of GET /commits/{ref}/check-runs. Conclusion is empty
// until Status is "completed".
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// checkRunsWire is GitHub's check-runs envelope.
type checkRunsWire struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// ListCheckRuns lists the check runs for a commit ref, following pagination.
func (c *Client) ListCheckRuns(ctx context.Context, accessToken, owner, repo, ref string) ([]CheckRun, error) {
	var out []CheckRun
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref), repoPageSize, page)
		status, body, err := c.do(ctx, accessToken, http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("list check runs: %w", err)
		}
		if status == http.StatusNotFound {
			return nil, ErrPRNotFound
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("list check runs: status %d", status)
		}
		var w checkRunsWire
		if err := json.Unmarshal(body, &w); err != nil {
			return nil, fmt.Errorf("decode check runs: %w", err)
		}
		out = append(out, w.CheckRuns...)
		if len(w.CheckRuns) < repoPageSize || len(out) >= w.TotalCount {
			return out, nil
		}
	}
}

// MergeResult is the 200 body of PUT /pulls/{n}/merge.
type MergeResult struct {
	SHA    string `json:"sha"`
	Merged bool   `json:"merged"`
}

// MergePR merges a pull request with the given method ("merge", "squash" or
// "rebase"). GitHub's refusals map to typed errors: 405 ErrPRNotMergeable,
// 409 ErrPRHeadChanged, 403 ErrPRMergeForbidden, 404 ErrPRNotFound.
func (c *Client) MergePR(ctx context.Context, accessToken, owner, repo string, number int, method string) (*MergeResult, error) {
	payload, _ := json.Marshal(map[string]string{"merge_method": method})
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", url.PathEscape(owner), url.PathEscape(repo), number)
	status, body, err := c.do(ctx, accessToken, http.MethodPut, path, payload)
	if err != nil {
		return nil, fmt.Errorf("merge pr: %w", err)
	}
	switch status {
	case http.StatusOK:
		var m MergeResult
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("decode merge: %w", err)
		}
		if !m.Merged {
			return nil, ErrPRNotMergeable
		}
		return &m, nil
	case http.StatusMethodNotAllowed:
		return nil, ErrPRNotMergeable
	case http.StatusConflict:
		return nil, ErrPRHeadChanged
	case http.StatusForbidden:
		return nil, ErrPRMergeForbidden
	case http.StatusNotFound:
		return nil, ErrPRNotFound
	default:
		return nil, fmt.Errorf("merge pr: status %d", status)
	}
}

// IssueComment is the subset of a created issue comment the caller needs.
type IssueComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// CreateIssueComment posts a comment on a pull request (PR comments live on
// the issues endpoint). A 404 surfaces as ErrPRNotFound.
func (c *Client) CreateIssueComment(ctx context.Context, accessToken, owner, repo string, number int, body string) (*IssueComment, error) {
	payload, _ := json.Marshal(map[string]string{"body": body})
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(repo), number)
	status, respBody, err := c.do(ctx, accessToken, http.MethodPost, path, payload)
	if err != nil {
		return nil, fmt.Errorf("comment pr: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, ErrPRNotFound
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("comment pr: status %d", status)
	}
	var cm IssueComment
	if err := json.Unmarshal(respBody, &cm); err != nil {
		return nil, fmt.Errorf("decode comment: %w", err)
	}
	if cm.ID == 0 {
		return nil, fmt.Errorf("comment pr: missing id")
	}
	return &cm, nil
}

// do performs an authenticated request and returns the status and body, leaving
// status interpretation to the caller (unlike getJSON, which folds non-200 into
// an opaque error).
func (c *Client) do(ctx context.Context, accessToken, method, path string, payload []byte) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, nil
}
