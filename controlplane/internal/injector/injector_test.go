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
	"github.com/tavon/proteos/controlplane/internal/profile"
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

func (d pipeDialer) DialGuest(_ context.Context, _ string, _ uint32) (net.Conn, error) {
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
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, audit.NewRecorder(q), nil)

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
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, audit.NewRecorder(q), nil)
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

// TestInjectorMergesClaudeProfileToken proves the portable-profile path: an
// env-kind item tied to the claude provider (the subscription OAuth token) is
// merged into the claude ProviderDef's env in the keyless branch — so it reaches
// both login shells and agent-launched sessions — and no empty ANTHROPIC_API_KEY
// is emitted alongside it (the plan's precedence constraint).
func TestInjectorMergesClaudeProfileToken(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 102, Login: "subtok"})
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	prof := profile.NewStore(q, sec, rec)
	def, ok := profile.Lookup(profile.ClaudeOAuthKey)
	if !ok {
		t.Fatal("claude-oauth not a registered profile item")
	}
	if err := prof.Set(ctx, uid, def, "oauth-tok-xyz"); err != nil {
		t.Fatalf("set profile item: %v", err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, rec, prof)
	if err := inj.Inject(ctx, uid, "m-tok"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	cdef, ok := guest.last.Providers["claude"]
	if !ok {
		t.Fatalf("claude not pushed: %v", guest.last.Providers)
	}
	if cdef.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "oauth-tok-xyz" {
		t.Fatalf("oauth token not merged into claude env: %v", cdef.Env)
	}
	if _, present := cdef.Env["ANTHROPIC_API_KEY"]; present {
		t.Fatalf("ANTHROPIC_API_KEY must not be emitted alongside the OAuth token: %v", cdef.Env)
	}
}

// TestInjectorEmitsFileItems proves a file-kind profile item is composed into the
// pushed SecretsRequest.Files with its $HOME-relative path, mode, and content, so
// the guest materializes it. Env-kind items are unaffected.
func TestInjectorEmitsFileItems(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 104, Login: "fileitem"})
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	prof := profile.NewStore(q, sec, rec)
	def, err := profile.FileDef("gitconfig", ".gitconfig", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if err := prof.Set(ctx, uid, def, "[user]\n\tname = Ada\n"); err != nil {
		t.Fatalf("set file item: %v", err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, rec, prof)
	if err := inj.Inject(ctx, uid, "m-file"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	if len(guest.last.Files) != 1 {
		t.Fatalf("pushed files = %v, want one", guest.last.Files)
	}
	f := guest.last.Files[0]
	if f.Path != ".gitconfig" || f.Mode != 0o640 || f.Content != "[user]\n\tname = Ada\n" {
		t.Fatalf("file def wrong: %+v", f)
	}
}

// TestInjectorEmitsSSHKeyFiles proves an SSH key set via the typed store is
// composed into SecretsRequest.Files: the private key at ~/.ssh/id_ed25519 (0600)
// and the SSH client config, so the guest materializes them under ~/.ssh.
func TestInjectorEmitsSSHKeyFiles(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 105, Login: "sshuser"})
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	prof := profile.NewStore(q, sec, rec)
	priv, pub, _, err := profile.GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := prof.SetSSHKey(ctx, uid, priv, pub); err != nil {
		t.Fatalf("set ssh key: %v", err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, rec, prof)
	if err := inj.Inject(ctx, uid, "m-ssh"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	byPath := map[string]struct {
		mode    uint32
		content string
	}{}
	for _, f := range guest.last.Files {
		byPath[f.Path] = struct {
			mode    uint32
			content string
		}{f.Mode, f.Content}
	}
	key, ok := byPath[".ssh/id_ed25519"]
	if !ok {
		t.Fatalf("private key file not pushed: %v", guest.last.Files)
	}
	if key.mode != 0o600 || key.content != priv {
		t.Fatalf("private key file wrong: mode=%o content-is-priv=%v", key.mode, key.content == priv)
	}
	if _, ok := byPath[".ssh/config"]; !ok {
		t.Fatalf("ssh config file not pushed: %v", guest.last.Files)
	}
}

// TestInjectorPrefersStoredApiKeyOverProfileToken proves the precedence guard:
// when a user has BOTH a stored claude API key and a profile OAuth token, the
// injector emits the API key (ANTHROPIC_API_KEY outranks the subscription token)
// and does NOT leak the OAuth token into the provider env — the token belongs
// only to the keyless/subscription branch.
func TestInjectorPrefersStoredApiKeyOverProfileToken(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 103, Login: "bothauth"})
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	if err := sec.Put(secrets.UserProviderPath(uid, "claude"), map[string]string{"api_key": "sk-real-key"}); err != nil {
		t.Fatal(err)
	}
	prof := profile.NewStore(q, sec, rec)
	def, _ := profile.Lookup(profile.ClaudeOAuthKey)
	if err := prof.Set(ctx, uid, def, "oauth-should-not-leak"); err != nil {
		t.Fatal(err)
	}

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), sec, rec, prof)
	if err := inj.Inject(ctx, uid, "m-both"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	guest.mu.Lock()
	defer guest.mu.Unlock()
	cdef := guest.last.Providers["claude"]
	if cdef.Env["ANTHROPIC_API_KEY"] != "sk-real-key" {
		t.Fatalf("stored API key not emitted: %v", cdef.Env)
	}
	if _, present := cdef.Env["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Fatalf("OAuth token leaked into the keyed branch: %v", cdef.Env)
	}
}

// TestInjectorPushesSubscriptionProviderWhenNoKeys proves that a user with no
// stored keys still gets the subscription-capable provider (Claude Code) pushed
// with an empty env — so the guest can launch claude on the image's own login —
// while every key-requiring provider (gemini/openai/pi) is omitted.
func TestInjectorPushesSubscriptionProviderWhenNoKeys(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 100, Login: "empty"})
	uid := machine.UUIDString(user.ID)

	guest := &fakeGuest{}
	inj := injector.New(pipeDialer{h: guest.handler()}, providers.NewRegistry(q), secrets.NewMemStore(), audit.NewRecorder(q), nil)

	if err := inj.Inject(ctx, uid, "m"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	guest.mu.Lock()
	defer guest.mu.Unlock()
	if !guest.got {
		t.Fatal("guest never received PUT /secrets")
	}
	def, ok := guest.last.Providers["claude"]
	if !ok {
		t.Fatalf("claude not pushed for keyless user: %v", guest.last.Providers)
	}
	if def.Command != "claude" || len(def.Env) != 0 {
		t.Fatalf("expected keyless claude def, got command=%q env=%v", def.Command, def.Env)
	}
	// Providers that require a key must not appear when the user has none.
	for _, k := range []string{"gemini", "openai", "pi"} {
		if _, present := guest.last.Providers[k]; present {
			t.Fatalf("key-requiring provider %q pushed without a key", k)
		}
	}
}
