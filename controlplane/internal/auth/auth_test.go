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
	"sync/atomic"
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
	// userInfoCalls counts userinfo requests, so a test can tell a cache hit
	// from a fresh lookup.
	userInfoCalls atomic.Int64
	// liveAccessToken, when set, is the only bearer userinfo will answer for;
	// anything else gets 401, the way a real IdP treats an unknown token.
	liveAccessToken string
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
		f.userInfoCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if f.liveAccessToken != "" && r.Header.Get("Authorization") != "Bearer "+f.liveAccessToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
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

// setRoles rewrites userinfo to carry Zitadel's project-roles claim, shaped the
// way Zitadel emits it: role key -> granting org id -> org domain. Passing no
// roles emits the claim as an empty object; a user with no grants at all is
// covered by the fixtures that omit the claim entirely.
func (f *fakeIdP) setRoles(sub string, roles ...string) {
	entries := make([]string, 0, len(roles))
	for _, r := range roles {
		entries = append(entries, fmt.Sprintf("%q:{\"org-1\":\"test.example\"}", r))
	}
	f.userBody = fmt.Sprintf(
		`{"sub":%q,"name":"","preferred_username":"octocat","email":"octo@example.com","email_verified":true,"picture":"","urn:zitadel:iam:org:project:roles":{%s}}`,
		sub, strings.Join(entries, ","))
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
	IsAdmin         bool `json:"is_admin"`
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

// A deep link has to survive the trip through Zitadel. Without this the SPA
// auto-starts the flow and then lands everyone on the dashboard, quietly losing
// the page they actually asked for.
func TestDeepLinkSurvivesLogin(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	state := h.startFlow(t, "/api/auth/login?next=%2Fm%2Fabc%2Fpr%2F7")
	resp := h.callback(t, "valid-code", state)
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "/m/abc/pr/7" {
		t.Fatalf("callback: want redirect to the deep link, got %q", loc)
	}
}

// The other half: `next` is attacker-reachable, so a value pointing off-origin
// must land on the dashboard rather than anywhere it asked for.
func TestLoginNextCannotLeaveTheOrigin(t *testing.T) {
	for _, next := range []string{
		"//evil.example",
		"/\\evil.example",
		"https://evil.example/steal",
	} {
		t.Run(next, func(t *testing.T) {
			idp, gh := newFakeIdP(t), newFakeGitHub(t)
			h := newHarness(t, idp, gh, nil)

			state := h.startFlow(t, "/api/auth/login?next="+url.QueryEscape(next))
			resp := h.callback(t, "valid-code", state)
			defer resp.Body.Close()

			if loc := resp.Header.Get("Location"); loc != "/" {
				t.Fatalf("callback: want /, got %q", loc)
			}
		})
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

// --- Admin role (Zitadel proteos.admin) --------------------------------------

// isAdminInDB reads the persisted flag straight from the row, so these tests
// assert what was STORED and not merely what the API happened to echo back.
func (h *harness) isAdminInDB(t *testing.T, sub string) bool {
	t.Helper()
	var admin bool
	row := h.pool.QueryRow(context.Background(), "SELECT is_admin FROM users WHERE oidc_subject = $1", sub)
	if err := row.Scan(&admin); err != nil {
		t.Fatal(err)
	}
	return admin
}

// The grant has to survive the trip from the IdP's claim to the users row —
// that row is the only thing requireAdmin can consult, because the session
// carries no token to re-read the claim from.
func TestAdminRoleFromClaimIsPersisted(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-admin", "proteos.admin")
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()

	if !h.isAdminInDB(t, "sub-admin") {
		t.Fatal("proteos.admin in the claim did not set users.is_admin")
	}
	status, me := h.me(t)
	if status != http.StatusOK {
		t.Fatalf("/api/me: want 200, got %d", status)
	}
	if !me.IsAdmin {
		t.Fatal("/api/me is_admin should be true for a proteos.admin holder")
	}
}

// No claim at all is the ordinary case: the vast majority of users have no
// ProteOS grant in Zitadel, and that must resolve to a plain user rather than
// to an error or, worse, to an elevated default.
func TestNoRolesClaimMeansOrdinaryUser(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t) // default fixture omits the claim
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()

	if h.isAdminInDB(t, "sub-1") {
		t.Fatal("a user with no roles claim must not be admin")
	}
	if _, me := h.me(t); me.IsAdmin {
		t.Fatal("/api/me is_admin should be false with no roles claim")
	}
}

// The Zitadel project is SHARED with the suite's other apps, so their roles ride
// in the very same claim. Another app's admin grant must not confer ProteOS
// admin — this is the test that fails the moment someone "simplifies" the role
// check into a prefix or substring match.
func TestForeignProjectRolesDoNotGrantAdmin(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-foreign", "databox.admin", "internal", "beta", "sessions.read_all")
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()

	if h.isAdminInDB(t, "sub-foreign") {
		t.Fatal("another application's roles granted ProteOS admin")
	}
}

// Revocation has to propagate. Because the role is stored, clearing it depends
// on the refresh writing `false` rather than only ever writing `true` — the
// failure mode this guards is admin becoming permanent once granted.
func TestRevokedAdminRoleIsClearedOnNextLogin(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-admin", "proteos.admin")
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()
	if !h.isAdminInDB(t, "sub-admin") {
		t.Fatal("precondition: first login should have granted admin")
	}

	// The grant is withdrawn in Zitadel; the user signs in again.
	idp.setRoles("sub-admin")
	resp2 := h.callback(t, "valid-code", h.login(t))
	resp2.Body.Close()

	if h.isAdminInDB(t, "sub-admin") {
		t.Fatal("revoking proteos.admin did not clear users.is_admin on next login")
	}
	if _, me := h.me(t); me.IsAdmin {
		t.Fatal("/api/me still reports is_admin after the grant was revoked")
	}
}

// A pre-Zitadel row adopted by verified-email linking must pick up the role on
// that first OIDC login too — otherwise the earliest accounts, which are the
// likeliest to be the operators', could never become admin.
func TestAdminRoleAppliesToEmailLinkedUser(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-linked", "proteos.admin")
	h := newHarness(t, idp, gh, nil)

	// Seed a GitHub-era row with the same (verified) email and no OIDC identity.
	if _, err := h.queries.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 4242,
		Login:        "octocat",
		Email:        "octo@example.com",
		AvatarUrl:    "",
	}); err != nil {
		t.Fatal(err)
	}

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()

	if !h.isAdminInDB(t, "sub-linked") {
		t.Fatal("linked pre-Zitadel account did not receive the admin role")
	}
}

