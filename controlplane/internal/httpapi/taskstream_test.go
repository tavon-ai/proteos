package httpapi_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// taskStreamFixture wires a Server with a live TaskEvents hub and one agent task.
type taskStreamFixture struct {
	url    string
	token  string
	mid    string
	taskID string
	hub    *taskevents.Hub
}

func setupTaskStream(t *testing.T, status string) taskStreamFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`)})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", mc.ID); err != nil {
		t.Fatalf("set state: %v", err)
	}

	task, err := q.InsertAgentTask(ctx, store.InsertAgentTaskParams{
		MachineID: mc.ID, UserID: mc.UserID, Provider: "claude", Project: "alpha", Prompt: "x",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if status != "" && status != "queued" {
		if _, err := pool.Exec(ctx, "UPDATE agent_tasks SET status=$1, result_summary='all set', usage='{\"cost_usd\":0.2}' WHERE id=$2", status, task.ID); err != nil {
			t.Fatalf("set task status: %v", err)
		}
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	hub := taskevents.New(taskevents.DefaultBufferSize, taskevents.DefaultRetention)
	srv := &httpapi.Server{
		Sessions:   sessions,
		Queries:    q,
		Audit:      audit.NewRecorder(q),
		Machines:   machine.NewService(pool, nil, machine.NewBroker(), nil, machine.Spec{}),
		TaskEvents: hub,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return taskStreamFixture{
		url: ts.URL, token: token,
		mid: machine.UUIDString(mc.ID), taskID: machine.UUIDString(task.ID), hub: hub,
	}
}

// readSSE connects to the task event stream and returns the frames it reads until
// a terminal `result` frame, EOF, or the context deadline. lastEventID, when
// non-empty, is sent as the reconnect header.
func readSSE(t *testing.T, fx taskStreamFixture, lastEventID string) []sseFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fx.url+"/api/machines/"+fx.mid+"/tasks/"+fx.taskID+"/events", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	var frames []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" || cur.data != "" {
				frames = append(frames, cur)
				if cur.event == "agent" && strings.Contains(cur.data, `"kind":"result"`) {
					return frames // terminal frame — stream will close
				}
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return frames
}

func TestTaskEvents_SnapshotThenTerminalClose(t *testing.T) {
	fx := setupTaskStream(t, "running")
	// Everything is already in the hub before connect: two events + terminal.
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"hi"}`), false)
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"tool_use","tool":"Bash"}`), false)
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"result","status":"done","is_error":false}`), true)

	frames := readSSE(t, fx, "")
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].event != "agent" || !strings.Contains(frames[0].data, "assistant_text") {
		t.Errorf("frame 0 = %+v", frames[0])
	}
	if frames[0].id != "1" || frames[2].id != "3" {
		t.Errorf("frame ids = %q..%q, want 1..3", frames[0].id, frames[2].id)
	}
	if !strings.Contains(frames[2].data, `"kind":"result"`) {
		t.Errorf("last frame should be the terminal result: %+v", frames[2])
	}
}

func TestTaskEvents_LiveFanOut(t *testing.T) {
	fx := setupTaskStream(t, "running")
	// Publish live, shortly after the reader connects.
	go func() {
		time.Sleep(150 * time.Millisecond)
		fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"live"}`), false)
		fx.hub.Publish(fx.taskID, []byte(`{"kind":"result","status":"done"}`), true)
	}()
	frames := readSSE(t, fx, "")
	if len(frames) != 2 || !strings.Contains(frames[0].data, "live") {
		t.Fatalf("unexpected live frames: %+v", frames)
	}
	if !strings.Contains(frames[1].data, `"kind":"result"`) {
		t.Errorf("expected terminal result, got %+v", frames[1])
	}
}

func TestTaskEvents_ReconnectReplay(t *testing.T) {
	fx := setupTaskStream(t, "running")
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"a"}`), false) // seq 1
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"b"}`), false) // seq 2
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"result","status":"done"}`), true)     // seq 3 (terminal)

	// Reconnect claiming we already saw seq 2 — only the terminal (seq 3) replays.
	frames := readSSE(t, fx, "2")
	if len(frames) != 1 || frames[0].id != "3" {
		t.Fatalf("reconnect replay = %+v, want only seq 3", frames)
	}
}

func TestTaskEvents_DBSynthFallbackWhenStreamGone(t *testing.T) {
	// The task is already terminal in the DB and the hub has no stream (CP restart
	// / past retention). The handler must synthesize the result and close.
	fx := setupTaskStream(t, "done")
	frames := readSSE(t, fx, "")
	if len(frames) != 1 {
		t.Fatalf("want 1 synthesized frame, got %d: %+v", len(frames), frames)
	}
	if !strings.Contains(frames[0].data, `"kind":"result"`) || !strings.Contains(frames[0].data, `"status":"done"`) {
		t.Errorf("synthesized frame = %+v", frames[0])
	}
	if !strings.Contains(frames[0].data, "all set") {
		t.Errorf("synthesized frame should carry the stored summary: %+v", frames[0])
	}
}

func TestTaskEvents_ResumeStreamsOnlyNewTurn(t *testing.T) {
	fx := setupTaskStream(t, "running")
	// Turn 1 runs and finishes.
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"turn one"}`), false)
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"result","status":"done"}`), true)

	// A follow-up turn (AT4): reactivate, then stream the new turn.
	fx.hub.Reopen(fx.taskID)
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"assistant_text","text":"turn two"}`), false)
	fx.hub.Publish(fx.taskID, []byte(`{"kind":"result","status":"done"}`), true)

	// A fresh connect replays only the new turn — turn one's result is not replayed
	// (which would otherwise close the client before it sees turn two).
	frames := readSSE(t, fx, "")
	if len(frames) != 2 {
		t.Fatalf("want 2 frames (new turn), got %d: %+v", len(frames), frames)
	}
	if !strings.Contains(frames[0].data, "turn two") {
		t.Errorf("first frame should be the new turn, got %+v", frames[0])
	}
	for _, f := range frames {
		if strings.Contains(f.data, "turn one") {
			t.Errorf("prior turn must not be replayed: %+v", f)
		}
	}
	if !strings.Contains(frames[1].data, `"kind":"result"`) {
		t.Errorf("expected the new turn's terminal result, got %+v", frames[1])
	}
}

func TestTaskEvents_404UnknownTask(t *testing.T) {
	fx := setupTaskStream(t, "running")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fx.url+"/api/machines/"+fx.mid+"/tasks/11111111-1111-1111-1111-111111111111/events", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
