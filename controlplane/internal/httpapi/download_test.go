package httpapi_test

import (
	"context"
	"io"
	"net"
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
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// dlFakeGuest is a plain HTTP server that mimics the guest's download route.
// It serves GET /download with a minimal zip body so the handler can proxy it.
type dlFakeGuest struct {
	srv      *httptest.Server
	requests []*http.Request // captured by the handler; read by the test
}

func newDLFakeGuest(t *testing.T) *dlFakeGuest {
	t.Helper()
	fg := &dlFakeGuest{}
	mux := http.NewServeMux()
	mux.HandleFunc(guestwire.RouteDownloadPath, func(w http.ResponseWriter, r *http.Request) {
		fg.requests = append(fg.requests, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/zip")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ZIPDATA")
	})
	fg.srv = httptest.NewServer(mux)
	t.Cleanup(fg.srv.Close)
	return fg
}

// DialGuest satisfies gateway.GuestDialer. It dials the fake HTTP server's
// listener directly, handing the raw TCP connection back to the download
// handler which then uses it as a one-shot HTTP transport.
func (fg *dlFakeGuest) DialGuest(_ context.Context, _ string, _ uint32) (net.Conn, error) {
	addr := strings.TrimPrefix(fg.srv.URL, "http://")
	return net.Dial("tcp", addr)
}

// dlFixture is a wired control plane for download endpoint tests.
type dlFixture struct {
	url   string
	token string
	mid   string
	ch    *fakeProjectChannel
	fg    *dlFakeGuest
}

// setupDownload seeds a user + session + host + running machine and wires a
// Server with a Projects channel (fakeProjectChannel) and a Guests dialer
// (dlFakeGuest). The machine must be running for the download path to proceed.
func setupDownload(t *testing.T) dlFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 77, Login: "dl-user", Email: "dl@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", mc.ID); err != nil {
		t.Fatalf("set running: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	ch := &fakeProjectChannel{
		projects: []guestwire.Project{
			{Name: "alpha", Path: "/workspace/alpha", Branch: "main"},
		},
	}
	fg := newDLFakeGuest(t)

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		Machines: machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{}),
		Projects: ch,
		Guests:   fg,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return dlFixture{
		url:   ts.URL,
		token: token,
		mid:   machine.UUIDString(mc.ID),
		ch:    ch,
		fg:    fg,
	}
}

func (fx dlFixture) getDownload(t *testing.T, path string, withCookie bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fx.url+path, nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// TestProjectDownload_200 is the golden path: authenticated user with a
// running machine requests a listable project → 200 with zip body and the
// correct Content-Disposition header.
func TestProjectDownload_200(t *testing.T) {
	fx := setupDownload(t)
	resp := fx.getDownload(t, "/api/projects/download?path=/workspace/alpha&machine="+fx.mid, true)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "alpha.zip") {
		t.Errorf("Content-Disposition = %q, want to contain alpha.zip", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ZIPDATA" {
		t.Errorf("body = %q, want ZIPDATA", body)
	}

	// The fake guest received the request with the correct cwd query param.
	if len(fx.fg.requests) == 0 {
		t.Fatal("no request reached the fake guest")
	}
	gotCwd := fx.fg.requests[0].URL.Query().Get(guestwire.QueryParamCwd)
	if gotCwd != "/workspace/alpha" {
		t.Errorf("guest cwd = %q, want /workspace/alpha", gotCwd)
	}
}

// TestProjectDownload_401Unauthenticated proves the download route is behind
// the requireAuth middleware.
func TestProjectDownload_401Unauthenticated(t *testing.T) {
	fx := setupDownload(t)
	resp := fx.getDownload(t, "/api/projects/download?path=/workspace/alpha&machine="+fx.mid, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestProjectDownload_400BadPath proves that a path not in the machine's
// listable project set is rejected before the guest is dialled.
func TestProjectDownload_400BadPath(t *testing.T) {
	fx := setupDownload(t)
	// "/workspace/secret" is not in the fakeProjectChannel's project list.
	resp := fx.getDownload(t, "/api/projects/download?path=/workspace/secret&machine="+fx.mid, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// The guest must not have been contacted.
	if len(fx.fg.requests) != 0 {
		t.Errorf("guest was contacted but should not have been for a bad path")
	}
}

// TestProjectDownload_400EmptyPath proves that an empty path query parameter
// is rejected.
func TestProjectDownload_400EmptyPath(t *testing.T) {
	fx := setupDownload(t)
	resp := fx.getDownload(t, "/api/projects/download?machine="+fx.mid, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty path)", resp.StatusCode)
	}
}

// TestProjectDownload_409NotRunning proves that a machine that is stopped
// results in 409, not a guest dial.
func TestProjectDownload_409NotRunning(t *testing.T) {
	// Build a stopped-machine fixture without reusing setupDownload.
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, _ := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 78, Login: "dl-stop"})
	mc, _ := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	pool.Exec(ctx, "UPDATE machines SET state='stopped' WHERE id=$1", mc.ID)

	sessions := session.NewManager(q, time.Hour)
	token, _ := sessions.Create(ctx, user.ID)
	mid := machine.UUIDString(mc.ID)

	fg := newDLFakeGuest(t)
	ch := &fakeProjectChannel{projects: []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha"},
	}}
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Machines: machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{}),
		Projects: ch,
		Guests:   fg,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/projects/download?path=/workspace/alpha&machine="+mid, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (machine not running)", resp.StatusCode)
	}
	if len(fg.requests) != 0 {
		t.Error("guest was contacted but should not have been for a non-running machine")
	}
}