// --- Admin console (GET /api/admin/overview) ---------------------------------

// adminOverviewView is the console payload, decoded structurally so a rename in
// the API surface fails a test rather than silently zeroing a field.
type adminOverviewView struct {
	Totals struct {
		Machines          int            `json:"machines"`
		Running           int            `json:"running"`
		ByState           map[string]int `json:"by_state"`
		Users             int            `json:"users"`
		UsersWithMachines int            `json:"users_with_machines"`
	} `json:"totals"`
	Machines []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
		Owner struct {
			ID    string `json:"id"`
			Login string `json:"login"`
			Email string `json:"email"`
		} `json:"owner"`
	} `json:"machines"`
	Truncated bool `json:"truncated"`
}

func (h *harness) adminOverview(t *testing.T) (int, adminOverviewView) {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/api/admin/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v adminOverviewView
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, v
}

// The console is the one surface that shows a user rows they do not own, so the
// server-side gate matters far more than hiding the nav item: a signed-in
// non-admin must be refused even though they hold a perfectly valid session.
func TestAdminOverviewRefusesNonAdmin(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()

	if status, _ := h.adminOverview(t); status != http.StatusForbidden {
		t.Fatalf("/api/admin/overview as ordinary user: want 403, got %d", status)
	}
}

// Losing the grant has to close the door, not just hide it. This is the pair to
// TestRevokedAdminRoleIsClearedOnNextLogin: that one proves the flag clears,
// this one proves the route then refuses.
func TestAdminOverviewRefusesAfterRoleRevoked(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-admin", "proteos.admin")
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()
	if status, _ := h.adminOverview(t); status != http.StatusOK {
		t.Fatalf("precondition: admin should reach the console, got %d", status)
	}

	idp.setRoles("sub-admin") // grant withdrawn in Zitadel
	resp2 := h.callback(t, "valid-code", h.login(t))
	resp2.Body.Close()

	if status, _ := h.adminOverview(t); status != http.StatusForbidden {
		t.Fatalf("/api/admin/overview after revocation: want 403, got %d", status)
	}
}

