package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/nodeclient"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// TestHibernateResumeE2E exercises the whole Phase 4 dev stack with no
// hypervisor: a real control-plane lifecycle (Service + poller) drives a real
// node-agent (DevDriver) which launches a real guest agent with a persist dir.
// It writes a file under $HOME, hibernates (stop), resumes (start), and proves
// the file survives — plus that the snapshot metadata round-trips and the API
// summary reports boot:resumed.
func TestHibernateResumeE2E(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available to build agents")
	}
	root := repoRoot(t)
	guestBin := buildBinary(t, filepath.Join(root, "guestagent"), "./cmd/guestagent")
	agentBin := buildBinary(t, filepath.Join(root, "nodeagent"), "./cmd/nodeagent")

	const agentToken = "p4-e2e-token"
	nodeURL := startNodeAgent(t, agentBin, guestBin, agentToken)
	nodes := nodeclient.New(nodeURL, agentToken)

	fx, svc := setupCPLifecycle(t, nodes, []string{testWSOrigin})
	ctx := context.Background()

	// Create drives the real lifecycle: disk + volume key minted, ensure on the
	// agent, poller advances to running.
	m, err := svc.Create(ctx, fx.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fx.machineID = machine.UUIDString(m.ID)
	fx.machinePgID = m.ID
	t.Cleanup(func() { _ = nodes.Destroy(context.Background(), fx.machineID) })
	waitCPState(t, fx, machine.StateRunning)
	waitGuestReachable(t, nodes, fx.machineID)

	// Write a file under $HOME (on the persist dir). Wait for a runtime-computed
	// token (not a literal substring of the typed command, which the PTY echoes
	// back) so the match proves the write actually completed.
	const marker = "proteos-persist-marker-4242"
	runInTerminal(t, fx, "w1", "echo "+marker+" > $HOME/proof.txt; echo done-$((21*2))", "done-42")

	// Stop = hibernate. Wait for stopped, then assert the snapshot metadata
	// round-tripped into the API summary.
	if _, err := svc.Stop(ctx, fx.userID, fx.machinePgID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitCPState(t, fx, machine.StateStopped)
	sum := getSummary(t, fx)
	if sum.Snapshot == nil || sum.Snapshot.FCVersion != "dev" {
		t.Fatalf("after hibernate, summary snapshot = %+v, want fc_version dev", sum.Snapshot)
	}

	// Start = resume. Wait for running; the new guest agent rebinds the socket.
	if _, err := svc.Start(ctx, fx.userID, fx.machinePgID); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitCPState(t, fx, machine.StateRunning)
	waitGuestReachable(t, nodes, fx.machineID)

	// The API summary now reports a resumed boot and no current snapshot.
	sum = getSummary(t, fx)
	if sum.Boot == nil || *sum.Boot != agentapi.BootResumed {
		t.Fatalf("after resume, summary boot = %v, want resumed", sum.Boot)
	}
	if sum.Snapshot != nil {
		t.Fatalf("after resume, snapshot should be consumed, got %+v", sum.Snapshot)
	}
	if sum.DiskMiB == nil || *sum.DiskMiB == 0 {
		t.Fatalf("summary missing disk size: %+v", sum.DiskMiB)
	}

	// The file written before hibernate is still there after resume (the PTY
	// session is NOT — dev limitation — but the persist dir survives). The marker
	// is not in the `cat` command, so matching it proves the file content.
	runInTerminal(t, fx, "r1", "cat $HOME/proof.txt", marker)

	// The lifecycle recorded the hibernating and starting transitions (these are
	// what the SSE stream surfaces; see TestSSEStreamsTransitionsInOrder).
	assertEventStates(t, fx, "hibernating", "starting")
}

// setupCPLifecycle wires a control plane whose machine.Service drives the REAL
// node-agent (unlike setupCP, which stubs it), runs a fast poller, and returns
// the fixture plus the service. The gateway uses the same nodes client as dialer.
func setupCPLifecycle(t *testing.T, nodes *nodeclient.Client, origins []string) (cpFixture, *machine.Service) {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 909, Login: "p4", Email: "p4@x", AvatarUrl: ""})
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: nodes.BaseURL()})
	if err != nil {
		t.Fatal(err)
	}

	registry := gateway.NewRegistry()
	sessions := session.NewManager(q, time.Hour)
	sessions.SetRevocationListener(registry)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	broker := machine.NewBroker()
	svc := machine.NewService(pool, nodes, broker, secrets.NewMemStore(), machine.Spec{
		Vcpus: 1, MemMiB: 128, DiskMiB: 1024, KernelRef: "k", RootfsRef: "r",
	})
	poller := machine.NewPoller(pool, nodes, broker)

	// Fast reconciliation loop (the production poller's 2s tick is too slow for a
	// snappy test). Stops on cleanup.
	pollCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-tk.C:
				poller.AdvanceTransitional(pollCtx)
			}
		}
	}()

	gw := gateway.NewProxy(origins, nodes, registry)
	srv := &httpapi.Server{Sessions: sessions, Machines: svc, Broker: broker, Queries: q, Gateway: gw}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// machineID is filled in after Create (the test creates the machine), so look
	// it up lazily via a closure-friendly fixture: we resolve it on first need.
	fx := cpFixture{url: ts.URL, token: token, sessions: sessions, pool: pool, q: q, userID: user.ID}
	return fx, svc
}

