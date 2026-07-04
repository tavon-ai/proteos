// Package github is a minimal client for the GitHub App user-authorization
// (OAuth) flow: build the authorize URL, exchange a code for user tokens, and
// fetch the authenticated user's profile. Endpoint URLs are injectable so tests
// can drive the flow against an httptest fake of GitHub.
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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default GitHub endpoints.
const (
	DefaultAuthorizeURL = "https://github.com/login/oauth/authorize"
	DefaultTokenURL     = "https://github.com/login/oauth/access_token"
	DefaultAPIBaseURL   = "https://api.github.com"
)

// Config configures the client. URL fields default to the real GitHub when empty.
type Config struct {
	ClientID     string
	ClientSecret string
	AuthorizeURL string
	TokenURL     string
	APIBaseURL   string
	HTTPClient   *http.Client
}

// Client talks to GitHub's OAuth and REST endpoints.
type Client struct {
	clientID     string
	clientSecret string
	authorizeURL string
	tokenURL     string
	apiBaseURL   string
	http         *http.Client
}

// NewClient builds a client from cfg, filling defaults.
func NewClient(cfg Config) *Client {
	c := &Client{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		authorizeURL: orDefault(cfg.AuthorizeURL, DefaultAuthorizeURL),
		tokenURL:     orDefault(cfg.TokenURL, DefaultTokenURL),
		apiBaseURL:   strings.TrimRight(orDefault(cfg.APIBaseURL, DefaultAPIBaseURL), "/"),
		http:         cfg.HTTPClient,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	return c
}

// AuthorizeURL builds the URL to redirect the user to, carrying the opaque
// state and the callback redirect URI.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return c.authorizeURL + "?" + q.Encode()
}

// Token is the token-exchange response from GitHub.
type Token struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
}

// tokenError mirrors GitHub's error response on a failed exchange.
type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange swaps an authorization code for user access/refresh tokens.
func (c *Client) Exchange(ctx context.Context, code, redirectURI string) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: status %d", resp.StatusCode)
	}
	// GitHub returns 200 with an error body on bad codes.
	var terr tokenError
	if err := json.Unmarshal(body, &terr); err == nil && terr.Error != "" {
		return nil, fmt.Errorf("token exchange: %s", terr.Error)
	}
	var tok Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: empty access token")
	}
	return &tok, nil
}

// ErrBadRefreshToken is returned by Refresh when GitHub rejects the refresh
// token (revoked grant, already-rotated token, or expired refresh token). The
// TokenSource maps it to a revoked grant — the user must re-run the login flow.
var ErrBadRefreshToken = errors.New("github: bad refresh token")

// Refresh exchanges a refresh token for a fresh access/refresh token pair. GitHub
// rotates the refresh token on every use, so the caller MUST persist the returned
// pair (both tokens) before relying on it. A rejected refresh token surfaces as
// ErrBadRefreshToken.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh: status %d", resp.StatusCode)
	}
	// GitHub returns 200 with an error body on a bad/expired refresh token.
	var terr tokenError
	if err := json.Unmarshal(body, &terr); err == nil && terr.Error != "" {
		if terr.Error == "bad_refresh_token" || terr.Error == "unauthorized" {
			return nil, ErrBadRefreshToken
		}
		return nil, fmt.Errorf("token refresh: %s", terr.Error)
	}
	var tok Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token refresh: empty access token")
	}
	return &tok, nil
}

// Repo is the subset of a GitHub repository the dashboard needs.
type Repo struct {
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	PushedAt      time.Time `json:"pushed_at"`
}

// installationsResponse is GET /user/installations.
type installationsResponse struct {
	Installations []struct {
		ID int64 `json:"id"`
	} `json:"installations"`
}

// reposResponse is GET /user/installations/{id}/repositories.
type reposResponse struct {
	Repositories []Repo `json:"repositories"`
}

// repoPageSize is the GitHub max page size.
const repoPageSize = 100

