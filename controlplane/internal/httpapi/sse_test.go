package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
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
	userID pgtype.UUID
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
	svc := machine.NewService(pool, nc, broker, host.ID, machine.Spec{Vcpus: 2, MemMiB: 2048, KernelRef: "k", RootfsRef: "r"})
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

	// Create the machine first (writes the provisioning event, id 1).
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

	// Transition 1: provisioning→running (event id 2).
	h.agent.set(idStr, agentapi.StateRunning, "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	f1 := nextFrame(t, frames)
	if f1.event != "machine" || f1.id != "2" {
		t.Fatalf("frame1 event=%q id=%q, want machine/2", f1.event, f1.id)
	}
	if !strings.Contains(f1.data, `"state":"running"`) {
		t.Fatalf("frame1 not running: %s", f1.data)
	}

	// Transition 2: running→stopping (event id 3).
	if _, err := h.svc.Stop(ctx, h.userID); err != nil {
		t.Fatal(err)
	}
	f2 := nextFrame(t, frames)
	if f2.event != "machine" || f2.id != "3" {
		t.Fatalf("frame2 event=%q id=%q, want machine/3", f2.event, f2.id)
	}
	if !strings.Contains(f2.data, `"state":"stopping"`) {
		t.Fatalf("frame2 not stopping: %s", f2.data)
	}
}

func TestSSEReplaysFromLastEventID(t *testing.T) {
	h := newSSEHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID) // event 1: provisioning
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)
	h.agent.set(idStr, agentapi.StateRunning, "172.30.0.2")
	h.poller.AdvanceTransitional(ctx) // event 2: running
	if _, err := h.svc.Stop(ctx, h.userID); err != nil {
		t.Fatal(err) // event 3: stopping
	}

	// Reconnect claiming we last saw event 1 ⇒ replay 2 and 3 from the DB.
	frames, closeConn := h.connect(t, "1")
	defer closeConn()

	// On replay the embedded machine summary is the current row (we persist
	// events, not historical machine snapshots), so assert on the event's
	// to_state and the id ordering — that is what makes replay lossless.
	r1 := nextFrame(t, frames)
	if r1.event != "machine" || r1.id != "2" || !strings.Contains(r1.data, `"to_state":"running"`) {
		t.Fatalf("replay1=%+v, want machine/2 to_state running", r1)
	}
	r2 := nextFrame(t, frames)
	if r2.event != "machine" || r2.id != "3" || !strings.Contains(r2.data, `"to_state":"stopping"`) {
		t.Fatalf("replay2=%+v, want machine/3 to_state stopping", r2)
	}
}
