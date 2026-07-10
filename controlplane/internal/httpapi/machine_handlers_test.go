package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// machFixture holds a wired test server and the IDs/credentials of the seeded data.
type machFixture struct {
	url   string
	token string   // session cookie value
	mid   string   // canonical UUID of the seeded machine
	extra string   // canonical UUID of an optional second machine (empty if not seeded)
}

// setupMach seeds a user + session + host + one machine in the given state, then
// serves a control plane. NodeClient is stubNodeClient (already defined in
// gateway_test.go); the MemStore is seeded with the machine's volume key so
// Start can call ensureOnAgent without failing.
func setupMach(t *testing.T, state string) machFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 7, Login: "tester", Email: "t@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, Name: "primary",
		KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state=$1 WHERE id=$2", state, mc.ID); err != nil {
		t.Fatalf("set state: %v", err)
	}
	mid := machine.UUIDString(mc.ID)

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	sec := secrets.NewMemStore()
	// Seed the volume key so ensureOnAgent (called by Start) can fetch it.
	if _, err := secrets.MintMachineVolumeKey(sec, deterministicEntropy{}, mid); err != nil {
		t.Fatalf("mint volume key: %v", err)
	}

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Machines: machine.NewService(pool, stubNodeClient{}, machine.NewBroker(), sec, machine.Spec{}),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return machFixture{url: ts.URL, token: token, mid: mid}
}

// setupMachMulti is like setupMach but also creates a second machine (stopped).
func setupMachMulti(t *testing.T) machFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 8, Login: "multi", Email: "m@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	mc1, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, Name: "first",
		KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine1: %v", err)
	}
	mc2, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, Name: "second",
		KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine2: %v", err)
	}
	// Both seeded as stopped.
	for _, id := range []interface{}{mc1.ID, mc2.ID} {
		if _, err := pool.Exec(ctx, "UPDATE machines SET state='stopped' WHERE id=$1", id); err != nil {
			t.Fatalf("set state: %v", err)
		}
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	sec := secrets.NewMemStore()
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Machines: machine.NewService(pool, stubNodeClient{}, machine.NewBroker(), sec, machine.Spec{}),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return machFixture{url: ts.URL, token: token, mid: machine.UUIDString(mc1.ID), extra: machine.UUIDString(mc2.ID)}
}

// doMach issues a request to the fixture server.
func (fx machFixture) doMach(t *testing.T, method, path, body string, withCookie, withCSRF bool) *http.Response {
	t.Helper()
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(method, fx.url+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, fx.url+path, nil)
	}
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	}
	if withCSRF {
		req.Header.Set("X-Requested-By", "proteos")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// deterministicEntropy is a fixed entropy source for MintMachineVolumeKey in tests.
type deterministicEntropy struct{}

func (deterministicEntropy) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i % 256)
	}
	return len(p), nil
}

// ─── handleListMachines ───────────────────────────────────────────────────────

func TestListMachines_Empty(t *testing.T) {
	// Seed a user+session with no machines.
	ctx := context.Background()
	pool, q := testutil.Postgres(t)
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 99, Login: "empty-user"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Machines: machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), machine.Spec{}),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/machines", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body []json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body) != 0 {
		t.Fatalf("expected empty list, got %d items", len(body))
	}
}

func TestListMachines_Multiple(t *testing.T) {
	fx := setupMachMulti(t)
	resp := fx.doMach(t, http.MethodGet, "/api/machines", "", true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(body))
	}
	names := map[string]bool{}
	for _, m := range body {
		names[m.Name] = true
	}
	if !names["first"] || !names["second"] {
		t.Fatalf("unexpected machine names: %+v", body)
	}
}

// ─── handleStartMachine ──────────────────────────────────────────────────────

func TestStartMachine_202(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/"+fx.mid+"/start", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.ID != fx.mid {
		t.Errorf("id = %q, want %q", body.ID, fx.mid)
	}
	if body.State == string(machine.StateStopped) {
		t.Errorf("state = %q, should have advanced past stopped", body.State)
	}
}

func TestStartMachine_409InvalidState(t *testing.T) {
	fx := setupMach(t, string(machine.StateRunning))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/"+fx.mid+"/start", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "invalid_state" {
		t.Fatalf("error = %q, want invalid_state", code)
	}
}

func TestStartMachine_404Unknown(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/00000000-0000-0000-0000-000000000000/start", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStartMachine_RequiresCSRF(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/"+fx.mid+"/start", "", true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

// ─── handleStopMachine ───────────────────────────────────────────────────────

func TestStopMachine_202(t *testing.T) {
	fx := setupMach(t, string(machine.StateRunning))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/"+fx.mid+"/stop", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.State == string(machine.StateRunning) {
		t.Errorf("state = %q, should have advanced past running", body.State)
	}
}

func TestStopMachine_409InvalidState(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/"+fx.mid+"/stop", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "invalid_state" {
		t.Fatalf("error = %q, want invalid_state", code)
	}
}

func TestStopMachine_404Unknown(t *testing.T) {
	fx := setupMach(t, string(machine.StateRunning))
	resp := fx.doMach(t, http.MethodPost, "/api/machines/00000000-0000-0000-0000-000000000000/stop", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ─── handleRenameMachine ─────────────────────────────────────────────────────

func TestRenameMachine_200(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/"+fx.mid, `{"name":"renamed"}`, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", body.Name)
	}
	if body.ID != fx.mid {
		t.Fatalf("id = %q, want %q", body.ID, fx.mid)
	}
}

func TestRenameMachine_400EmptyName(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/"+fx.mid, `{"name":""}`, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRenameMachine_400WhitespaceName(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/"+fx.mid, `{"name":"   "}`, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (whitespace-only name)", resp.StatusCode)
	}
}

func TestRenameMachine_400BadBody(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/"+fx.mid, `not json`, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (malformed body)", resp.StatusCode)
	}
}

func TestRenameMachine_404Unknown(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/00000000-0000-0000-0000-000000000000", `{"name":"x"}`, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRenameMachine_RequiresCSRF(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodPatch, "/api/machines/"+fx.mid, `{"name":"x"}`, true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

// ─── handleDestroyMachine ────────────────────────────────────────────────────

func TestDestroyMachine_204(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodDelete, "/api/machines/"+fx.mid, "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	// The machine is now gone — a second GET must return 404.
	get := fx.doMach(t, http.MethodDelete, "/api/machines/"+fx.mid, "", true, true)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404 (machine was destroyed)", get.StatusCode)
	}
}

func TestDestroyMachine_404Unknown(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodDelete, "/api/machines/00000000-0000-0000-0000-000000000000", "", true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDestroyMachine_RequiresCSRF(t *testing.T) {
	fx := setupMach(t, string(machine.StateStopped))
	resp := fx.doMach(t, http.MethodDelete, "/api/machines/"+fx.mid, "", true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}
