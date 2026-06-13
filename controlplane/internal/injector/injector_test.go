package injector_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/injector"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// fakeGuest records the last PUT /secrets payload, standing in for the guest
// agent's HTTP surface across the tunnel (controlplane cannot import the guest's
// internal server package).
type fakeGuest struct {
	mu   sync.Mutex
	last guestwire.SecretsRequest
	got  bool
}

func (g *fakeGuest) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(guestwire.RouteSecrets, func(w http.ResponseWriter, r *http.Request) {
		var req guestwire.SecretsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.last = req
		g.got = true
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// pipeDialer returns one end of a net.Pipe and serves the guest handler on the
// other, so the injector's real HTTP client speaks to a real HTTP server with no
// sockets involved.
type pipeDialer struct{ h http.Handler }

func (d pipeDialer) DialGuest(_ context.Context, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	ch := make(chan net.Conn, 1)
	ch <- server
	go (&http.Server{Handler: d.h}).Serve(&oneConnListener{ch: ch})
	return client, nil
}

// oneConnListener is a net.Listener that yields a single conn then blocks until
// closed.
type oneConnListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.closed = make(chan struct{}) })
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *oneConnListener) Close() error {
	l.once.Do(func() { l.closed = make(chan struct{}) })
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}
func (l *oneConnListener) Addr() net.Addr { return fakeAddr{} }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "pipe" }
func (fakeAddr) String() string  { return "pipe" }

func TestInjectorPushesComposedSecrets(t *testing.T) {
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 99, Login: "inj"})
	if err != nil {
		t.Fatal(err)
	}
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	if err := sec.Put(secrets.UserProviderPath(uid, "claude"), map[string]string{"api_key": "sk-inject-42"}); err != nil {
		t.Fatal(err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, audit.NewRecorder(q))

	if err := inj.Inject(ctx, uid, "machine-1"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	if !guest.got {
		t.Fatal("guest never received PUT /secrets")
	}
	def, ok := guest.last.Providers["claude"]
	if !ok {
		t.Fatalf("claude not in pushed providers: %v", guest.last.Providers)
	}
	if def.Command != "claude" {
		t.Fatalf("command = %q, want claude", def.Command)
	}
	if def.Env["ANTHROPIC_API_KEY"] != "sk-inject-42" {
		t.Fatalf("env mapping wrong: %v", def.Env)
	}

	// An audit secret.read row exists with the path (not the value) as target.
	rows, err := pool.Query(ctx, "SELECT actor, action, target FROM audit_log")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sawRead bool
	for rows.Next() {
		var actor, action, target string
		_ = rows.Scan(&actor, &action, &target)
		if action == audit.ActionSecretRead {
			sawRead = true
			if actor != audit.ActorSystemInjector {
				t.Fatalf("read actor = %q, want %q", actor, audit.ActorSystemInjector)
			}
			if target != secrets.UserProviderPath(uid, "claude") {
				t.Fatalf("read target = %q, want the path", target)
			}
		}
	}
	if !sawRead {
		t.Fatal("no secret.read audit row")
	}
}

func TestInjectorPushesEmptyWhenNoKeys(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 100, Login: "empty"})
	uid := machine.UUIDString(user.ID)

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), secrets.NewMemStore(), audit.NewRecorder(q))

	if err := inj.Inject(ctx, uid, "m"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	guest.mu.Lock()
	defer guest.mu.Unlock()
	if !guest.got || len(guest.last.Providers) != 0 {
		t.Fatalf("expected empty replace-all push, got %v", guest.last.Providers)
	}
}

