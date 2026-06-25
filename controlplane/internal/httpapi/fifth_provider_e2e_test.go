package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/injector"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/nodeclient"
	"github.com/tavon-ai/proteos/controlplane/internal/providers"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// TestFifthProviderE2E is the executable proof of the master-plan Phase 6
// criterion: "a 5th provider = a registry entry + a launch template, no
// control-plane code change." It onboards brand-new providers entirely as DATA —
// a row inserted at runtime via SQL plus a launch script in the (dev) guest —
// and drives the full public chain (set key via the API → launch via
// /gw/agent/<key>) with ZERO provider-specific Go anywhere in this file or the
// server. If anyone hardcodes a provider key in the control plane, this test
// keeps passing for the hardcoded one but these arbitrary `stub*` providers
// would fail — so it documents the onboarding recipe by example.
//
// It also walks the Phase 6 matrix: two providers keyed on one machine are both
// launchable; a setup_command failure makes a provider degrade and its launch
// close 4003 with reason setup_failed; rotating the key re-runs setup and clears
// the degraded state.
func TestFifthProviderE2E(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available to build agents")
	}
	root := repoRoot(t)
	guestBin := buildBinary(t, filepath.Join(root, "guestagent"), "./cmd/guestagent")
	agentBin := buildBinary(t, filepath.Join(root, "nodeagent"), "./cmd/nodeagent")

	const agentToken = "e2e-fifth-token"
	nodeURL := startNodeAgent(t, agentBin, guestBin, agentToken)
	nodes := nodeclient.New(nodeURL, agentToken)

	fx := setupFifthCP(t, nodes)

	ctx := context.Background()
	if _, err := nodes.Ensure(ctx, fx.machineID, agentapi.EnsureRequest{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"}); err != nil {
		t.Fatalf("ensure on node-agent: %v", err)
	}
	t.Cleanup(func() { _ = nodes.Destroy(context.Background(), fx.machineID) })
	waitNodeRunning(t, nodes, fx.machineID)
	waitGuestReachable(t, nodes, fx.machineID)

	// A scratch dir on the host; the dev guest shares the host filesystem, so a
	// launch script / setup marker written here is reachable from the guest.
	work := t.TempDir()

	// launchScript writes an executable script that echoes one env var then idles
	// (so the attach catches the output), and returns its path.
	launchScript := func(name, envVar string) string {
		p := filepath.Join(work, name)
		body := "#!/bin/sh\necho \"" + envVar + "_SEEN=$" + envVar + "\"\nsleep 30\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// --- Provider 1: "stub" — happy path + setup_command marker -------------
	stubMarker := filepath.Join(work, "stub-setup-ran")
	insertProvider(t, fx, providerRow{
		key:     "stub",
		command: launchScript("stub.sh", "STUB_TOKEN"),
		// Setup proves it runs after the env file exists and sees the secret.
		setup:  "printf '%s' \"$STUB_TOKEN\" > " + stubMarker,
		fields: `[{"name":"token","label":"Stub token","env":"STUB_TOKEN"}]`,
	})
	setProviderKey(t, fx, "stub", map[string]string{"token": "sk-stub-111"})

	out := launchAgent(t, fx, "stub")
	if !strings.Contains(out, "STUB_TOKEN_SEEN=sk-stub-111") {
		t.Fatalf("stub did not see its injected key; got %q", out)
	}
	if got, err := os.ReadFile(stubMarker); err != nil || string(got) != "sk-stub-111" {
		t.Fatalf("setup_command marker = %q (err=%v); want the injected key", got, err)
	}

	// --- Provider 2: "plain" — second provider keyed on the same machine ----
	insertProvider(t, fx, providerRow{
		key:     "plain",
		command: launchScript("plain.sh", "PLAIN_TOKEN"),
		fields:  `[{"name":"token","label":"Plain token","env":"PLAIN_TOKEN"}]`,
	})
	setProviderKey(t, fx, "plain", map[string]string{"token": "sk-plain-222"})

	// Both are now launchable on one machine.
	if out := launchAgent(t, fx, "plain"); !strings.Contains(out, "PLAIN_TOKEN_SEEN=sk-plain-222") {
		t.Fatalf("plain provider not launchable; got %q", out)
	}
	if out := launchAgent(t, fx, "stub"); !strings.Contains(out, "STUB_TOKEN_SEEN=sk-stub-111") {
		t.Fatalf("stub no longer launchable after adding plain; got %q", out)
	}

	// --- Provider 3: "gate" — setup gated on the key value ------------------
	// Setup succeeds only when the key is "good"; this lets us drive the degraded
	// path with a bad key and then prove rotation re-runs setup.
	insertProvider(t, fx, providerRow{
		key:     "gate",
		command: launchScript("gate.sh", "GATE_TOKEN"),
		setup:   `test "$GATE_TOKEN" = "good"`,
		fields:  `[{"name":"token","label":"Gate token","env":"GATE_TOKEN"}]`,
	})

	// Bad key ⇒ setup fails ⇒ launch closes 4003 with reason setup_failed.
	setProviderKey(t, fx, "gate", map[string]string{"token": "bad"})
	code, reason := launchAgentClose(t, fx, "gate")
	if code != guestwire.CloseProviderUnavailable || reason != guestwire.CloseReasonSetupFailed {
		t.Fatalf("degraded launch: code=%d reason=%q, want %d/%q",
			code, reason, guestwire.CloseProviderUnavailable, guestwire.CloseReasonSetupFailed)
	}

	// Rotate the key ⇒ the next launch re-pushes, re-runs setup (now passing) and
	// the provider becomes launchable — degraded cleared by a successful re-push.
	setProviderKey(t, fx, "gate", map[string]string{"token": "good"})
	if out := launchAgent(t, fx, "gate"); !strings.Contains(out, "GATE_TOKEN_SEEN=good") {
		t.Fatalf("gate not launchable after key rotation; got %q", out)
	}
}

// fifthFixture is the wired control plane for the fifth-provider e2e. rawExec
// inserts registry rows directly — onboarding a provider is data, not code.
type fifthFixture struct {
	url       string
	token     string
	machineID string
	pool      *pgxpool.Pool
	rawExec   func(ctx context.Context, sql string, args ...any)
}

// setupFifthCP seeds a user/host/running-machine and serves a control plane with
// the full Phase 5/6 provider stack (registry + secrets + injector + audit) and
// the agent gateway wired to the real node-agent tunnel.
func setupFifthCP(t *testing.T, nodes *nodeclient.Client) fifthFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 6006, Login: "fifth-user", Email: "f@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: nodes.BaseURL()})
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
	gw := gateway.NewProxy([]string{testWSOrigin}, nodes, registry)

	svc := machine.NewService(pool, stubNodeClient{}, machine.NewBroker(), secrets.NewMemStore(), host.ID,
		machine.Spec{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"})

	reg := providers.NewRegistry(q)
	sec := secrets.NewMemStore()
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
		Injector:  injector.New(nodes, reg, sec, rec, nil),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	fx := fifthFixture{url: ts.URL, token: token, machineID: machine.UUIDString(m.ID), pool: pool}
	fx.rawExec = func(ctx context.Context, sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	return fx
}

// providerRow is a registry row to insert at runtime — the "data only" half of
// onboarding a provider.
type providerRow struct {
	key, command, setup, fields string
}

func insertProvider(t *testing.T, fx fifthFixture, row providerRow) {
	t.Helper()
	var setup any
	if row.setup != "" {
		setup = row.setup
	}
	// Upsert (so a leftover row from an earlier interrupted run can't dup-error)
	// and delete on cleanup, restoring the shared providers table — testutil does
	// not truncate it (see its doc).
	fx.rawExec(context.Background(),
		`INSERT INTO providers (key, display_name, launch_command, setup_command, secret_fields, enabled)
		 VALUES ($1, $1, $2, $3, $4::jsonb, true)
		 ON CONFLICT (key) DO UPDATE SET launch_command=EXCLUDED.launch_command,
		   setup_command=EXCLUDED.setup_command, secret_fields=EXCLUDED.secret_fields, enabled=true`,
		row.key, row.command, setup, row.fields)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), "DELETE FROM providers WHERE key=$1", row.key)
	})
}

