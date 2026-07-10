package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// fakeChannel records clone dispatches and reports channel presence.
type fakeChannel struct {
	mu          sync.Mutex
	has         bool
	lastURL     string
	lastDest    string
	lastOpID    string
	cloneCalled bool
}

func (f *fakeChannel) HasChannel(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.has }
func (f *fakeChannel) Clone(_ context.Context, _, url, dest, opID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cloneCalled = true
	f.lastURL, f.lastDest, f.lastOpID = url, dest, opID
	return nil
}

// fakeGitHubAPI serves the App installations + repositories endpoints with a
// single repo, octocat/hello.
func fakeGitHubAPI(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user/installations", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":1,"installations":[{"id":111}]}`))
	})
	mux.HandleFunc("/user/installations/111/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"full_name":"octocat/hello","private":true,"default_branch":"main","pushed_at":"2026-01-02T03:04:05Z"}]}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

type gitFixture struct {
	url   string
	token string
	uid   string
	ch    *fakeChannel
	q     *store.Queries
	pool  *pgxpool.Pool
	mc    store.Machine
}

func setupGit(t *testing.T, revoked bool, machineState string) gitFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	if err := sec.Put(secrets.UserGitHubPath(uid), map[string]string{
		"access_token":            "gho_valid",
		"refresh_token":           "ghr_valid",
		"access_token_expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"revoked": revoked})
	if _, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{UserID: user.ID, Metadata: meta, SecretRef: secrets.UserGitHubPath(uid)}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`)})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state=$1 WHERE id=$2", machineState, mc.ID); err != nil {
		t.Fatalf("set state: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	gh := github.NewClient(github.Config{ClientID: "id", ClientSecret: "s", APIBaseURL: fakeGitHubAPI(t)})
	ch := &fakeChannel{has: machineState == string(machine.StateRunning)}

	srv := &httpapi.Server{
		Sessions:      sessions,
		Queries:       q,
		Audit:         audit.NewRecorder(q),
		Machines:      machine.NewService(pool, nil, machine.NewBroker(), sec, machine.Spec{}),
		GitHub:        gh,
		Tokens:        github.NewTokenSource(gh, q, sec),
		GitChannel:    ch,
		GitHost:       "github.com",
		GitHubAppSlug: "proteos-dev",
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return gitFixture{url: ts.URL, token: token, uid: uid, ch: ch, q: q, pool: pool, mc: mc}
}

func (fx gitFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	var req *http.Request
	if r != nil {
		req, _ = http.NewRequest(method, fx.url+path, r)
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

func TestGitRepos_200(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateRunning))
	resp := fx.do(t, http.MethodGet, "/api/git/repos", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Repos []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
		} `json:"repos"`
		GrantsURL string `json:"grants_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Repos) != 1 || body.Repos[0].FullName != "octocat/hello" || !body.Repos[0].Private {
		t.Fatalf("unexpected repos: %+v", body.Repos)
	}
	if !strings.Contains(body.GrantsURL, "apps/proteos-dev/installations/new") {
		t.Fatalf("unexpected grants_url: %q", body.GrantsURL)
	}
}

func TestGitRepos_409ReconnectWhenRevoked(t *testing.T) {
	fx := setupGit(t, true, string(machine.StateRunning))
	resp := fx.do(t, http.MethodGet, "/api/git/repos", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "reconnect_github" {
		t.Fatalf("error = %q, want reconnect_github", code)
	}
}

func TestGitClone_202(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateRunning))
	resp := fx.do(t, http.MethodPost, "/api/git/clone", `{"full_name":"octocat/hello"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		OpID string `json:"op_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.OpID == "" {
		t.Fatal("missing op_id")
	}
	if !fx.ch.cloneCalled {
		t.Fatal("clone was not dispatched to the channel")
	}
	if fx.ch.lastURL != "https://github.com/octocat/hello.git" {
		t.Fatalf("clone url = %q (token must not be embedded)", fx.ch.lastURL)
	}
	if fx.ch.lastDest != "/workspace/hello" {
		t.Fatalf("clone dest = %q", fx.ch.lastDest)
	}
}

// A repo outside the user's granted set is still clonable: the URL is
// host-pinned and the credential helper only supplies the user's own token, so
// the gate would only block harmless public clones. The dispatched URL must
// still target s.GitHost with no embedded token.
func TestGitClone_202NotListable(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateRunning))
	resp := fx.do(t, http.MethodPost, "/api/git/clone", `{"full_name":"someone/public"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if !fx.ch.cloneCalled {
		t.Fatal("clone was not dispatched for an ad-hoc repo")
	}
	if fx.ch.lastURL != "https://github.com/someone/public.git" {
		t.Fatalf("clone url = %q (token must not be embedded)", fx.ch.lastURL)
	}
	if fx.ch.lastDest != "/workspace/public" {
		t.Fatalf("clone dest = %q", fx.ch.lastDest)
	}
}

func TestGitClone_409NotRunning(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateStopped))
	resp := fx.do(t, http.MethodPost, "/api/git/clone", `{"full_name":"octocat/hello"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "machine_not_running" {
		t.Fatalf("error = %q, want machine_not_running", code)
	}
}

func TestGitClone_400BadFullName(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateRunning))
	resp := fx.do(t, http.MethodPost, "/api/git/clone", `{"full_name":"../etc/passwd"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGitClone_RequiresCSRF(t *testing.T) {
	fx := setupGit(t, false, string(machine.StateRunning))
	resp := fx.do(t, http.MethodPost, "/api/git/clone", `{"full_name":"octocat/hello"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Error
}
