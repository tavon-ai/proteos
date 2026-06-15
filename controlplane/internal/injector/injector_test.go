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

	// A secret.read row exists for this user's claude path, with the injector as
	// actor and the path (never the value) as target. Scoped by actor+action+
	// target so a shared CI DB with leaked rows from other tests can't confuse it.
	var reads int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE actor=$1 AND action=$2 AND target=$3",
		audit.ActorSystemInjector, audit.ActionSecretRead, secrets.UserProviderPath(uid, "claude"),
	).Scan(&reads); err != nil {
		t.Fatal(err)
	}
	if reads == 0 {
		t.Fatal("no secret.read audit row for this user's claude path")
	}
	// And no audit row ever carries the secret value in its target.
	var leaks int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE target LIKE '%sk-inject-42%'",
	).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatalf("audit target leaked the key value (%d rows)", leaks)
	}
}

// TestInjectorComposesMultiFieldSetupProvider proves the injector builds the
// guest ProviderDef for a multi-field provider that carries a setup_command:
// each declared field maps to its env var, and the setup command rides along in
// the push (Phase 6 — composed from data, no per-provider code).
func TestInjectorComposesMultiFieldSetupProvider(t *testing.T) {
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	// Insert a custom provider row directly — onboarding a provider is data only.
	// Upsert + cleanup so the shared (CI) providers table is restored afterwards
	// (testutil.Postgres does not truncate providers — see its doc).
	const setup = "printenv OPENAI_API_KEY | codex login --with-api-key"
	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (key, display_name, launch_command, setup_command, secret_fields, enabled)
		 VALUES ('injstub','Inj Stub','stub',$1,$2::jsonb,true)
		 ON CONFLICT (key) DO UPDATE SET launch_command=EXCLUDED.launch_command,
		   setup_command=EXCLUDED.setup_command, secret_fields=EXCLUDED.secret_fields, enabled=true`,
		setup,
		`[{"name":"client_id","label":"Client ID","env":"STUB_CLIENT_ID"},
		  {"name":"client_secret","label":"Client secret","env":"STUB_CLIENT_SECRET"}]`,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM providers WHERE key='injstub'") })

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 101, Login: "multi"})
	if err != nil {
		t.Fatal(err)
	}
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	if err := sec.Put(secrets.UserProviderPath(uid, "injstub"), map[string]string{
		"client_id":     "id-42",
		"client_secret": "shh-99",
	}); err != nil {
		t.Fatal(err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, audit.NewRecorder(q))
	if err := inj.Inject(ctx, uid, "m-multi"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	def, ok := guest.last.Providers["injstub"]
	if !ok {
		t.Fatalf("injstub not pushed: %v", guest.last.Providers)
	}
	if def.Command != "stub" {
		t.Fatalf("command = %q, want stub", def.Command)
	}
	if def.SetupCommand != setup {
		t.Fatalf("setup_command = %q, want %q", def.SetupCommand, setup)
	}
	if def.Env["STUB_CLIENT_ID"] != "id-42" || def.Env["STUB_CLIENT_SECRET"] != "shh-99" {
		t.Fatalf("env composition wrong: %v", def.Env)
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