// setProviderKey sets a provider's secret fields through the public write-only
// API (the same call the React settings panel makes).
func setProviderKey(t *testing.T, fx fifthFixture, key string, fields map[string]string) {
	t.Helper()
	body := `{"fields":{`
	first := true
	for k, v := range fields {
		if !first {
			body += ","
		}
		body += `"` + k + `":"` + v + `"`
		first = false
	}
	body += "}}"

	req, _ := http.NewRequest(http.MethodPut, fx.url+"/api/secrets/providers/"+key, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	req.Header.Set("X-Requested-By", "proteos")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("set key %s: %v", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set key %s: status %d, want 204", key, resp.StatusCode)
	}
}

// dialAgent opens the browser-side agent WebSocket for a provider.
func dialAgent(t *testing.T, fx fifthFixture, provider string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Origin", testWSOrigin)
	hdr.Set("Cookie", auth.SessionCookieName+"="+fx.token)
	u := "ws" + strings.TrimPrefix(fx.url, "http") + "/gw/agent/" + provider + "?machine=" + fx.machineID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial agent %s: %v", provider, err)
	}
	c.SetReadLimit(8 << 20)
	return c
}

// launchAgent launches a provider and returns the PTY output it produced until
// the launch script's marker line appears.
func launchAgent(t *testing.T, fx fifthFixture, provider string) string {
	t.Helper()
	c := dialAgent(t, fx, provider)
	defer c.Close(websocket.StatusNormalClosure, "")
	if f := readControlFrame(t, c); f.Type != guestwire.FrameHello {
		t.Fatalf("%s: first frame = %q, want hello", provider, f.Type)
	}
	return readBinaryUntil(t, c, "_SEEN=", 10*time.Second)
}

// launchAgentClose launches a provider expected to be rejected post-upgrade and
// returns the WebSocket close code + reason.
func launchAgentClose(t *testing.T, fx fifthFixture, provider string) (websocket.StatusCode, string) {
	t.Helper()
	c := dialAgent(t, fx, provider)
	defer c.Close(websocket.StatusInternalError, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("%s: expected a close error, got %v", provider, err)
			}
			return ce.Code, ce.Reason
		}
	}
}
