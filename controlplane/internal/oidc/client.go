// Package oidc is a minimal OpenID Connect client for the Zitadel login flow
// (TAV-149): discover endpoints, build the authorize URL (authorization code +
// PKCE, public client — no secret), exchange the code, and fetch the userinfo
// claims. Claims come from the userinfo endpoint over TLS rather than from
// parsing the ID token, so no JWT verification machinery is needed. The issuer
// URL is injectable so tests can drive the flow against an httptest fake.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config configures the client. Issuer is the IdP base URL, e.g.
// https://auth.tavon.io (no trailing slash needed).
type Config struct {
	Issuer     string
	ClientID   string
	HTTPClient *http.Client
}

// Client talks to the IdP's discovery, authorize, token, and userinfo endpoints.
type Client struct {
	issuer   string
	clientID string
	http     *http.Client

	mu   sync.Mutex
	disc *discovery
}

// discovery is the subset of the OIDC discovery document the flow needs.
type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// NewClient builds a client from cfg.
func NewClient(cfg Config) *Client {
	c := &Client{
		issuer:   strings.TrimRight(cfg.Issuer, "/"),
		clientID: cfg.ClientID,
		http:     cfg.HTTPClient,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	return c
}

// discover fetches (once) and caches the discovery document.
func (c *Client) discover(ctx context.Context) (*discovery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disc != nil {
		return c.disc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decode oidc discovery: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: document missing endpoints")
	}
	c.disc = &d
	return c.disc, nil
}

// Issuer returns the issuer as declared by the discovery document. Stored with
// the user's subject so identities stay unambiguous if the IdP ever changes.
func (c *Client) Issuer(ctx context.Context) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	if d.Issuer != "" {
		return d.Issuer, nil
	}
	return c.issuer, nil
}

// NewVerifier mints a PKCE code verifier (RFC 7636: 43–128 chars, unreserved).
func NewVerifier() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// challengeS256 derives the S256 code challenge from a verifier.
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL builds the URL to redirect the user to, carrying the opaque
// state, the callback redirect URI, and the PKCE challenge.
func (c *Client) AuthorizeURL(ctx context.Context, state, redirectURI, verifier string) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirectURI)
	// urn:zitadel:iam:org:projects:roles asks Zitadel to include the caller's
	// project roles in the userinfo response, which is where the admin grant is
	// read from. Requested explicitly rather than relying on the project's
	// "assert roles on authentication" setting: that setting is project-wide and
	// the project is shared with the suite's other apps, so ProteOS should not
	// depend on — or need to change — a flag that also moves their tokens.
	//
	// Note this covers the BROWSER login only. Access tokens minted for the
	// other first-party apps and presented to this API (see auth/bearer.go)
	// carry whatever scopes those apps requested; if they omit roles, their
	// bearer callers resolve as non-admin. That is the safe direction to fail,
	// and the admin console is a browser surface regardless.
	q.Set("scope", "openid profile email urn:zitadel:iam:org:projects:roles")
	q.Set("state", state)
	q.Set("code_challenge", challengeS256(verifier))
	q.Set("code_challenge_method", "S256")
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Token is the token-exchange response.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IDToken     string `json:"id_token"`
}

// tokenError mirrors the OAuth2 error response on a failed exchange.
type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange swaps an authorization code (plus the PKCE verifier) for tokens.
func (c *Client) Exchange(ctx context.Context, code, redirectURI, verifier string) (*Token, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", c.clientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
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
		var terr tokenError
		if err := json.Unmarshal(body, &terr); err == nil && terr.Error != "" {
			return nil, fmt.Errorf("token exchange: %s", terr.Error)
		}
		return nil, fmt.Errorf("token exchange: status %d", resp.StatusCode)
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

// UserInfo is the subset of the OIDC userinfo claims the login flow persists.
type UserInfo struct {
	Subject           string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Picture           string `json:"picture"`

	// Roles is Zitadel's project-roles claim, shaped role key -> org id -> org
	// domain. Only the outer keys matter here; the inner map says which orgs
	// granted the role, which ProteOS (single-tenant) does not distinguish on.
	//
	// The claim is absent unless the Zitadel project asserts roles on
	// authentication (see scripts/zitadel/register-proteos-admin-role.sh). An
	// absent claim is not an error — it decodes to nil and HasRole reports
	// false, which is exactly right: no grant means no elevated access.
	Roles map[string]map[string]string `json:"urn:zitadel:iam:org:project:roles"`
}

// HasRole reports whether the userinfo carries the given project role key.
//
// Membership is an exact key lookup and never a prefix or substring match: the
// Zitadel project is shared with the suite's other apps, so this claim also
// carries their roles (databox.admin, internal, beta, …). Anything looser would
// let a role defined for another app confer ProteOS privilege.
func (u *UserInfo) HasRole(key string) bool {
	_, ok := u.Roles[key]
	return ok
}

// GetUserInfo fetches the userinfo claims using the access token.
func (c *Client) GetUserInfo(ctx context.Context, accessToken string) (*UserInfo, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get userinfo: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var u UserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	if u.Subject == "" {
		return nil, fmt.Errorf("get userinfo: missing sub")
	}
	return &u, nil
}

// Login returns the display login for a userinfo: preferred_username, falling
// back to name, then email — Zitadel accounts need not resemble GitHub logins.
func (u *UserInfo) Login() string {
	switch {
	case u.PreferredUsername != "":
		return u.PreferredUsername
	case u.Name != "":
		return u.Name
	default:
		return u.Email
	}
}
