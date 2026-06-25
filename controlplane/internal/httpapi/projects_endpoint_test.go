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

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// fakeProjectChannel implements httpapi.ProjectChannel with canned projects and
// an in-memory kv, and can simulate a missing channel.
type fakeProjectChannel struct {
	mu       sync.Mutex
	projects []guestwire.Project
	noChan   bool
	kv       map[string]string
}

func (f *fakeProjectChannel) HasChannel(string) bool { return !f.noChan }
func (f *fakeProjectChannel) ListProjects(context.Context, string) ([]guestwire.Project, error) {
	if f.noChan {
		return nil, guestctl.ErrNoChannel
	}
	return f.projects, nil
}
func (f *fakeProjectChannel) KVGet(_ context.Context, _, key string) (*string, error) {
	if f.noChan {
		return nil, guestctl.ErrNoChannel
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.kv[key]; ok {
		return &v, nil
	}
	return nil, nil
}
func (f *fakeProjectChannel) KVSet(_ context.Context, _, key, value string) error {
	if f.noChan {
		return guestctl.ErrNoChannel
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kv == nil {
		f.kv = map[string]string{}
	}
	f.kv[key] = value
	return nil
}

type projFixture struct {
	url   string
	token string
	ch    *fakeProjectChannel
}

func setupProjects(t *testing.T, machineState string, ch *fakeProjectChannel) projFixture {
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

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		Machines: machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{}),
		Projects: ch,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return projFixture{url: ts.URL, token: token, ch: ch}
}

func (fx projFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
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

func TestProjects_200(t *testing.T) {
	ch := &fakeProjectChannel{projects: []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha", Branch: "main", Dirty: false},
		{Name: "beta", Path: "/workspace/beta", Branch: "dev", Dirty: true},
	}}
	fx := setupProjects(t, string(machine.StateRunning), ch)
	resp := fx.do(t, http.MethodGet, "/api/projects", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Projects []guestwire.Project `json:"projects"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Projects) != 2 || body.Projects[0].Name != "alpha" || !body.Projects[1].Dirty {
		t.Fatalf("unexpected projects: %+v", body.Projects)
	}
}

func TestProjects_409WhenStopped(t *testing.T) {
	fx := setupProjects(t, string(machine.StateStopped), &fakeProjectChannel{})
	resp := fx.do(t, http.MethodGet, "/api/projects", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestDesktopLayoutRoundTrip(t *testing.T) {
	ch := &fakeProjectChannel{}
	fx := setupProjects(t, string(machine.StateRunning), ch)

	// Initially unset → layout null.
	resp := fx.do(t, http.MethodGet, "/api/machine/desktop", "", false)
	var got struct {
		Layout json.RawMessage `json:"layout"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if string(got.Layout) != "null" {
		t.Fatalf("initial layout = %s, want null", got.Layout)
	}

	// PUT a layout (needs CSRF header).
	layout := `{"windows":[{"id":"w1","x":10}]}`
	resp = fx.do(t, http.MethodPut, "/api/machine/desktop", `{"layout":`+layout+`}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// GET returns it verbatim.
	resp = fx.do(t, http.MethodGet, "/api/machine/desktop", "", false)
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if string(got.Layout) != layout {
		t.Fatalf("round-trip layout = %s, want %s", got.Layout, layout)
	}
}

func TestDesktopPut_CSRFRequired(t *testing.T) {
	fx := setupProjects(t, string(machine.StateRunning), &fakeProjectChannel{})
	resp := fx.do(t, http.MethodPut, "/api/machine/desktop", `{"layout":{}}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF)", resp.StatusCode)
	}
}
