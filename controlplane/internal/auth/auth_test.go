package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// fakeGitHub stands in for GitHub's token + user endpoints. Knobs let each test
// force error paths. Only the external boundary is faked — our code runs for real.
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

func newHarness(t *testing.T, gh *fakeGitHub, allowlist []string) *harness {
	t.Helper()
	pool, q := testutil.Postgres(t)
	sec, err := secrets.NewFileStore(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewManager(q, 30*24*time.Hour)
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
	}, ghClient, sessions, q, sec)

	api := &httpapi.Server{Sessions: sessions, Auth: authHandler}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	// BaseURL must point at our own server so callbackURL is correct.
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

// mustUserID returns the pgtype.UUID of the single seeded GitHub user (id 12345).
func mustUserID(t *testing.T, h *harness) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	row := h.pool.QueryRow(context.Background(), "SELECT id FROM users WHERE github_user_id = 12345")
	if err := row.Scan(&id); err != nil {
		t.Fatalf("lookup seeded user: %v", err)
	}
	return id
}

func mustPool(t *testing.T, h *harness) *pgxpool.Pool {
	t.Helper()
	return h.pool
}

// login performs GET /api/auth/github/login and returns the state extracted
// from the redirect URL (the matching cookie is now in the jar).
func (h *harness) login(t *testing.T) string {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/api/auth/github/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login: no state in redirect")
	}
	return state
}

func (h *harness) callback(t *testing.T, code, state string) *http.Response {
	t.Helper()
	q := url.Values{"code": {code}, "state": {state}}
	resp, err := h.client.Get(h.server.URL + "/api/auth/github/callback?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHappyPathLogin(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)

	state := h.login(t)
	resp := h.callback(t, "valid-code", state)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("callback: want redirect to /, got %q", loc)
	}

	// Session cookie issued?
	if !hasCookie(resp, auth.SessionCookieName) {
		t.Fatal("callback: no session cookie set")
	}

	// /api/me now returns the authenticated user.
	meResp, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me: want 200, got %d", meResp.StatusCode)
	}
	var me struct {
		User struct {
			Login     string `json:"login"`
			Email     string `json:"email"`
			AvatarURL string `json:"avatar_url"`
		} `json:"user"`
		Machine *any `json:"machine"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.User.Login != "octocat" || me.User.Email != "octo@example.com" {
		t.Fatalf("unexpected user: %+v", me.User)
	}
	if me.Machine != nil {
		t.Fatalf("machine should be null this phase, got %v", *me.Machine)
	}

	// INVARIANT: tokens live in the secrets store, not Postgres.
	link, err := h.queries.GetGitHubLink(context.Background(), mustUserID(t, h))
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

func TestRepeatLoginUpsertsAndFreshSession(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)

	// First login.
	resp1 := h.callback(t, "c1", h.login(t))
	resp1.Body.Close()
	cookie1 := cookieValue(resp1, auth.SessionCookieName)

	// Second login (same GitHub id) — upsert, new session token.
	resp2 := h.callback(t, "c2", h.login(t))
	resp2.Body.Close()
	cookie2 := cookieValue(resp2, auth.SessionCookieName)

	if cookie1 == "" || cookie2 == "" {
		t.Fatal("expected session cookies on both logins")
	}
	if cookie1 == cookie2 {
		t.Fatal("expected a fresh session token on repeat login")
	}

	// Exactly one user row for this GitHub id (upsert, not duplicate).
	var count int
	row := mustPool(t, h).QueryRow(context.Background(), "SELECT count(*) FROM users WHERE github_user_id = 12345")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("want 1 user row, got %d", count)
	}
}

func TestBadStateRejected(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)

	h.login(t) // sets a valid state cookie in the jar
	// Present a different state value than the cookie.
	resp := h.callback(t, "code", "tampered-state")
	defer resp.Body.Close()
	assertRedirectError(t, resp, "bad_state")
	if hasCookie(resp, auth.SessionCookieName) {
		t.Fatal("no session should be issued on bad state")
	}
}

func TestMissingStateCookieRejected(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)
	// No login() call → no state cookie in the jar.
	resp := h.callback(t, "code", "whatever")
	defer resp.Body.Close()
	assertRedirectError(t, resp, "missing_state")
}

func TestGitHubErrorResponse(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.tokenStatus = http.StatusOK
	gh.tokenBody = `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`
	h := newHarness(t, gh, nil)

	resp := h.callback(t, "bad", h.login(t))
	defer resp.Body.Close()
	assertRedirectError(t, resp, "exchange_failed")
}

func TestProviderErrorParam(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)
	h.login(t)
	resp, err := h.client.Get(h.server.URL + "/api/auth/github/callback?error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assertRedirectError(t, resp, "github_error")
}

func TestAllowlistBlocksUninvited(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, []string{"someone-else"})
	resp := h.callback(t, "code", h.login(t))
	defer resp.Body.Close()
	assertRedirectError(t, resp, "not_invited")
}

func TestLogoutRevokesSession(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)
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

	// Session is gone — /api/me now 401 (jar still holds the cleared cookie,
	// but server-side revoke makes it invalid).
	meResp, err := h.client.Get(h.server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout /api/me: want 401, got %d", meResp.StatusCode)
	}
}

func TestLogoutRequiresCSRFHeader(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, gh, nil)
	h.callback(t, "code", h.login(t)).Body.Close()

	// No X-Requested-By header → rejected.
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
	if !strings.Contains(loc, "error="+code) {
		t.Fatalf("want redirect with error=%s, got %q", code, loc)
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