// The console's reason to exist: an admin sees machines they do not own, with
// the owner attached and the totals counting the whole fleet.
func TestAdminOverviewReportsFleetAndOwners(t *testing.T) {
	idp, gh := newFakeIdP(t), newFakeGitHub(t)
	idp.setRoles("sub-admin", "proteos.admin")
	h := newHarness(t, idp, gh, nil)

	resp := h.callback(t, "valid-code", h.login(t))
	resp.Body.Close()
	adminID := userIDByOIDCSub(t, h, "sub-admin")

	// A second, unrelated user who owns two machines — one running, one stopped.
	ctx := context.Background()
	other, err := h.queries.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 777,
		Login:        "someone-else",
		Email:        "else@example.com",
		AvatarUrl:    "",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedMachine := func(owner pgtype.UUID, name, state string) {
		t.Helper()
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO machines (user_id, name, state, kernel_ref, rootfs_ref, resource_spec)
			 VALUES ($1, $2, $3, 'k', 'r', '{"vcpus":2,"mem_mib":2048}'::jsonb)`,
			owner, name, state); err != nil {
			t.Fatal(err)
		}
	}
	seedMachine(other.ID, "theirs-running", "running")
	seedMachine(other.ID, "theirs-stopped", "stopped")
	seedMachine(adminID, "mine-running", "running")

	status, ov := h.adminOverview(t)
	if status != http.StatusOK {
		t.Fatalf("/api/admin/overview: want 200, got %d", status)
	}
	if ov.Totals.Machines != 3 {
		t.Fatalf("totals.machines = %d, want 3", ov.Totals.Machines)
	}
	if ov.Totals.Running != 2 {
		t.Fatalf("totals.running = %d, want 2", ov.Totals.Running)
	}
	if ov.Totals.ByState["stopped"] != 1 {
		t.Fatalf("by_state[stopped] = %d, want 1", ov.Totals.ByState["stopped"])
	}
	if ov.Totals.Users != 2 || ov.Totals.UsersWithMachines != 2 {
		t.Fatalf("users = %d / with_machines = %d, want 2 / 2", ov.Totals.Users, ov.Totals.UsersWithMachines)
	}
	if ov.Truncated {
		t.Fatal("truncated should be false for a 3-machine fleet")
	}
	if len(ov.Machines) != 3 {
		t.Fatalf("machines = %d rows, want 3", len(ov.Machines))
	}
	// The other user's machines must be present AND attributed to them — a list
	// that silently showed only the caller's own would pass a bare count check.
	owners := map[string]string{}
	for _, m := range ov.Machines {
		owners[m.Name] = m.Owner.Login
	}
	if owners["theirs-running"] != "someone-else" || owners["theirs-stopped"] != "someone-else" {
		t.Fatalf("another user's machines not attributed correctly: %v", owners)
	}
	if owners["mine-running"] != "octocat" {
		t.Fatalf("own machine owner = %q, want octocat", owners["mine-running"])
	}
}
