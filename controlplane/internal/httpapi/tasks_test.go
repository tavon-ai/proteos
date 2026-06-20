package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon/proteos/guestagent/api"
)

func setupTasks(t *testing.T, machineState string, withKey bool) wtFixture {
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
	uid := machine.UUIDString(user.ID)

	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`)})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state=$1 WHERE id=$2", machineState, mc.ID); err != nil {
		t.Fatalf("set state: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	reg := providers.NewRegistry(q)
	if _, err := reg.SetEnabled(ctx, []string{"claude", "gemini"}); err != nil {
		t.Fatalf("enable providers: %v", err)
	}
	sec := secrets.NewMemStore()
	if withKey {
		if err := sec.Put(secrets.UserProviderPath(uid, "claude"), map[string]string{"api_key": "sk-x"}); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	srv := &httpapi.Server{
		Sessions:    sessions,
		Queries:     q,
		Audit:       audit.NewRecorder(q),
		Machines:    machine.NewService(pool, nil, machine.NewBroker(), sec, host.ID, machine.Spec{}),
		Providers:   reg,
		Secrets:     sec,
		GitWorktree: ch,
		TaskChannel: ch,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return wtFixture{url: ts.URL, token: token, mid: machine.UUIDString(mc.ID), ch: ch, q: q}
}

func TestCreateTask_202(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"make it responsive","provider":"claude","project":"alpha"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.TaskID == "" {
		t.Fatal("missing task_id")
	}
	if fx.ch.lastRunProvider != "claude" || fx.ch.lastRunPath != "/workspace/alpha" || fx.ch.lastRunPrompt != "make it responsive" {
		t.Fatalf("guest call = provider %q path %q prompt %q", fx.ch.lastRunProvider, fx.ch.lastRunPath, fx.ch.lastRunPrompt)
	}
	if fx.ch.lastRunTaskID != body.TaskID {
		t.Fatalf("task id mismatch: dispatched %q, returned %q", fx.ch.lastRunTaskID, body.TaskID)
	}

	// The task is now readable and in the running state.
	get := fx.get(t, "/api/machines/"+fx.mid+"/tasks/"+body.TaskID)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get task status = %d, want 200", get.StatusCode)
	}
	var tv struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(get.Body).Decode(&tv)
	if tv.Status != "running" || tv.Provider != "claude" || tv.ID != body.TaskID {
		t.Fatalf("unexpected task view: %+v", tv)
	}
}

func TestCreateTask_400ProviderNotHeadless(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"x","provider":"gemini","project":"alpha"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "provider_not_headless" {
		t.Fatalf("error = %q, want provider_not_headless", code)
	}
	if fx.ch.lastRunProvider != "" {
		t.Errorf("non-headless provider should not be dispatched")
	}
}

func TestCreateTask_409NoProviderKey(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), false) // no key
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"x","provider":"claude","project":"alpha"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "no_provider_key" {
		t.Fatalf("error = %q, want no_provider_key", code)
	}
}

func TestCreateTask_400BadProject(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"x","provider":"claude","project":"ghost"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "bad_project" {
		t.Fatalf("error = %q, want bad_project", code)
	}
}

func TestCreateTask_RequiresCSRF(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"x","provider":"claude","project":"alpha"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestCreateTask_409NotRunning(t *testing.T) {
	fx := setupTasks(t, string(machine.StateStopped), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"x","provider":"claude","project":"alpha"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestListTasks(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	r1 := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"one","provider":"claude","project":"alpha"}`, true)
	r1.Body.Close()

	list := fx.get(t, "/api/machines/"+fx.mid+"/tasks")
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", list.StatusCode)
	}
	var body struct {
		Tasks []struct {
			Status  string `json:"status"`
			Project string `json:"project"`
		} `json:"tasks"`
	}
	_ = json.NewDecoder(list.Body).Decode(&body)
	if len(body.Tasks) != 1 || body.Tasks[0].Project != "alpha" {
		t.Fatalf("unexpected task list: %+v", body.Tasks)
	}
}

// createTaskFor posts a task and returns its id (status running after dispatch).
func createTaskFor(t *testing.T, fx wtFixture) string {
	t.Helper()
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks",
		`{"prompt":"do it","provider":"claude","project":"alpha"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create task status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.TaskID == "" {
		t.Fatal("missing task_id")
	}
	return body.TaskID
}

func TestCancelTask_202DispatchesCancel(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)

	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks/"+taskID+"/cancel", "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202", resp.StatusCode)
	}
	if fx.ch.lastCancelTask != taskID {
		t.Fatalf("guest cancel dispatched for %q, want %q", fx.ch.lastCancelTask, taskID)
	}
}

func TestCancelTask_200NoopWhenTerminal(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)

	// Drive the task to a terminal state directly, then cancel.
	tid, _ := machine.ParseUUID(taskID)
	if err := fx.q.FinishAgentTask(context.Background(), store.FinishAgentTaskParams{
		ID: tid, Status: "done", Usage: []byte("{}"),
	}); err != nil {
		t.Fatalf("finish task: %v", err)
	}

	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks/"+taskID+"/cancel", "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel-after-done status = %d, want 200 (no-op)", resp.StatusCode)
	}
	if fx.ch.lastCancelTask != "" {
		t.Errorf("a terminal task must not dispatch a guest cancel (got %q)", fx.ch.lastCancelTask)
	}
}

func TestCancelTask_RequiresCSRF(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks/"+taskID+"/cancel", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestCancelTask_404Unknown(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/tasks/11111111-1111-1111-1111-111111111111/cancel", "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetTask_404Unknown(t *testing.T) {
	fx := setupTasks(t, string(machine.StateRunning), true)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/tasks/11111111-1111-1111-1111-111111111111")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
