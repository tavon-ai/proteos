// Package gitea is a minimal client for the Gitea REST API (/api/v1),
// covering exactly the surface ProteOS needs: PAT validation (GetUser),
// default-branch resolution (GetRepo), and PR creation (CreatePR). Forgejo is
// a Gitea fork with an identical /api/v1 for all of these, so one client
// serves both.
//
// Auth model (Gitea/Forgejo phase 2): API calls send the user's Personal
// Access Token as `Authorization: token <PAT>`. git-over-https instead uses
// HTTP Basic with the user's login as username and the PAT as password —
// which is why GetUser captures the login at PAT-save time.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors the HTTP layer maps to stable API error codes.
var (
	// ErrBadToken: the host rejected the PAT (401/403).
	ErrBadToken = errors.New("gitea: token rejected")
	// ErrNotFound: unknown repo — or a private repo the token cannot see
	// (Gitea answers 404 for both, like GitHub).
	ErrNotFound = errors.New("gitea: not found")
	// ErrPRAlreadyExists: a PR for this head/base pair is already open.
	ErrPRAlreadyExists = errors.New("gitea: pull request already exists")
	// ErrNoPRCommits: nothing to merge between base and head.
	ErrNoPRCommits = errors.New("gitea: no commits between base and head")
)

// Client talks to one host's /api/v1. Construct per host via New.
type Client struct {
	apiBase string
	httpc   *http.Client
}

// New returns a client for the given API base URL (no trailing slash needed).
// Production callers build it with APIBaseForHost; tests point it at a local
// fake.
func New(apiBaseURL string) *Client {
	return &Client{
		apiBase: strings.TrimRight(apiBaseURL, "/"),
		httpc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// APIBaseForHost builds the production API base for an allowlisted host
// (the lowercased host[:port] form): https://<host>/api/v1.
func APIBaseForHost(host string) string {
	return "https://" + host + "/api/v1"
}

// Repo is the subset of a Gitea repository ProteOS reads.
type Repo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// PR is the subset of a created pull request ProteOS returns to callers.
type PR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// GetUser validates the token and returns the account's login — the username
// git-over-https Basic auth needs. An empty login is treated as an error so a
// half-shaped response can never produce an unusable credential.
func (c *Client) GetUser(ctx context.Context, token string) (login string, err error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("gitea: user response missing login")
	}
	return u.Login, nil
}

// GetRepo fetches repo metadata (default branch). Works tokenless for public
// repos; pass the token when present so private repos resolve too.
func (c *Client) GetRepo(ctx context.Context, token, owner, repo string) (Repo, error) {
	var r Repo
	err := c.do(ctx, token, http.MethodGet, "/repos/"+owner+"/"+repo, nil, &r)
	return r, err
}

// CreatePR opens a pull request from head into base.
func (c *Client) CreatePR(ctx context.Context, token, owner, repo, head, base, title, body string) (PR, error) {
	payload := map[string]string{"head": head, "base": base, "title": title, "body": body}
	var pr PR
	err := c.do(ctx, token, http.MethodPost, "/repos/"+owner+"/"+repo+"/pulls", payload, &pr)
	return pr, err
}

// do issues one API call, mapping non-2xx statuses to the sentinel errors.
// The token is attached as `Authorization: token <t>` when non-empty.
func (c *Client) do(ctx context.Context, token, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("gitea: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	// Gitea error bodies are {"message": "...", "url": "..."}. The message is
	// needed to split 409's two meanings; it is never echoed to end users.
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&apiErr)

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrBadToken
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict, http.StatusUnprocessableEntity:
		// Gitea uses 409 both for "PR already exists" and "no commits between
		// branches" (and some versions 422 for validation): split on message.
		if strings.Contains(strings.ToLower(apiErr.Message), "exist") {
			return ErrPRAlreadyExists
		}
		return ErrNoPRCommits
	default:
		return fmt.Errorf("gitea: %s %s: http %d: %s", method, path, resp.StatusCode, apiErr.Message)
	}
}
