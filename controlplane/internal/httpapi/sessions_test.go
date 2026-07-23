package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// TestListSessions_RequiresAuthAndReturnsMachineContext exercises GET
// /api/sessions: it requires authentication and returns the caller's coding
// agent sessions (agent_tasks rows) tagged with the owning machine's name.
func TestListSessions_RequiresAuthAndReturnsMachineContext(t *testing.T) {
	t.Parallel()
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)

	// Unauthenticated → 401.
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/api/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", resp.StatusCode)
	}

	resp = fx.get(t, "/api/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions status = %d", resp.StatusCode)
	}
	var body struct {
		Sessions []struct {
			ID          string `json:"id"`
			MachineID   string `json:"machine_id"`
			MachineName string `json:"machine_name"`
			Status      string `json:"status"`
			Project     string `json:"project"`
			Prompt      string `json:"prompt"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(body.Sessions))
	}
	s := body.Sessions[0]
	if s.ID != taskID || s.MachineID != fx.mid || s.Status != "running" || s.Project != "alpha" || s.Prompt != "do it" {
		t.Fatalf("unexpected session view: %+v", s)
	}
	if s.MachineName == "" {
		t.Fatal("expected machine_name to be populated")
	}
}

// TestListSessions_FiltersByStatus exercises ?status=active|finished on GET
// /api/sessions: active covers queued/running, finished covers
// done/failed/canceled.
func TestListSessions_FiltersByStatus(t *testing.T) {
	t.Parallel()
	fx := setupTasks(t, string(machine.StateRunning), true)
	activeID := createTaskFor(t, fx)
	doneID := createTaskFor(t, fx)
	finishTask(t, fx, doneID, "done", "sess-1")

	resp := fx.get(t, "/api/sessions?status=active")
	defer resp.Body.Close()
	var active struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&active); err != nil {
		t.Fatalf("decode active: %v", err)
	}
	if len(active.Sessions) != 1 || active.Sessions[0].ID != activeID {
		t.Fatalf("active sessions = %+v, want just %q", active.Sessions, activeID)
	}

	resp = fx.get(t, "/api/sessions?status=finished")
	defer resp.Body.Close()
	var finished struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&finished); err != nil {
		t.Fatalf("decode finished: %v", err)
	}
	if len(finished.Sessions) != 1 || finished.Sessions[0].ID != doneID {
		t.Fatalf("finished sessions = %+v, want just %q", finished.Sessions, doneID)
	}

	resp = fx.get(t, "/api/sessions?status=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status = %d, want 400", resp.StatusCode)
	}
}

// TestExportSessions_DownloadsCSVAttachment exercises GET /api/sessions/export:
// it requires auth, needs no CSRF header (a GET), and returns a CSV attachment
// containing every matching session.
func TestExportSessions_DownloadsCSVAttachment(t *testing.T) {
	t.Parallel()
	fx := setupTasks(t, string(machine.StateRunning), true)
	_ = createTaskFor(t, fx)

	resp := fx.get(t, "/api/sessions/export")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "id,machine,status,provider,project,prompt") {
		t.Fatalf("export missing header row: %q", text)
	}
	if !strings.Contains(text, "do it") {
		t.Fatalf("export missing session prompt: %q", text)
	}
}

// TestGetSession_ReturnsFullSessionAndScopesToOwner exercises GET
// /api/sessions/{id} (TAV-142 detail view): it requires auth, returns the same
// fields as the list view for the caller's own session, 404s on an unknown id,
// and 404s (not leaking existence) on a session owned by a different user.
func TestGetSession_ReturnsFullSessionAndScopesToOwner(t *testing.T) {
	t.Parallel()
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)

	// Unauthenticated → 401.
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/api/sessions/"+taskID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", resp.StatusCode)
	}

	resp = fx.get(t, "/api/sessions/"+taskID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions/{id} status = %d", resp.StatusCode)
	}
	var body struct {
		ID          string `json:"id"`
		MachineID   string `json:"machine_id"`
		MachineName string `json:"machine_name"`
		Status      string `json:"status"`
		Project     string `json:"project"`
		Prompt      string `json:"prompt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != taskID || body.MachineID != fx.mid || body.Status != "running" ||
		body.Project != "alpha" || body.Prompt != "do it" || body.MachineName == "" {
		t.Fatalf("unexpected session view: %+v", body)
	}

	// Unknown id → 404 no_session.
	resp = fx.get(t, "/api/sessions/00000000-0000-0000-0000-000000000000")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", resp.StatusCode)
	}

	// A session owned by a different user 404s rather than leaking existence.
	otherToken := otherUserToken(t, fx.q)
	req, _ = http.NewRequest(http.MethodGet, fx.url+"/api/sessions/"+taskID, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: otherToken})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("other-user GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other-user GET status = %d, want 404", resp.StatusCode)
	}
}

// TestExportSession_DownloadsJSONAttachment exercises GET
// /api/sessions/{id}/export: it requires auth, needs no CSRF header (a GET),
// and returns the single session as a JSON attachment.
func TestExportSession_DownloadsJSONAttachment(t *testing.T) {
	t.Parallel()
	fx := setupTasks(t, string(machine.StateRunning), true)
	taskID := createTaskFor(t, fx)

	resp := fx.get(t, "/api/sessions/"+taskID+"/export")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}
	var body struct {
		ID     string `json:"id"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != taskID || body.Prompt != "do it" {
		t.Fatalf("unexpected export body: %+v", body)
	}

	resp = fx.get(t, "/api/sessions/00000000-0000-0000-0000-000000000000/export")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id export status = %d, want 404", resp.StatusCode)
	}
}

// otherUserToken creates a second user and returns a valid session cookie
// value for them — used to assert that session detail/export routes scope to
// the caller and 404 rather than leak another user's session.
func otherUserToken(t *testing.T, q *store.Queries) string {
	t.Helper()
	ctx := context.Background()
	other, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 99, Login: "other", Email: "other@example.com"})
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	token, err := session.NewManager(q, time.Hour).Create(ctx, other.ID)
	if err != nil {
		t.Fatalf("other session: %v", err)
	}
	return token
}
