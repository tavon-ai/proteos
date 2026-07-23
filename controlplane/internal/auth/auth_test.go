package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/oidc"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// fakeIdP stands in for Zitadel: discovery, token, and userinfo endpoints.
// Knobs let each test force error paths and vary the identity returned. Only
// the external boundary is faked — our code runs for real.
type fakeIdP struct {
	server      *httptest.Server
	tokenStatus int
	tokenBody   string
	userStatus  int
	userBody    string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"zat_access","token_type":"Bearer","expires_in":3600,"id_token":"unused"}`,
		userStatus:  http.StatusOK,
		userBody:    `{"sub":"sub-1","name":"Octo Cat","preferred_username":"octocat","email":"octo@example.com","email_verified":true,"picture":"https://avatars/oc.png"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.server.URL,
			"authorization_endpoint": f.server.URL + "/oauth/v2/authorize",
			"token_endpoint":         f.server.URL + "/oauth/v2/token",
			"userinfo_endpoint":      f.server.URL + "/oidc/v1/userinfo",
		})
	})
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.tokenStatus)
		_, _ = w.Write([]byte(f.tokenBody))
	})
	mux.HandleFunc("/oidc/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.userStatus)
		_, _ = w.Write([]byte(f.userBody))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// setIdentity points the fake IdP's userinfo at a specific identity.
func (f *fakeIdP) setIdentity(sub, username, email string, verified bool) {
	f.userBody = fmt.Sprintf(
		`{"sub":%q,"name":"","preferred_username":%q,"email":%q,"email_verified":%v,"picture":""}`,
		sub, username, email, verified)
}

// fakeGitHub stands in for GitHub's token + user endpoints (Connect GitHub).
type fakeGitHub struct {
	server      *httptest.Server
	tokenStatus int
	tokenBody   string
	userStatus  int
	userBody    string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"gho_access","refresh_token":"ghr_refresh","token_type":"bearer","scope":"repo","expires_in":3600,"refresh_token_expires_in":15897600}`,
		userStatus:  http.StatusOK,
		userBody:    `{"id":12345,"login":"octocat","email":"octo@example.com","avatar_url":"https://avatars/oc.png"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.tokenStatus)
		_, _ = w.Write([]byte(f.tokenBody))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.userStatus)
		_, _ = w.Write([]byte(f.userBody))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type harness struct {
	server  *httptest.Server
	secrets secrets.Store
	queries *store.Queries
	pool    *pgxpool.Pool
	client  *http.Client
}

func newHarness(t *testing.T, idp *fakeIdP, gh *fakeGitHub, allowlist []string) *harness {
	t.Helper()
	pool, q := testutil.Postgres(t)
	sec, err := secrets.NewFileStore(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewManager(q, 30*24*time.Hour)
	oidcClient := oidc.NewClient(oidc.Config{
		Issuer:   idp.server.URL,
		ClientID: "test-zitadel-client",
	})
	ghClient := github.NewClient(github.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		TokenURL:     gh.server.URL + "/login/oauth/access_token",
		APIBaseURL:   gh.server.URL,
		AuthorizeURL: gh.server.URL + "/login/oauth/authorize",
	})

	authHandler := auth.NewHandler(auth.Config{
		StateKey:            []byte("test-state-key"),
		CookieSecure:        false, // httptest is plain HTTP; Secure cookies wouldn't round-trip
		SessionTTL:          30 * 24 * time.Hour,
		AllowedGitHubLogins: allowlist,
	}, oidcClient, ghClient, sessions, q, sec, nil)

	// /api/me reports the user's machines; wire a real (DB-backed) machine
	// service. No machine is ever created in these auth tests, so the node
	// client is never dialed.
	machineSvc := machine.NewService(pool, nil, nil, secrets.NewMemStore(), machine.Spec{})
	api := &httpapi.Server{Sessions: sessions, Auth: authHandler, Machines: machineSvc, Queries: q}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	// BaseURL must point at our own server so the callback URLs are correct.
	authHandler.SetBaseURL(srv.URL)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // never follow; assert on Location
		},
	}
	return &harness{server: srv, secrets: sec, queries: q, pool: pool, client: client}
}