// ListUserRepos lists every repository the user can access through the App's
// installations, using the user-to-server access token (Phase 7 decision #7).
// With a GitHub App the user chooses which repos the App may see, so this is the
// authoritative "what can I clone" set. Results are de-duplicated (a repo can
// appear under multiple installations) and sorted by pushed_at, newest first.
func (c *Client) ListUserRepos(ctx context.Context, accessToken string) ([]Repo, error) {
	var insts installationsResponse
	if err := c.getJSON(ctx, accessToken, "/user/installations?per_page="+itoa(repoPageSize), &insts); err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}

	seen := map[string]bool{}
	var out []Repo
	for _, inst := range insts.Installations {
		for page := 1; ; page++ {
			var rr reposResponse
			path := fmt.Sprintf("/user/installations/%d/repositories?per_page=%d&page=%d", inst.ID, repoPageSize, page)
			if err := c.getJSON(ctx, accessToken, path, &rr); err != nil {
				return nil, fmt.Errorf("list installation %d repos: %w", inst.ID, err)
			}
			for _, r := range rr.Repositories {
				if seen[r.FullName] {
					continue
				}
				seen[r.FullName] = true
				out = append(out, r)
			}
			if len(rr.Repositories) < repoPageSize {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PushedAt.After(out[j].PushedAt) })
	return out, nil
}

// getJSON performs an authenticated GET and decodes a JSON body into v.
// Transient errors (network failures, 5xx responses) are retried up to 3 times
// with linear backoff so intermittent GitHub API hiccups don't surface as errors.
func (c *Client) getJSON(ctx context.Context, accessToken, path string, v any) error {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		return json.Unmarshal(body, v)
	}
	return lastErr
}

func itoa(n int) string { return strconv.Itoa(n) }

// User is the subset of the GitHub user profile we persist.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// GetUser fetches the authenticated user's profile using the access token.
func (c *Client) GetUser(ctx context.Context, accessToken string) (*User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+"/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get user: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var u User
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}
	if u.ID == 0 {
		return nil, fmt.Errorf("get user: missing id")
	}
	return &u, nil
}

// GetRepo fetches a single repository (used to resolve the default branch when a
// PR base is not specified).
func (c *Client) GetRepo(ctx context.Context, accessToken, owner, repo string) (*Repo, error) {
	var r Repo
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := c.getJSON(ctx, accessToken, path, &r); err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return &r, nil
}

// PullRequest is the subset of a created pull request the caller needs.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// PR-creation outcomes the HTTP layer maps to distinct statuses.
var (
	// ErrNoPRCommits: head has no commits beyond base — nothing to open a PR for.
	ErrNoPRCommits = errors.New("github: no commits between base and head")
	// ErrPRAlreadyExists: a PR for this head→base already exists.
	ErrPRAlreadyExists = errors.New("github: pull request already exists")
)

// CreatePR opens a pull request from head into base in owner/repo. GitHub
// rejects an empty diff (ErrNoPRCommits) or a duplicate (ErrPRAlreadyExists) with
// 422; both are surfaced as typed errors.
func (c *Client) CreatePR(ctx context.Context, accessToken, owner, repo, head, base, title, body string) (*PullRequest, error) {
	payload, _ := json.Marshal(map[string]string{"title": title, "head": head, "base": base, "body": body})
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create pr: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusCreated {
		var pr PullRequest
		if err := json.Unmarshal(respBody, &pr); err != nil {
			return nil, fmt.Errorf("decode pr: %w", err)
		}
		if pr.HTMLURL == "" {
			return nil, fmt.Errorf("create pr: empty url")
		}
		return &pr, nil
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var e struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(respBody, &e)
		joined := strings.ToLower(e.Message)
		for _, er := range e.Errors {
			joined += " " + strings.ToLower(er.Message)
		}
		switch {
		case strings.Contains(joined, "no commits between"):
			return nil, ErrNoPRCommits
		case strings.Contains(joined, "already exist"):
			return nil, ErrPRAlreadyExists
		default:
			return nil, fmt.Errorf("create pr: %s", e.Message)
		}
	}
	return nil, fmt.Errorf("create pr: status %d", resp.StatusCode)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
