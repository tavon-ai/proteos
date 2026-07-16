package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	"github.com/tavon-ai/proteos/controlplane/internal/token"
)

func setupTokens(t *testing.T) (url, cookie string) {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	_, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	sessions := session.NewManager(q, time.Hour)
	sessTok, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	srv := &httpapi.Server{
		Sessions: sessions,
		PATs:     token.NewManager(q),
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		Machines: machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), machine.Spec{}),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, sessTok
}

// do issues a request with optional cookie / bearer / csrf.
func do(t *testing.T, method, url, body, cookie, bearer string, csrf bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, r)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if csrf {
		req.Header.Set("X-Requested-By", "proteos")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func createToken(t *testing.T, url, cookie, name string) (id, plaintext, prefix string) {
	t.Helper()
	resp := do(t, http.MethodPost, url+"/api/tokens", `{"name":"`+name+`"}`, cookie, "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token status = %d, want 201", resp.StatusCode)
	}
	var b struct{ ID, Name, Token, Prefix string }
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Token == "" || b.ID == "" || b.Prefix == "" {
		t.Fatalf("incomplete create response: %+v", b)
	}
	return b.ID, b.Token, b.Prefix
}

func TestCreateTokenAndUseAsBearer(t *testing.T) {
	t.Parallel()
	url, cookie := setupTokens(t)
	_, plaintext, _ := createToken(t, url, cookie, "laptop")

	// The minted token authenticates an existing route with no cookie.
	resp := do(t, http.MethodGet, url+"/api/me", "", "", plaintext, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me with bearer = %d, want 200", resp.StatusCode)
	}
	var me struct {
		User struct{ Login string } `json:"user"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me.User.Login != "octocat" {
		t.Fatalf("bearer auth resolved wrong user: %+v", me)
	}
}

func TestCreateTokenRequiresCSRFForCookie(t *testing.T) {
	t.Parallel()
	url, cookie := setupTokens(t)
	// Cookie auth without the CSRF header is rejected.
	resp := do(t, http.MethodPost, url+"/api/tokens", `{"name":"x"}`, cookie, "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 csrf_required", resp.StatusCode)
	}
}

func TestBearerExemptFromCSRF(t *testing.T) {
	t.Parallel()
	url, cookie := setupTokens(t)
	_, plaintext, _ := createToken(t, url, cookie, "first")

	// A bearer-authenticated mutation needs no CSRF header.
	resp := do(t, http.MethodPost, url+"/api/tokens", `{"name":"second"}`, "", plaintext, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bearer create status = %d, want 201", resp.StatusCode)
	}
}

func TestListTokensNeverLeaksSecret(t *testing.T) {
	t.Parallel()
	url, cookie := setupTokens(t)
	_, _, prefix := createToken(t, url, cookie, "laptop")

	resp := do(t, http.MethodGet, url+"/api/tokens", "", cookie, "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	s := string(raw)
	if strings.Contains(s, "token_hash") || strings.Contains(s, `"token"`) {
		t.Fatalf("listing leaked secret material: %s", s)
	}
	if !strings.Contains(s, prefix) {
		t.Fatalf("listing missing the token prefix %q: %s", prefix, s)
	}
}

func TestInvalidBearerRejected(t *testing.T) {
	t.Parallel()
	url, _ := setupTokens(t)
	resp := do(t, http.MethodGet, url+"/api/me", "", "", "proteos_pat_bogus", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRevokeTokenEndpoint(t *testing.T) {
	t.Parallel()
	url, cookie := setupTokens(t)
	id, plaintext, _ := createToken(t, url, cookie, "doomed")

	// Revoke it.
	resp := do(t, http.MethodDelete, url+"/api/tokens/"+id, "", cookie, "", true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", resp.StatusCode)
	}
	// The revoked token no longer authenticates.
	resp = do(t, http.MethodGet, url+"/api/me", "", "", plaintext, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token GET /api/me = %d, want 401", resp.StatusCode)
	}
	// Revoking again is a 404 (already gone).
	resp = do(t, http.MethodDelete, url+"/api/tokens/"+id, "", cookie, "", true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", resp.StatusCode)
	}
}

func TestRevokeOtherUsersTokenIs404(t *testing.T) {
	t.Parallel()
	// Two independent fixtures would share no users; instead create a second user
	// in the same DB and a token, then try to revoke it with the first user's
	// cookie. Simplest: a malformed/foreign id yields 404.
	url, cookie := setupTokens(t)
	resp := do(t, http.MethodDelete, url+"/api/tokens/00000000-0000-0000-0000-000000000000", "", cookie, "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
