package machine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// fakeAgent is an in-process stand-in for the node-agent. Tests drive its
// reported status directly (SetStatus) so the poller's reconciliation can be
// exercised deterministically without sleeping.
type fakeAgent struct {
	mu           sync.Mutex
	status       map[string]agentapi.MachineStatus
	failEnsure   bool
	ensureCalls  int
	stopCalls    int
	destroyCalls int
	lastEnsure   map[string]agentapi.EnsureRequest // captured per id (incl. volume key)
	stopModes    []string                          // modes seen on stop calls
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{status: map[string]agentapi.MachineStatus{}, lastEnsure: map[string]agentapi.EnsureRequest{}}
}

func (f *fakeAgent) SetStatus(id, state, reason, guestIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Preserve any snapshot/boot already set for this id.
	prev := f.status[id]
	f.status[id] = agentapi.MachineStatus{
		MachineID: id, State: state, Reason: reason, GuestIP: guestIP, Handle: "fc-" + id[:8],
		Boot: prev.Boot, Snapshot: prev.Snapshot,
	}
}

// SetSnapshot sets the snapshot metadata the agent reports for id (Phase 4).
func (f *fakeAgent) SetSnapshot(id string, present bool, fcVersion string, memBytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status[id]
	st.Snapshot = agentapi.SnapshotInfo{Present: present, FCVersion: fcVersion, MemBytes: memBytes, CreatedAt: "2026-06-11T00:00:00Z"}
	f.status[id] = st
}

// SetBoot sets how the agent reports the machine started (cold|resumed).
func (f *fakeAgent) SetBoot(id, boot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status[id]
	st.Boot = boot
	f.status[id] = st
}

