package guestctl_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/guestctl"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/taskevents"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// fakeGuest is a minimal stand-in for the guest agent's /control endpoint
// (controlplane cannot import the guest's internal ctlchan package). It records
// CP → guest requests and can itself initiate guest → CP requests.
type fakeGuest struct {
	mu         sync.Mutex
	conn       *websocket.Conn
	nextID     int64
	waiters    map[int64]chan guestwire.ControlFrame
	configured chan guestwire.GitConfigurePayload
	cloned     chan guestwire.GitClonePayload
	canceled   chan guestwire.AgentCancelPayload
}

func newFakeGuest() *fakeGuest {
	return &fakeGuest{
		waiters:    map[int64]chan guestwire.ControlFrame{},
		configured: make(chan guestwire.GitConfigurePayload, 4),
		cloned:     make(chan guestwire.GitClonePayload, 4),
		canceled:   make(chan guestwire.AgentCancelPayload, 4),
	}
}

func (g *fakeGuest) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(guestwire.RouteControl, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		g.mu.Lock()
		g.conn = c
		g.mu.Unlock()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var f guestwire.ControlFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Kind {
			case guestwire.ControlReq:
				g.onReq(r.Context(), f)
			case guestwire.ControlResp, guestwire.ControlErr:
				g.deliver(f)
			}
		}
	})
	return mux
}

func (g *fakeGuest) onReq(ctx context.Context, f guestwire.ControlFrame) {
	switch f.Op {
	case guestwire.OpGitConfigure:
		var p guestwire.GitConfigurePayload
		_ = json.Unmarshal(f.Payload, &p)
		g.configured <- p
		g.write(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp})
	case guestwire.OpGitClone:
		var p guestwire.GitClonePayload
		_ = json.Unmarshal(f.Payload, &p)
		g.write(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp})
		g.cloned <- p
		// Report completion back to the CP (guest → CP notification).
		go g.write(ctx, guestwire.ControlFrame{ID: 99999, Kind: guestwire.ControlReq, Op: guestwire.OpGitCloneDone,
			Payload: mustMarshal(guestwire.GitCloneDonePayload{OpID: p.OpID, OK: true})})
	case guestwire.OpAgentCancel:
		var p guestwire.AgentCancelPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.write(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp})
		g.canceled <- p
	}
}

func (g *fakeGuest) deliver(f guestwire.ControlFrame) {
	g.mu.Lock()
	ch := g.waiters[f.ID]
	delete(g.waiters, f.ID)
	g.mu.Unlock()
	if ch != nil {
		ch <- f
	}
}

func (g *fakeGuest) write(ctx context.Context, f guestwire.ControlFrame) {
	b, _ := json.Marshal(f)
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c != nil {
		_ = c.Write(ctx, websocket.MessageText, b)
	}
}

// guestRequest sends a guest → CP request and waits for the reply.
func (g *fakeGuest) guestRequest(t *testing.T, op string, payload any) guestwire.ControlFrame {
	t.Helper()
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	ch := make(chan guestwire.ControlFrame, 1)
	g.waiters[id] = ch
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		t.Fatal("no guest connection")
	}
	g.write(context.Background(), guestwire.ControlFrame{ID: id, Kind: guestwire.ControlReq, Op: op, Payload: mustMarshal(payload)})
	select {
	case f := <-ch:
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for reply to %s", op)
		return guestwire.ControlFrame{}
	}
}

func mustMarshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// tcpDialer dials the fake guest's TCP listener as if it were the guest tunnel.
type tcpDialer struct{ addr string }

