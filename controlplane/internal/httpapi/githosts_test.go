package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/gitea"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// fakeGiteaAPI serves GET /user gated on the known PAT, standing in for a
// Gitea/Forgejo instance during PAT validation.
func fakeGiteaAPI(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token pat-ok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token is required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"login":"ivan","id":1}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

type gitHostsFixture struct {
	url   string
	token string
	uid   string
	sec   secrets.Store
}

func setupGitHosts(t *testing.T) gitHostsFixture {
	t.Helper()
	ctx := context.Background()
	_, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	sec := secrets.NewMemStore()
	giteaURL := fakeGiteaAPI(t)
	srv := &httpapi.Server{
		Sessions:       sessions,
		Queries:        q,
		Audit:          audit.NewRecorder(q),
		Secrets:        sec,
		GitPublicHosts: []string{"codeberg.example", "git.example.com:3000"},
		GiteaFor:       func(string) *gitea.Client { return gitea.New(giteaURL) },
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return gitHostsFixture{url: ts.URL, token: token, uid: machine.UUIDString(user.ID), sec: sec}
}

func (fx gitHostsFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
	t.Helper()
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(method, fx.url+path, strings.NewReader(body))
	} else {
		req, _ = http.NewRequest(method, fx.url+path, nil)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	if csrf {
		req.Header.Set("X-Requested-By", "proteos")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (fx gitHostsFixture) hosts(t *testing.T) map[string]struct {
	Linked bool
	Login  string
} {
	t.Helper()
	resp := fx.do(t, http.MethodGet, "/api/git/hosts", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET hosts status = %d", resp.StatusCode)
	}
	var body struct {
		Hosts []struct {
			Host   string `json:"host"`
			Linked bool   `json:"linked"`
			Login  string `json:"login"`
		} `json:"hosts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	out := map[string]struct {
		Linked bool
		Login  string
	}{}
	for _, h := range body.Hosts {
		out[h.Host] = struct {
			Linked bool
			Login  string
		}{h.Linked, h.Login}
	}
	return out
}

func TestGitHosts_SetTokenAndList(t *testing.T) {
	fx := setupGitHosts(t)

	// Unlinked to start, one row per allowlisted host.
	hosts := fx.hosts(t)
	if len(hosts) != 2 || hosts["codeberg.example"].Linked {
		t.Fatalf("initial hosts = %+v", hosts)
	}

	resp := fx.do(t, http.MethodPut, "/api/git/hosts/codeberg.example/token", `{"token":"pat-ok"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set token status = %d", resp.StatusCode)
	}
	var view struct {
		Host   string `json:"host"`
		Linked bool   `json:"linked"`
		Login  string `json:"login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&view)
	if !view.Linked || view.Login != "ivan" {
		t.Fatalf("set token response = %+v", view)
	}

	// The list reflects the link; the secret holds token + login.
	if h := fx.hosts(t)["codeberg.example"]; !h.Linked || h.Login != "ivan" {
		t.Fatalf("after set: %+v", h)
	}
	data, err := fx.sec.Get(secrets.UserGitHostPath(fx.uid, "codeberg.example"))
	if err != nil || data[secrets.GitHostFieldToken] != "pat-ok" || data[secrets.GitHostFieldLogin] != "ivan" {
		t.Fatalf("stored secret = (%v, %v)", data, err)
	}
}

func TestGitHosts_BadTokenRejectedWithoutStoring(t *testing.T) {
	fx := setupGitHosts(t)
	resp := fx.do(t, http.MethodPut, "/api/git/hosts/codeberg.example/token", `{"token":"pat-wrong"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "bad_token" {
		t.Fatalf("error = %q, want bad_token", code)
	}
	if _, err := fx.sec.Get(secrets.UserGitHostPath(fx.uid, "codeberg.example")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("secret must not be stored on a rejected token, got err=%v", err)
	}
	if fx.hosts(t)["codeberg.example"].Linked {
		t.Fatal("host must stay unlinked after a rejected token")
	}
}

func TestGitHosts_UnknownHost404(t *testing.T) {
	fx := setupGitHosts(t)
	resp := fx.do(t, http.MethodPut, "/api/git/hosts/evil.example/token", `{"token":"pat-ok"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "unknown_host" {
		t.Fatalf("error = %q, want unknown_host", code)
	}
}

func TestGitHosts_EmptyToken400(t *testing.T) {
	fx := setupGitHosts(t)
	resp := fx.do(t, http.MethodPut, "/api/git/hosts/codeberg.example/token", `{"token":"  "}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGitHosts_DeleteIsIdempotent(t *testing.T) {
	fx := setupGitHosts(t)
	resp := fx.do(t, http.MethodPut, "/api/git/hosts/codeberg.example/token", `{"token":"pat-ok"}`, true)
	resp.Body.Close()

	del := fx.do(t, http.MethodDelete, "/api/git/hosts/codeberg.example/token", "", true)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", del.StatusCode)
	}
	if fx.hosts(t)["codeberg.example"].Linked {
		t.Fatal("host still linked after delete")
	}
	if _, err := fx.sec.Get(secrets.UserGitHostPath(fx.uid, "codeberg.example")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("secret must be deleted, got err=%v", err)
	}

	again := fx.do(t, http.MethodDelete, "/api/git/hosts/codeberg.example/token", "", true)
	again.Body.Close()
	if again.StatusCode != http.StatusNoContent {
		t.Fatalf("second delete status = %d, want 204 (idempotent)", again.StatusCode)
	}
}

func TestGitHosts_SetTokenRequiresCSRF(t *testing.T) {
	fx := setupGitHosts(t)
	resp := fx.do(t, http.MethodPut, "/api/git/hosts/codeberg.example/token", `{"token":"pat-ok"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}
