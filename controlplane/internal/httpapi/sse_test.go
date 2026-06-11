package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// --- a minimal fake node-agent (same shape as the machine package test) ------

type sseFakeAgent struct {
	status map[string]agentapi.MachineStatus
}

func (f *sseFakeAgent) set(id, state, guestIP string) {
	f.status[id] = agentapi.MachineStatus{MachineID: id, State: state, GuestIP: guestIP, Handle: "fc-" + id[:8]}
}

func (f *sseFakeAgent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(agentapi.RouteEnsure, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := f.status[id]; !ok {
			f.set(id, agentapi.StateCreating, "")
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(agentapi.EnsureResponse{Handle: "fc-" + id[:8]})
	})
	mux.HandleFunc(agentapi.RouteStop, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc(agentapi.RouteGetMachine, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		st, ok := f.status[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}

// --- SSE frame parsing --------------------------------------------------------

type sseFrame struct {
	id    string
	event string
	data  string
}

// readFrames parses SSE frames off the stream into ch until the body closes.
func readFrames(body *bufio.Reader, ch chan<- sseFrame) {
	var cur sseFrame
	for {
		line, err := body.ReadString('\n')
		if err != nil {
			close(ch)
			return
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.event != "" || cur.data != "" {
				ch <- cur
				cur = sseFrame{}
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat comment; ignore
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func nextFrame(t *testing.T, ch <-chan sseFrame) sseFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("stream closed before expected frame")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE frame")
		return sseFrame{}
	}
}

// --- harness ------------------------------------------------------------------

type sseHarness struct {
	ts     *httptest.Server
	cookie *http.Cookie
	svc    *machine.Service
	poller *machine.Poller
	agent  *sseFakeAgent
	q      *store.Queries
	userID pgtype.UUID
}

// eventIDs returns the machine's event ids oldest-first. Tests derive expected
// SSE id: values from these rather than hard-coding absolute bigserial ids,
// which are not stable across tests on the shared CI Postgres.
func (h *sseHarness) eventIDs(t *testing.T, machineID pgtype.UUID) []int64 {
	t.Helper()
	evs, err := h.q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: machineID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(evs))
	for i, e := range evs {
		ids[i] = e.ID
	}
	return ids
}

// lastEventID returns the highest (most recent) event id for the machine.
func (h *sseHarness) lastEventID(t *testing.T, machineID pgtype.UUID) int64 {
	ids := h.eventIDs(t, machineID)
	if len(ids) == 0 {
		t.Fatal("no events for machine")
	}
	return ids[len(ids)-1]
}

func newSSEHarness(t *testing.T) *sseHarness {
	t.Helper()
	pool, q := testutil.Postgres(t)
	ctx := context.Background()

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 99, Login: "sse"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "h", AgentUrl: "http://x"})
	if err != nil {
		t.Fatal(err)
	}

	agent := &sseFakeAgent{status: map[string]agentapi.MachineStatus{}}
	agentSrv := httptest.NewServer(agent.handler())
	t.Cleanup(agentSrv.Close)

	nc := nodeclient.New(agentSrv.URL, "tok")
	broker := machine.NewBroker()
	svc := machine.NewService(pool, nc, broker, secrets.NewMemStore(), host.ID, machine.Spec{Vcpus: 2, MemMiB: 2048, KernelRef: "k", RootfsRef: "r"})
	poller := machine.NewPoller(pool, nc, broker)

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv := &httpapi.Server{Sessions: sessions, Machines: svc, Broker: broker, Queries: q}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &sseHarness{
		ts:     ts,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: token},
		svc:    svc,
		poller: poller,
		agent:  agent,
		q:      q,
		userID: user.ID,
	}
}

// connect opens an SSE stream with the auth cookie and optional Last-Event-ID,
// returning a channel of parsed frames and a closer.
func (h *sseHarness) connect(t *testing.T, lastEventID string) (<-chan sseFrame, func()) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/machine/events", nil)
	req.AddCookie(h.cookie)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE connect: status %d", resp.StatusCode)
	}
	ch := make(chan sseFrame, 16)
	go readFrames(bufio.NewReader(resp.Body), ch)
	return ch, func() { resp.Body.Close() }
}