func (f *fakeAgent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(agentapi.RouteEnsure, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req agentapi.EnsureRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.ensureCalls++
		f.lastEnsure[id] = req
		fail := f.failEnsure
		if !fail {
			// A real boot starts in creating; tests advance it with SetStatus.
			if _, ok := f.status[id]; !ok {
				f.status[id] = agentapi.MachineStatus{MachineID: id, State: agentapi.StateCreating, Handle: "fc-" + id[:8]}
			}
		}
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: agentapi.ErrInternal})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(agentapi.EnsureResponse{Handle: "fc-" + id[:8]})
	})
	mux.HandleFunc(agentapi.RouteStop, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req agentapi.StopRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.stopCalls++
		f.stopModes = append(f.stopModes, req.Mode)
		if st, ok := f.status[id]; ok {
			st.State = agentapi.StateStopping
			f.status[id] = st
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc(agentapi.RouteGetMachine, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		st, ok := f.status[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(agentapi.ErrorResponse{Error: agentapi.ErrUnknownMachine})
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc(agentapi.RouteDestroy, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.destroyCalls++
		delete(f.status, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// harness bundles a migrated DB, a fake agent, and a wired service+poller.
type harness struct {
	q      *store.Queries
	svc    *machine.Service
	poller *machine.Poller
	agent  *fakeAgent
	srv    *httptest.Server
	sec    *secrets.MemStore
	userID pgtype.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool, q := testutil.Postgres(t)
	ctx := context.Background()

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 7, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "h", AgentUrl: "http://x"})
	if err != nil {
		t.Fatal(err)
	}

	agent := newFakeAgent()
	srv := httptest.NewServer(agent.handler())
	t.Cleanup(srv.Close)

	nc := nodeclient.New(srv.URL, "tok")
	broker := machine.NewBroker()
	sec := secrets.NewMemStore()
	svc := machine.NewService(pool, nc, broker, sec, host.ID, machine.Spec{
		Vcpus: 2, MemMiB: 2048, DiskMiB: 10240, KernelRef: "k1", RootfsRef: "r1",
	})
	poller := machine.NewPoller(pool, nc, broker)
	return &harness{q: q, svc: svc, poller: poller, agent: agent, srv: srv, sec: sec, userID: user.ID}
}

func (h *harness) machine(t *testing.T) store.Machine {
	t.Helper()
	m, err := h.svc.Get(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	return m
}

func (h *harness) eventStates(t *testing.T, id pgtype.UUID) []string {
	t.Helper()
	evs, err := h.q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, e := range evs {
		to := ""
		if e.ToState != nil {
			to = *e.ToState
		}
		seq = append(seq, to)
	}
	return seq
}

func TestFullLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Create → provisioning, ensure called, handle recorded.
	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.State != string(machine.StateProvisioning) {
		t.Fatalf("after create state=%q, want provisioning", m.State)
	}
	if m.VmHandle == nil || *m.VmHandle == "" {
		t.Fatalf("handle not recorded after create")
	}
	id := m.ID
	idStr := machine.UUIDString(id)

	// Agent reports running with a guest IP; poller advances provisioning→running.
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	m = h.machine(t)
	if m.State != string(machine.StateRunning) {
		t.Fatalf("after poll state=%q, want running", m.State)
	}
	if m.GuestIp == nil || m.GuestIp.String() != "172.30.0.2" {
		t.Fatalf("guest_ip not recorded: %v", m.GuestIp)
	}

	// Stop → stopping, agent stop called; agent reports stopped; poller → stopped.
	if _, err := h.svc.Stop(ctx, h.userID, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if h.agent.stopCalls != 1 {
		t.Fatalf("stop not called on agent (calls=%d)", h.agent.stopCalls)
	}
	h.agent.SetStatus(idStr, agentapi.StateStopped, "", "")
	h.poller.AdvanceTransitional(ctx)
	if got := h.machine(t).State; got != string(machine.StateStopped) {
		t.Fatalf("after stop poll state=%q, want stopped", got)
	}

	// Start from stopped → starting → running again.
	if _, err := h.svc.Start(ctx, h.userID, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := h.machine(t).State; got != string(machine.StateStarting) {
		t.Fatalf("after start state=%q, want starting", got)
	}
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	if got := h.machine(t).State; got != string(machine.StateRunning) {
		t.Fatalf("after restart poll state=%q, want running", got)
	}

	// Every transition wrote an event row, in order. Stop hibernates (decision
	// #4), so the cold-stop "stopping" state is replaced by "hibernating".
	want := []string{"provisioning", "running", "hibernating", "stopped", "starting", "running"}
	got := h.eventStates(t, id)
	if len(got) != len(want) {
		t.Fatalf("event sequence=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestBootFailureMovesToError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	idStr := machine.UUIDString(m.ID)

	// Agent reports the boot failed.
	h.agent.SetStatus(idStr, agentapi.StateError, "boot failed (dev:fail-boot)", "")
	h.poller.AdvanceTransitional(ctx)

	m = h.machine(t)
	if m.State != string(machine.StateError) {
		t.Fatalf("state=%q, want error", m.State)
	}
	if m.LastError == nil || *m.LastError == "" {
		t.Fatalf("last_error not set on boot failure")
	}
}

func TestAgentUnreachableDuringPollMovesToError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Agent goes away before the poller can observe the boot completing.
	h.srv.Close()
	h.poller.AdvanceTransitional(ctx)

	m := h.machine(t)
	if m.State != string(machine.StateError) {
		t.Fatalf("state=%q, want error after agent unreachable", m.State)
	}
	if m.LastError == nil {
		t.Fatalf("last_error not set on unreachable agent")
	}
}

func TestEnsureFailureAtCreateMovesToError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.agent.failEnsure = true

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create returned hard error: %v", err)
	}
	if m.State != string(machine.StateError) {
		t.Fatalf("state=%q, want error when agent ensure fails", m.State)
	}
}

func TestInvalidStateTransitions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Stop a machine that does not exist → ErrNoMachine.
	if _, err := h.svc.Stop(ctx, h.userID, nonexistentID(t)); err != machine.ErrNoMachine {
		t.Fatalf("stop nonexistent: got %v, want ErrNoMachine", err)
	}

	// Create one (provisioning).
	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Start while provisioning (not stopped/error) → ErrInvalidState.
	if _, err := h.svc.Start(ctx, h.userID, m.ID); err != machine.ErrInvalidState {
		t.Fatalf("start while provisioning: got %v, want ErrInvalidState", err)
	}
	// Stop while provisioning (not running) → ErrInvalidState.
	if _, err := h.svc.Stop(ctx, h.userID, m.ID); err != machine.ErrInvalidState {
		t.Fatalf("stop while provisioning: got %v, want ErrInvalidState", err)
	}
}

// nonexistentID returns a valid-but-unused machine UUID for ownership/not-found
// assertions.
func nonexistentID(t *testing.T) pgtype.UUID {
	t.Helper()
	id, err := machine.ParseUUID("00000000-0000-0000-0000-0000000000ff")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDestroy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Destroy a machine that does not exist → ErrNoMachine.
	if err := h.svc.Destroy(ctx, h.userID, nonexistentID(t)); err != machine.ErrNoMachine {
		t.Fatalf("destroy nonexistent: got %v, want ErrNoMachine", err)
	}

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	if h.machine(t).State != string(machine.StateRunning) {
		t.Fatal("expected running before destroy")
	}

	// Create minted a disk and a volume key; confirm they exist pre-destroy.
	if _, err := h.q.GetDiskByMachineID(ctx, m.ID); err != nil {
		t.Fatalf("expected disk before destroy: %v", err)
	}
	if _, err := secrets.GetMachineVolumeKey(h.sec, idStr); err != nil {
		t.Fatalf("expected volume key before destroy: %v", err)
	}

	// Destroy: agent torn down, row gone, disk + volume key cleaned up.
	if err := h.svc.Destroy(ctx, h.userID, m.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if h.agent.destroyCalls != 1 {
		t.Fatalf("agent destroy calls=%d, want 1", h.agent.destroyCalls)
	}
	if _, err := h.svc.Get(ctx, h.userID); err != machine.ErrNoMachine {
		t.Fatalf("after destroy Get: got %v, want ErrNoMachine", err)
	}
	if _, err := h.q.GetDiskByMachineID(ctx, m.ID); err == nil {
		t.Fatal("disk row should be gone after destroy (cascade)")
	}
	if _, err := secrets.GetMachineVolumeKey(h.sec, idStr); err == nil {
		t.Fatal("volume key should be gone after destroy")
	}

	// The user can create a fresh machine after a destroy.
	if _, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{}); err != nil {
		t.Fatalf("create after destroy: %v", err)
	}
}

func TestMultipleMachines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two machines for the same user, auto-named machine-1 / machine-2.
	m1, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create m1: %v", err)
	}
	m2, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create m2: %v", err)
	}
	if m1.Name != "machine-1" || m2.Name != "machine-2" {
		t.Fatalf("auto names = %q,%q; want machine-1,machine-2", m1.Name, m2.Name)
	}
	if m1.ID == m2.ID {
		t.Fatal("the two machines share an id")
	}

	ms, err := h.svc.List(ctx, h.userID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("list: len=%d err=%v, want 2 machines", len(ms), err)
	}

	// Destroying one leaves the other untouched.
	if err := h.svc.Destroy(ctx, h.userID, m1.ID); err != nil {
		t.Fatalf("destroy m1: %v", err)
	}
	ms, _ = h.svc.List(ctx, h.userID)
	if len(ms) != 1 || ms[0].ID != m2.ID {
		t.Fatalf("after destroy m1, list should be [m2]; got %d machines", len(ms))
	}
}

func TestMachineLimit(t *testing.T) {
	h := newHarness(t) // default cap is 3
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{}); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
	}
	// The 4th exceeds the cap.
	if _, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{}); err != machine.ErrMachineLimit {
		t.Fatalf("4th create: got %v, want ErrMachineLimit", err)
	}
}

func TestOwnershipRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A different user may not start/stop/destroy/rename this machine — all map
	// to ErrNoMachine (existence never leaked).
	other, err := h.q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 99, Login: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Start(ctx, other.ID, m.ID); err != machine.ErrNoMachine {
		t.Fatalf("foreign start: got %v, want ErrNoMachine", err)
	}
	if err := h.svc.Destroy(ctx, other.ID, m.ID); err != machine.ErrNoMachine {
		t.Fatalf("foreign destroy: got %v, want ErrNoMachine", err)
	}
	if _, err := h.svc.Rename(ctx, other.ID, m.ID, "hax"); err != machine.ErrNoMachine {
		t.Fatalf("foreign rename: got %v, want ErrNoMachine", err)
	}
}

func TestRename(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := h.svc.Rename(ctx, h.userID, m.ID, "api-box")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "api-box" {
		t.Fatalf("name = %q, want api-box", renamed.Name)
	}
}

func TestRunningSweepDetectsCrash(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	if h.machine(t).State != string(machine.StateRunning) {
		t.Fatal("expected running before sweep")
	}

	// The VM crashes: agent now reports stopped. The 30s sweep moves it to error.
	h.agent.SetStatus(idStr, agentapi.StateStopped, "", "")
	h.poller.SweepRunning(ctx)
	if got := h.machine(t).State; got != string(machine.StateError) {
		t.Fatalf("after crash sweep state=%q, want error", got)
	}
}
