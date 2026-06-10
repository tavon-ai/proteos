// Package github is a minimal client for the GitHub App user-authorization
// (OAuth) flow: build the authorize URL, exchange a code for user tokens, and
// fetch the authenticated user's profile. Endpoint URLs are injectable so tests
// can drive the flow against an httptest fake of GitHub.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