func TestSSEStreamsTransitionsInOrder(t *testing.T) {
	h := newSSEHarness(t)
	ctx := context.Background()

	// Create the machine first (writes the provisioning event).
	m, err := h.svc.Create(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)

	// Connect: the snapshot carries the current machine + the provisioning event.
	frames, closeConn := h.connect(t, "")
	defer closeConn()

	snap := nextFrame(t, frames)
	if snap.event != "snapshot" {
		t.Fatalf("first frame event=%q, want snapshot", snap.event)
	}
	if !strings.Contains(snap.data, `"state":"provisioning"`) {
		t.Fatalf("snapshot missing provisioning machine: %s", snap.data)
	}
	provID := h.lastEventID(t, m.ID)

	// Transition 1: provisioning→running. The live frame's id: must equal the
	// new event row id (which is > the provisioning id) — we read it from the DB
	// rather than assuming an absolute value.
	h.agent.set(idStr, agentapi.StateRunning, "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	runningID := h.lastEventID(t, m.ID)
	f1 := nextFrame(t, frames)
	if f1.event != "machine" || f1.id != strconv.FormatInt(runningID, 10) {
		t.Fatalf("frame1 event=%q id=%q, want machine/%d", f1.event, f1.id, runningID)
	}
	if runningID <= provID || !strings.Contains(f1.data, `"state":"running"`) {
		t.Fatalf("frame1 not running or id not increasing: %s", f1.data)
	}

	// Transition 2: running→hibernating.
	if _, err := h.svc.Stop(ctx, h.userID); err != nil {
		t.Fatal(err)
	}
	hibernatingID := h.lastEventID(t, m.ID)
	f2 := nextFrame(t, frames)
	if f2.event != "machine" || f2.id != strconv.FormatInt(hibernatingID, 10) {
		t.Fatalf("frame2 event=%q id=%q, want machine/%d", f2.event, f2.id, hibernatingID)
	}
	if hibernatingID <= runningID || !strings.Contains(f2.data, `"state":"hibernating"`) {
		t.Fatalf("frame2 not hibernating or id not increasing: %s", f2.data)
	}
}

func TestSSEReplaysFromLastEventID(t *testing.T) {
	h := newSSEHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID) // provisioning event
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)
	h.agent.set(idStr, agentapi.StateRunning, "172.30.0.2")
	h.poller.AdvanceTransitional(ctx) // running event
	if _, err := h.svc.Stop(ctx, h.userID); err != nil {
		t.Fatal(err) // hibernating event
	}

	// The machine now has exactly three events; their ids are whatever the DB
	// assigned (not necessarily 1,2,3 on the shared CI Postgres).
	ids := h.eventIDs(t, m.ID)
	if len(ids) != 3 {
		t.Fatalf("want 3 events, got %d (%v)", len(ids), ids)
	}
	provID, runningID, hibernatingID := ids[0], ids[1], ids[2]

	// Reconnect claiming we last saw the provisioning event ⇒ replay the
	// running + stopping events from the DB, in order, with the right ids.
	frames, closeConn := h.connect(t, strconv.FormatInt(provID, 10))
	defer closeConn()

	// On replay the embedded machine summary is the current row (we persist
	// events, not historical machine snapshots), so assert on the event's
	// to_state and the id ordering — that is what makes replay lossless.
	r1 := nextFrame(t, frames)
	if r1.event != "machine" || r1.id != strconv.FormatInt(runningID, 10) || !strings.Contains(r1.data, `"to_state":"running"`) {
		t.Fatalf("replay1=%+v, want machine/%d to_state running", r1, runningID)
	}
	r2 := nextFrame(t, frames)
	if r2.event != "machine" || r2.id != strconv.FormatInt(hibernatingID, 10) || !strings.Contains(r2.data, `"to_state":"hibernating"`) {
		t.Fatalf("replay2=%+v, want machine/%d to_state hibernating", r2, hibernatingID)
	}
}