// userIDByOIDCSub returns the pgtype.UUID of the user with the given OIDC subject.
func userIDByOIDCSub(t *testing.T, h *harness, sub string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	row := h.pool.QueryRow(context.Background(), "SELECT id FROM users WHERE oidc_subject = $1", sub)
	if err := row.Scan(&id); err != nil {
		t.Fatalf("lookup user by oidc subject %q: %v", sub, err)
	}
	return id
}

// login performs GET /api/auth/login and returns the state extracted from the
// redirect URL (the state + PKCE cookies are now in the jar).
func (h *harness) login(t *testing.T) string {
	t.Helper()
	return h.startFlow(t, "/api/auth/login")
}

// connect performs GET /api/auth/github/connect (requires a session in the jar).
func (h *harness) connect(t *testing.T) string {
	t.Helper()
	return h.startFlow(t, "/api/auth/github/connect")
}

func (h *harness) startFlow(t *testing.T, path string) string {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("%s: want 302, got %d", path, resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatalf("%s: no state in redirect", path)
	}
	return state
}

func (h *harness) callback(t *testing.T, code, state string) *http.Response {
	t.Helper()
	q := url.Values{"code": {code}, "state": {state}}
	resp, err := h.client.Get(h.server.URL + "/api/auth/callback?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) githubCallback(t *testing.T, code, state string) *http.Response {
	t.Helper()
	q := url.Values{"code": {code}, "state": {state}}
	resp, err := h.client.Get(h.server.URL + "/api/auth/github/callback?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// me fetches /api/me with the jar's session cookie.
type meView struct {
	User struct {
		Login     string `json:"login"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	GitHubConnected bool `json:"github_connected"`
}

func (h *harness) me(t *testing.T) (int, meView) {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v meView
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, v
}

func TestHappyPathOIDCLogin(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	state := h.login(t)
	resp := h.callback(t, "valid-code", state)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("callback: want redirect to /, got %q", loc)
	}
	if !hasCookie(resp, auth.SessionCookieName) {
		t.Fatal("callback: no session cookie set")
	}

	status, me := h.me(t)
	if status != http.StatusOK {
		t.Fatalf("/api/me: want 200, got %d", status)
	}
	if me.User.Login != "octocat" || me.User.Email != "octo@example.com" {
		t.Fatalf("unexpected user: %+v", me.User)
	}
	// Fresh OIDC signup: GitHub is not connected yet — the SPA gates on this.
	if me.GitHubConnected {
		t.Fatal("github_connected should be false before Connect GitHub")
	}

	// The row carries the OIDC identity and no GitHub id.
	var ghID *int64
	row := h.pool.QueryRow(context.Background(), "SELECT github_user_id FROM users WHERE oidc_subject = 'sub-1'")
	if err := row.Scan(&ghID); err != nil {
		t.Fatal(err)
	}
	if ghID != nil {
		t.Fatalf("github_user_id should be NULL after OIDC signup, got %d", *ghID)
	}
}

func TestConnectGitHubLinksAndStoresTokens(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	h.callback(t, "code", h.login(t)).Body.Close()

	resp := h.githubCallback(t, "gh-code", h.connect(t))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github callback: want 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("github callback: want redirect to /, got %q", loc)
	}

	status, me := h.me(t)
	if status != http.StatusOK || !me.GitHubConnected {
		t.Fatalf("github_connected should be true after connect (status %d, me %+v)", status, me)
	}

	// INVARIANT: tokens live in the secrets store, not Postgres.
	userID := userIDByOIDCSub(t, h, "sub-1")
	link, err := h.queries.GetGitHubLink(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.SecretRef, "secret/users/") {
		t.Fatalf("unexpected secret_ref: %q", link.SecretRef)
	}
	if strings.Contains(string(link.Metadata), "gho_access") || strings.Contains(string(link.Metadata), "ghr_refresh") {
		t.Fatal("token material leaked into github_links metadata")
	}
	stored, err := h.secrets.Get(link.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored["access_token"] != "gho_access" || stored["refresh_token"] != "ghr_refresh" {
		t.Fatalf("tokens not in secrets store: %+v", stored)
	}
}

func TestReconnectHealsRevokedGrant(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	h.callback(t, "code", h.login(t)).Body.Close()
	h.githubCallback(t, "gh-code", h.connect(t)).Body.Close()

	// Simulate a dead grant (what the TokenSource records when GitHub rejects
	// the refresh token) and rotate the fake's next token pair.
	userID := userIDByOIDCSub(t, h, "sub-1")
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`UPDATE github_links SET metadata = metadata || '{"revoked":true}' WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	gh.tokenBody = `{"access_token":"gho_fresh","refresh_token":"ghr_fresh","token_type":"bearer","scope":"repo","expires_in":3600,"refresh_token_expires_in":15897600}`

	// Re-running the connect flow (the Reconnect GitHub banner) heals it.
	h.githubCallback(t, "gh-code-2", h.connect(t)).Body.Close()

	link, err := h.queries.GetGitHubLink(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.Unmarshal(link.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Revoked {
		t.Fatal("reconnect should clear the revoked flag")
	}
	stored, err := h.secrets.Get(link.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored["access_token"] != "gho_fresh" || stored["refresh_token"] != "ghr_fresh" {
		t.Fatalf("reconnect should store the fresh token pair: %+v", stored)
	}
}

func TestConnectGitHubRequiresAuth(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	resp, err := h.client.Get(h.server.URL + "/api/auth/github/connect")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connect without session: want 401, got %d", resp.StatusCode)
	}
}

func TestLinkByVerifiedEmail(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	// A pre-Zitadel user: GitHub identity, no OIDC identity.
	legacy, err := h.queries.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 12345, Login: "octocat", Email: "octo@example.com", AvatarUrl: "a",
	})
	if err != nil {
		t.Fatal(err)
	}

	// First Zitadel login with the same, verified email links the existing row.
	h.callback(t, "code", h.login(t)).Body.Close()

	linked := userIDByOIDCSub(t, h, "sub-1")
	if linked != legacy.ID {
		t.Fatalf("verified-email login should link the legacy user, got a different row")
	}
	status, me := h.me(t)
	if status != http.StatusOK || !me.GitHubConnected {
		t.Fatalf("linked legacy user keeps GitHub connected (status %d, me %+v)", status, me)
	}

	var count int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("want 1 user row after linking, got %d", count)
	}
}

func TestUnverifiedEmailNeverLinks(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	if _, err := h.queries.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 12345, Login: "octocat", Email: "octo@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	// Same email but unverified: linking would be an account takeover vector,
	// so a fresh user is created instead.
	idp.setIdentity("sub-1", "impostor", "octo@example.com", false)
	h.callback(t, "code", h.login(t)).Body.Close()

	var count int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("unverified email must not link: want 2 user rows, got %d", count)
	}
}

func TestAmbiguousEmailRejected(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	ctx := context.Background()
	for i, login := range []string{"octo-a", "octo-b"} {
		if _, err := h.queries.UpsertUser(ctx, store.UpsertUserParams{
			GithubUserID: int64(100 + i), Login: login, Email: "octo@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	}

	resp := h.callback(t, "code", h.login(t))
	defer resp.Body.Close()
	assertRedirectError(t, resp, "link_ambiguous")
	if hasCookie(resp, auth.SessionCookieName) {
		t.Fatal("no session should be issued on an ambiguous link")
	}
}

func TestRepeatLoginUpsertsAndFreshSession(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	resp1 := h.callback(t, "c1", h.login(t))
	resp1.Body.Close()
	cookie1 := cookieValue(resp1, auth.SessionCookieName)

	resp2 := h.callback(t, "c2", h.login(t))
	resp2.Body.Close()
	cookie2 := cookieValue(resp2, auth.SessionCookieName)

	if cookie1 == "" || cookie2 == "" {
		t.Fatal("expected session cookies on both logins")
	}
	if cookie1 == cookie2 {
		t.Fatal("expected a fresh session token on repeat login")
	}

	var count int
	row := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE oidc_subject = 'sub-1'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("want 1 user row, got %d", count)
	}
}

func TestBadStateRejected(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	h.login(t) // sets valid state + PKCE cookies in the jar
	resp := h.callback(t, "code", "tampered-state")
	defer resp.Body.Close()
	assertRedirectError(t, resp, "bad_state")
	if hasCookie(resp, auth.SessionCookieName) {
		t.Fatal("no session should be issued on bad state")
	}
}

func TestMissingStateCookieRejected(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)
	// No login() call → no state cookie in the jar.
	resp := h.callback(t, "code", "whatever")
	defer resp.Body.Close()
	assertRedirectError(t, resp, "missing_state")
}

func TestIdPErrorParam(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)
	h.login(t)
	resp, err := h.client.Get(h.server.URL + "/api/auth/callback?error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assertRedirectError(t, resp, "idp_error")
}

func TestExchangeFailure(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.tokenStatus = http.StatusBadRequest
	idp.tokenBody = `{"error":"invalid_grant","error_description":"code expired"}`
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "bad", h.login(t))
	defer resp.Body.Close()
	assertRedirectError(t, resp, "exchange_failed")
}

func TestAllowlistBlocksUninvitedConnect(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, []string{"someone-else"})

	h.callback(t, "code", h.login(t)).Body.Close()
	resp := h.githubCallback(t, "gh-code", h.connect(t))
	defer resp.Body.Close()
	assertConnectError(t, resp, "not_invited")

	if _, me := h.me(t); me.GitHubConnected {
		t.Fatal("allowlisted-out account must not be linked")
	}
}

func TestGitHubAlreadyLinkedConflict(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	// Another user already owns GitHub id 12345 (different email so the OIDC
	// login below does not link to them).
	if _, err := h.queries.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 12345, Login: "first-owner", Email: "owner@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	h.callback(t, "code", h.login(t)).Body.Close()
	resp := h.githubCallback(t, "gh-code", h.connect(t))
	defer resp.Body.Close()
	assertConnectError(t, resp, "github_already_linked")
}

func TestLogoutRevokesSession(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)
	h.callback(t, "code", h.login(t)).Body.Close()

	// Logout requires the CSRF header.
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/auth/logout", nil)
	req.Header.Set("X-Requested-By", "proteos")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", resp.StatusCode)
	}

	if status, _ := h.me(t); status != http.StatusUnauthorized {
		t.Fatalf("post-logout /api/me: want 401, got %d", status)
	}
}

func TestLogoutRequiresCSRFHeader(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)
	h.callback(t, "code", h.login(t)).Body.Close()

	resp, err := h.client.Post(h.server.URL+"/api/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF header: want 403, got %d", resp.StatusCode)
	}
}

// --- helpers ---

func assertRedirectError(t *testing.T, resp *http.Response, code string) {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302 redirect, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?") || !strings.Contains(loc, "error="+code) {
		t.Fatalf("want /login redirect with error=%s, got %q", code, loc)
	}
}

// assertConnectError asserts the connect-flow error redirect: back to the
// dashboard (the connect gate screen), not the login page.
func assertConnectError(t *testing.T, resp *http.Response, code string) {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302 redirect, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/?") || !strings.Contains(loc, "github_error="+code) {
		t.Fatalf("want / redirect with github_error=%s, got %q", code, loc)
	}
}

func hasCookie(resp *http.Response, name string) bool {
	return cookieValue(resp, name) != ""
}

func cookieValue(resp *http.Response, name string) string {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return c.Value
		}
	}
	return ""
}