func (d tcpDialer) DialGuest(ctx context.Context, _ string, _ uint32) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func setupManager(t *testing.T) (*guestctl.Manager, *machine.Broker, *fakeGuest, store.Machine, *store.Queries, *taskevents.Hub) {
	t.Helper()
	_, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(context.Background(), store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(context.Background(), store.UpsertUserParams{GithubUserID: 7, Login: "octocat", Email: "octo@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(context.Background(), store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	mc.State = string(machine.StateRunning) // what the broker will publish

	// Seed a valid (non-expiring) GitHub token so the credential path needs no refresh.
	uid := uuidStrT(user.ID)
	sec, err := secrets.NewFileStore(t.TempDir() + "/s.json")
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	if err := sec.Put(secrets.UserGitHubPath(uid), map[string]string{
		"access_token":            "gho_valid",
		"refresh_token":           "ghr_valid",
		"access_token_expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"revoked": false})
	if _, err := q.UpsertGitHubLink(context.Background(), store.UpsertGitHubLinkParams{UserID: user.ID, Metadata: meta, SecretRef: secrets.UserGitHubPath(uid)}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	gh := github.NewClient(github.Config{ClientID: "id", ClientSecret: "s"})
	tokens := github.NewTokenSource(gh, q, sec)

	fg := newFakeGuest()
	ts := httptest.NewServer(fg.handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	broker := machine.NewBroker()
	hub := taskevents.New(taskevents.DefaultBufferSize, taskevents.DefaultRetention)
	mgr := guestctl.New(tcpDialer{addr: addr}, broker, q, tokens, audit.NewRecorder(q), hub, "github.com")
	return mgr, broker, fg, mc, q, hub
}

func TestControlChannel_ConfigureCredentialClone(t *testing.T) {
	mgr, broker, fg, mc, q, _ := setupManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// Trigger the manager to dial by publishing the running transition. Publish in
	// a loop until the channel is up: the broker drops updates delivered before
	// Run subscribes, and ensure() de-dupes so repeats are harmless.
	machineID := machine.UUIDString(mc.ID)
	go func() {
		for i := 0; i < 60; i++ {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// 1. Channel comes up and git.configure is applied with the owner's identity.
	select {
	case cfg := <-fg.configured:
		if cfg.Name != "octocat" || cfg.Email != "octo@example.com" {
			t.Fatalf("unexpected configure payload: %+v", cfg)
		}
		if cfg.Helper != guestwire.HelperBinPath {
			t.Fatalf("unexpected helper: %q", cfg.Helper)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("git.configure was never applied")
	}

	waitChannel(t, mgr, machineID)

	// 2. git.credential success → x-access-token + the stored token.
	resp := fg.guestRequest(t, guestwire.OpGitCredential, guestwire.GitCredentialRequest{Host: "github.com", Protocol: "https"})
	if resp.Kind != guestwire.ControlResp {
		t.Fatalf("credential: expected resp, got %s (%s)", resp.Kind, resp.Payload)
	}
	var cred guestwire.GitCredentialResponse
	_ = json.Unmarshal(resp.Payload, &cred)
	if cred.Username != "x-access-token" || cred.Password != "gho_valid" {
		t.Fatalf("unexpected credential: %+v", cred)
	}

	// 3. Foreign host is refused at the choke point.
	bad := fg.guestRequest(t, guestwire.OpGitCredential, guestwire.GitCredentialRequest{Host: "evil.example.com", Protocol: "https"})
	if bad.Kind != guestwire.ControlErr {
		t.Fatalf("foreign host: expected err frame, got %s", bad.Kind)
	}
	var ep guestwire.ControlErrorPayload
	_ = json.Unmarshal(bad.Payload, &ep)
	if ep.Code != guestwire.ErrCodeForbiddenHost {
		t.Fatalf("foreign host: expected forbidden_host, got %q", ep.Code)
	}

	// 4. Unknown op from the guest gets an error frame.
	unk := fg.guestRequest(t, "git.bogus", map[string]string{})
	if unk.Kind != guestwire.ControlErr {
		t.Fatalf("unknown op: expected err frame, got %s", unk.Kind)
	}

	// 5. Clone dispatch → guest receives it → completion lands as a machine_events row.
	if err := mgr.Clone(ctx, machineID, "https://github.com/octocat/hello.git", "/workspace/hello", "op123"); err != nil {
		t.Fatalf("clone dispatch: %v", err)
	}
	select {
	case cl := <-fg.cloned:
		if cl.OpID != "op123" || cl.Dest != "/workspace/hello" {
			t.Fatalf("unexpected clone payload: %+v", cl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest never received git.clone")
	}
	waitCloneEvent(t, q, mc.ID, "op123")
}

func TestControlChannel_AgentDone(t *testing.T) {
	mgr, broker, fg, mc, q, _ := setupManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	machineID := machine.UUIDString(mc.ID)
	go func() {
		for i := 0; i < 60; i++ {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitChannel(t, mgr, machineID)

	// A queued task exists; the guest reports it done.
	task, err := q.InsertAgentTask(ctx, store.InsertAgentTaskParams{
		MachineID: mc.ID, UserID: mc.UserID, Provider: "claude", Project: "alpha", Prompt: "x",
	})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	taskID := machine.UUIDString(task.ID)

	resp := fg.guestRequest(t, guestwire.OpAgentDone, guestwire.AgentDonePayload{
		TaskID: taskID, OK: true, SessionID: "sess-1", Summary: "did it", CostUSD: 0.1, NumTurns: 2,
	})
	if resp.Kind != guestwire.ControlResp {
		t.Fatalf("agent.done: expected resp, got %s (%s)", resp.Kind, resp.Payload)
	}

	// The task row reaches done with the agent's session id captured.
	for i := 0; i < 80; i++ {
		got, err := q.GetAgentTask(ctx, task.ID)
		if err == nil && got.Status == "done" {
			if got.AgentSessionID != "sess-1" || got.ResultSummary != "did it" {
				t.Fatalf("unexpected finished task: %+v", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent task never reached done")
}

func TestControlChannel_AgentEventFanOut(t *testing.T) {
	mgr, broker, fg, mc, _, hub := setupManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	machineID := machine.UUIDString(mc.ID)
	go func() {
		for i := 0; i < 60; i++ {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitChannel(t, mgr, machineID)

	const taskID = "task-evt-1"
	// The guest relays a normalized assistant_text event, then the run finishes.
	fg.guestRequest(t, guestwire.OpAgentEvent, guestwire.AgentEventPayload{
		TaskID: taskID, Kind: guestwire.AgentEventAssistantText, Text: "working on it",
	})

	// Subscribe and confirm the event reached the hub (snapshot of the ring).
	var backlog []taskevents.Frame
	for i := 0; i < 80; i++ {
		var cancelSub func()
		backlog, _, cancelSub, _ = hub.Subscribe(taskID, 0)
		cancelSub()
		if len(backlog) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(backlog) != 1 {
		t.Fatalf("want 1 buffered event, got %d", len(backlog))
	}
	if !strings.Contains(string(backlog[0].Data), "assistant_text") || !strings.Contains(string(backlog[0].Data), "working on it") {
		t.Fatalf("unexpected event payload: %s", backlog[0].Data)
	}
	// The task id is stripped from the per-frame payload (the stream is task-scoped).
	if strings.Contains(string(backlog[0].Data), taskID) {
		t.Errorf("event payload should not echo the task id: %s", backlog[0].Data)
	}

	// Completion publishes a terminal result frame that closes subscribers.
	fg.guestRequest(t, guestwire.OpAgentDone, guestwire.AgentDonePayload{
		TaskID: taskID, OK: true, Summary: "all set",
	})
	var terminal bool
	for i := 0; i < 80; i++ {
		var cancelSub func()
		backlog, _, cancelSub, terminal = hub.Subscribe(taskID, 0)
		cancelSub()
		if terminal {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !terminal {
		t.Fatal("stream never marked terminal after agent.done")
	}
	last := backlog[len(backlog)-1]
	if !last.Terminal || !strings.Contains(string(last.Data), "\"kind\":\"result\"") {
		t.Fatalf("final frame not a terminal result: %+v", last)
	}
}

func TestControlChannel_CancelAgentDispatch(t *testing.T) {
	mgr, broker, fg, mc, _, _ := setupManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	machineID := machine.UUIDString(mc.ID)
	go func() {
		for range 60 {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitChannel(t, mgr, machineID)

	if err := mgr.CancelAgent(ctx, machineID, "task-9"); err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	select {
	case p := <-fg.canceled:
		if p.TaskID != "task-9" {
			t.Fatalf("guest got cancel for %q, want task-9", p.TaskID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guest never received agent.cancel")
	}

	// No live channel ⇒ ErrNoChannel.
	if err := mgr.CancelAgent(ctx, "00000000-0000-0000-0000-000000000000", "task-9"); err != guestctl.ErrNoChannel {
		t.Fatalf("CancelAgent on unknown machine = %v, want ErrNoChannel", err)
	}
}

func TestControlChannel_AgentDoneCanceled(t *testing.T) {
	mgr, broker, fg, mc, q, hub := setupManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	machineID := machine.UUIDString(mc.ID)
	go func() {
		for range 60 {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitChannel(t, mgr, machineID)

	task, err := q.InsertAgentTask(ctx, store.InsertAgentTaskParams{
		MachineID: mc.ID, UserID: mc.UserID, Provider: "claude", Project: "alpha", Prompt: "x",
	})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	taskID := machine.UUIDString(task.ID)

	// The guest reports the run ended via cancellation.
	fg.guestRequest(t, guestwire.OpAgentDone, guestwire.AgentDonePayload{
		TaskID: taskID, OK: false, Canceled: true, Error: "canceled",
	})

	// The task row reaches the terminal `canceled` state (not failed).
	for i := 0; i < 80; i++ {
		got, err := q.GetAgentTask(ctx, task.ID)
		if err == nil && got.Status == "canceled" {
			break
		}
		if i == 79 {
			t.Fatal("task never reached canceled")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The live stream gets a terminal result frame carrying status canceled.
	backlog, _, cancelSub, terminal := hub.Subscribe(taskID, 0)
	cancelSub()
	if !terminal || len(backlog) == 0 {
		t.Fatalf("expected terminal stream, got backlog=%d terminal=%v", len(backlog), terminal)
	}
	last := backlog[len(backlog)-1]
	if !last.Terminal || !strings.Contains(string(last.Data), `"status":"canceled"`) {
		t.Fatalf("final frame not a canceled result: %s", last.Data)
	}
}

func waitChannel(t *testing.T, mgr *guestctl.Manager, machineID string) {
	t.Helper()
	for i := 0; i < 80; i++ {
		if mgr.HasChannel(machineID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("channel never registered")
}

func waitCloneEvent(t *testing.T, q *store.Queries, machineID pgtype.UUID, opID string) {
	t.Helper()
	for i := 0; i < 80; i++ {
		evs, err := q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: machineID, Limit: 20})
		if err == nil {
			for _, ev := range evs {
				if ev.Type == "git.clone" && strings.Contains(string(ev.Payload), opID) {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("clone completion event never recorded")
}

func uuidStrT(u pgtype.UUID) string {
	const hexdig = "0123456789abcdef"
	buf := make([]byte, 36)
	pos := 0
	for i := range 16 {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[pos] = '-'
			pos++
		}
		b := u.Bytes[i]
		buf[pos] = hexdig[b>>4]
		buf[pos+1] = hexdig[b&0x0f]
		pos += 2
	}
	return string(buf)
}