// waitCPState polls the user's machine row until it reaches want or times out.
func waitCPState(t *testing.T, fx cpFixture, want machine.State) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		m, err := fx.q.GetMachineByUserID(context.Background(), fx.userID)
		if err == nil {
			last = m.State
			// Cache the machine id on the fixture's machineID via the test's
			// expectations: resolve it here so terminal/summary helpers can use it.
			if m.State == string(want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("machine never reached %q (last=%q)", want, last)
}

// machineUUID resolves the user's machine id (canonical) from the DB.
func machineUUID(t *testing.T, fx cpFixture) (string, pgtype.UUID) {
	t.Helper()
	m, err := fx.q.GetMachineByUserID(context.Background(), fx.userID)
	if err != nil {
		t.Fatalf("resolve machine: %v", err)
	}
	return machine.UUIDString(m.ID), m.ID
}

// runInTerminal opens the named terminal session, sends cmd, and waits until
// want appears in the output. Each test step uses a distinct session name so a
// dead (post-hibernate) session is never reused.
func runInTerminal(t *testing.T, fx cpFixture, session, cmd, want string) {
	t.Helper()
	c := dialBrowser(t, fx, session)
	defer c.Close(websocket.StatusNormalClosure, "")
	if f := readControlFrame(t, c); f.Type != guestwire.FrameHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	writeBinary(t, c, cmd+"\n")
	if got := readBinaryUntil(t, c, want, 8*time.Second); !strings.Contains(got, want) {
		t.Fatalf("terminal: did not see %q; got %q", want, got)
	}
}

// getSummary fetches the user's machine summary from /api/machines/{id}.
func getSummary(t *testing.T, fx cpFixture) httpapi.MachineSummary {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/api/machines/"+fx.machineID, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get summary: status %d", resp.StatusCode)
	}
	var sum httpapi.MachineSummary
	if err := json.NewDecoder(resp.Body).Decode(&sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return sum
}

// assertEventStates fails unless every wanted to_state appears in the machine's
// event log.
func assertEventStates(t *testing.T, fx cpFixture, wantStates ...string) {
	t.Helper()
	_, id := machineUUID(t, fx)
	evs, err := fx.q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: id, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.ToState != nil {
			seen[*e.ToState] = true
		}
	}
	for _, w := range wantStates {
		if !seen[w] {
			t.Fatalf("event log missing to_state %q (seen=%v)", w, seen)
		}
	}
}
