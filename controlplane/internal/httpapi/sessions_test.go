package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
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
