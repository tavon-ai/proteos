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

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/injector"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// errDialer fails the guest tunnel dial with an ordinary error (unlike
// failDialer, which fails the test). Used to prove the injector was reached.
type errDialer struct{}

func (errDialer) DialGuest(context.Context, string, uint32) (net.Conn, error) {
	return nil, errors.New("errDialer: no guest")
}

type agentFixture struct {
	url    string
	token  string
	userID pgtype.UUID
	sec    *secrets.MemStore
}

// setupAgent wires a control plane with the providers API + agent gateway route
// and a running machine for the seeded user. The gateway uses failDialer, so any
// negative path that reaches a tunnel dial fails the test loudly.
func setupAgent(t *testing.T) agentFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 555, Login: "agent-user"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", m.ID); err != nil {
		t.Fatal(err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	registry := gateway.NewRegistry()
	sessions.SetRevocationListener(registry)
	gw := gateway.NewProxy([]string{testWSOrigin}, failDialer{t}, registry)

	svc := machine.NewService(pool, stubNodeClient{}, machine.NewBroker(), secrets.NewMemStore(), host.ID,
		machine.Spec{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"})
	sec := secrets.NewMemStore()

	reg := providers.NewRegistry(q)
	rec := audit.NewRecorder(q)
	srv := &httpapi.Server{
		Sessions:  sessions,
		Machines:  svc,
		Broker:    machine.NewBroker(),
		Queries:   q,
		Gateway:   gw,
		Providers: reg,
		Secrets:   sec,
		Audit:     rec,
		Injector:  injector.New(errDialer{}, reg, sec, rec, nil),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return agentFixture{url: ts.URL, token: token, userID: user.ID, sec: sec}
}

// getAgent issues a plain GET /gw/agent/{provider} (no WS upgrade) so the
// pre-upgrade authz/provider outcomes surface as status codes.
func getAgent(t *testing.T, fx agentFixture, provider string, withCookie bool, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/gw/agent/"+provider, nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAgentUnauthenticated401(t *testing.T) {
	fx := setupAgent(t)
	if code := getAgent(t, fx, "claude", false, testWSOrigin); code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

func TestAgentBadOrigin403(t *testing.T) {
	fx := setupAgent(t)
	if code := getAgent(t, fx, "claude", true, "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", code)
	}
}

func TestAgentUnknownProvider404(t *testing.T) {
	fx := setupAgent(t)
	if code := getAgent(t, fx, "bogus", true, testWSOrigin); code != http.StatusNotFound {
		t.Fatalf("want 404 unknown_provider, got %d", code)
	}
}

func TestAgentNoProviderKey409(t *testing.T) {
	fx := setupAgent(t)
	// gemini is registered+enabled and the machine is running, but no key is set —
	// and gemini needs a key (unlike Claude, it cannot run on a subscription).
	if code := getAgent(t, fx, "gemini", true, testWSOrigin); code != http.StatusConflict {
		t.Fatalf("want 409 no_provider_key, got %d", code)
	}
}

// TestAgentClaudeNoKeyReachesInjection proves Claude is exempt from the stored-key
// requirement (subscription auth): with no key the handler proceeds past the 409
// to the secret push, which fails on failDialer — surfacing as 502, the same
// outcome as the key-set case and proof the key gate was skipped.
func TestAgentClaudeNoKeyReachesInjection(t *testing.T) {
	fx := setupAgent(t)
	if code := getAgent(t, fx, "claude", true, testWSOrigin); code != http.StatusBadGateway {
		t.Fatalf("want 502 injection_failed (key gate skipped), got %d", code)
	}
}

func TestAgentKeySetReachesInjection(t *testing.T) {
	fx := setupAgent(t)
	// With a key set, the handler proceeds past the 409 to the secret push, which
	// fails because failDialer refuses to dial — surfacing as 502 injection_failed
	// (proving the key check passed and the injector was invoked).
	if err := fx.sec.Put(secrets.UserProviderPath(machine.UUIDString(fx.userID), "claude"),
		map[string]string{"api_key": "sk-x"}); err != nil {
		t.Fatal(err)
	}
	if code := getAgent(t, fx, "claude", true, testWSOrigin); code != http.StatusBadGateway {
		t.Fatalf("want 502 injection_failed, got %d", code)
	}
}
