package httpapi_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

const testWSOrigin = "http://localhost:5173"

// stubNodeClient satisfies machine.NodeClient; resolution (Get/GetByID) never
// calls it, so the methods are inert.
type stubNodeClient struct{}

func (stubNodeClient) Ensure(context.Context, string, agentapi.EnsureRequest) (agentapi.EnsureResponse, error) {
	return agentapi.EnsureResponse{}, nil
}
func (stubNodeClient) Stop(context.Context, string) error { return nil }
func (stubNodeClient) Status(context.Context, string) (agentapi.MachineStatus, error) {
	return agentapi.MachineStatus{}, nil
}

// failDialer is a gateway.GuestDialer that must never be reached (used by the
// authz tests, where every case errors before the tunnel dial).
type failDialer struct{ t *testing.T }

func (d failDialer) DialGuest(context.Context, string) (net.Conn, error) {
	d.t.Error("DialGuest reached during a pre-upgrade authz check")
	return nil, errors.New("should not dial")
}

// cpFixture is a wired control plane over a real (test) database.
type cpFixture struct {
	url         string
	token       string // session cookie value for the seeded user
	machineID   string // canonical UUID of the seeded machine
	machinePgID pgtype.UUID
	sessions    *session.Manager
	pool        *pgxpool.Pool
	q           *store.Queries
	userID      pgtype.UUID
}

// setupCP seeds a user, session, host, and a running machine, then serves a
// control plane whose gateway uses the given dialer and allowed origins.
func setupCP(t *testing.T, dialer gateway.GuestDialer, origins []string) cpFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 4242, Login: "term-user", Email: "t@example.com", AvatarUrl: "",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	m, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}
	// Force it to running for the gateway happy path (bypassing the guarded
	// lifecycle is fine for a fixture).
	if _, err := pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", m.ID); err != nil {
		t.Fatalf("set running: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	registry := gateway.NewRegistry()
	sessions.SetRevocationListener(registry)
	gw := gateway.NewProxy(origins, dialer, registry)

	svc := machine.NewService(pool, stubNodeClient{}, machine.NewBroker(), host.ID, machine.Spec{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"})
	srv := &httpapi.Server{Sessions: sessions, Machines: svc, Broker: machine.NewBroker(), Queries: q, Gateway: gw}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return cpFixture{
		url:         ts.URL,
		token:       token,
		machineID:   machine.UUIDString(m.ID),
		machinePgID: m.ID,
		sessions:    sessions,
		pool:        pool,
		q:           q,
		userID:      user.ID,
	}
}

// getTerminal issues a plain HTTP GET to /gw/terminal (no WebSocket upgrade), so
// pre-upgrade authz outcomes surface as ordinary status codes.
func getTerminal(t *testing.T, fx cpFixture, withCookie bool, origin, machineParam string) int {
	t.Helper()
	u := fx.url + "/gw/terminal"
	if machineParam != "" {
		u += "?machine=" + machineParam
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestGatewayAuthzTable(t *testing.T) {
	fx := setupCP(t, failDialer{t}, []string{testWSOrigin})
	ctx := context.Background()

	if code := getTerminal(t, fx, false, testWSOrigin, ""); code != http.StatusUnauthorized {
		t.Fatalf("no cookie: want 401, got %d", code)
	}
	if code := getTerminal(t, fx, true, "", ""); code != http.StatusForbidden {
		t.Fatalf("missing Origin: want 403, got %d", code)
	}
	if code := getTerminal(t, fx, true, "http://evil.example", ""); code != http.StatusForbidden {
		t.Fatalf("bad Origin: want 403, got %d", code)
	}
	if code := getTerminal(t, fx, true, testWSOrigin, "00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Fatalf("foreign machine: want 404, got %d", code)
	}

	// Not running ⇒ 409 (machine exists and is owned, but stopped).
	if _, err := fx.pool.Exec(ctx, "UPDATE machines SET state='stopped' WHERE id=$1", fx.machinePgID); err != nil {
		t.Fatal(err)
	}
	if code := getTerminal(t, fx, true, testWSOrigin, ""); code != http.StatusConflict {
		t.Fatalf("stopped machine: want 409, got %d", code)
	}
}
