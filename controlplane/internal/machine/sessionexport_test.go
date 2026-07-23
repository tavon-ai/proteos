package machine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// seedSessions inserts n queued agent_tasks rows for m, returning their ids.
func seedSessions(t *testing.T, h *harness, m store.Machine, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		task, err := h.q.InsertAgentTask(ctx, store.InsertAgentTaskParams{
			MachineID: m.ID, UserID: h.userID, Provider: "claude", Project: "proj",
			Prompt: "do something useful",
		})
		if err != nil {
			t.Fatalf("seed session %d: %v", i, err)
		}
		ids = append(ids, machine.UUIDString(task.ID))
	}
	return ids
}

// TAV-141: Destroy exports every one of the machine's sessions to a
// "<machine-name>_<session-id>.json" file before tearing it down.
func TestDestroy_ExportsSessionsBeforeDestroy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sessionIDs := seedSessions(t, h, m, 2)

	if err := h.svc.Destroy(ctx, h.userID, m.ID, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	entries, err := os.ReadDir(h.exportDir)
	if err != nil {
		t.Fatalf("read export dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("exported files = %d, want 2 (dir: %v)", len(entries), entries)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		wantPrefix := m.Name + "_"
		if !strings.HasPrefix(e.Name(), wantPrefix) || !strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("unexpected export filename %q", e.Name())
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(e.Name(), wantPrefix), ".json")
		seen[id] = true

		data, err := os.ReadFile(filepath.Join(h.exportDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var rec map[string]any
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Name(), err)
		}
		if rec["machine_name"] != m.Name {
			t.Errorf("%s: machine_name = %v, want %q", e.Name(), rec["machine_name"], m.Name)
		}
		if rec["prompt"] != "do something useful" {
			t.Errorf("%s: prompt = %v", e.Name(), rec["prompt"])
		}
	}
	for _, id := range sessionIDs {
		if !seen[id] {
			t.Errorf("session %s was not exported", id)
		}
	}
}

// A machine with no sessions is a no-op: no export directory is even created.
func TestDestroy_NoSessionsSkipsExportDir(t *testing.T) {
	h := newHarnessWithExportDir(t, filepath.Join(t.TempDir(), "never-created"))
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Destroy(ctx, h.userID, m.ID, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(h.exportDir); !os.IsNotExist(err) {
		t.Fatalf("export dir should not have been created for a session-less machine, stat err = %v", err)
	}
}

// TAV-141: when the export directory can't be created (simulated here by a
// regular file sitting where the directory should go — portable across CI,
// unlike a chmod-based permission test which no-ops when tests run as root),
// Destroy blocks and the machine survives, unless force is set.
func TestDestroy_BlocksOnExportFailureUnlessForced(t *testing.T) {
	blockedDir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithExportDir(t, blockedDir)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seedSessions(t, h, m, 1)

	err = h.svc.Destroy(ctx, h.userID, m.ID, false)
	if !errors.Is(err, machine.ErrSessionExportFailed) {
		t.Fatalf("destroy without force: got %v, want ErrSessionExportFailed", err)
	}
	if h.agent.destroyCalls != 0 {
		t.Fatalf("agent destroy should not have been called, got %d calls", h.agent.destroyCalls)
	}
	if _, err := h.svc.GetByID(ctx, m.ID); err != nil {
		t.Fatalf("machine should still exist after a blocked destroy: %v", err)
	}

	// force=true bypasses the export failure and deletes the machine anyway.
	if err := h.svc.Destroy(ctx, h.userID, m.ID, true); err != nil {
		t.Fatalf("forced destroy: %v", err)
	}
	if h.agent.destroyCalls != 1 {
		t.Fatalf("agent destroy calls = %d, want 1 after forced destroy", h.agent.destroyCalls)
	}
	if _, err := h.svc.GetByID(ctx, m.ID); err != machine.ErrNoMachine {
		t.Fatalf("machine should be gone after forced destroy: got %v", err)
	}
}

// TAV-141: DestroyAll forwards force to every machine, so a batch destroy can
// bypass a blocked export across the board.
func TestDestroyAll_ForceBypassesExportFailure(t *testing.T) {
	blockedDir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithExportDir(t, blockedDir)
	ctx := context.Background()

	m1, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seedSessions(t, h, m1, 1)
	m2, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	results, err := h.svc.DestroyAll(ctx, h.userID, false)
	if err != nil {
		t.Fatalf("DestroyAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, res := range results {
		if res.MachineID == m1.ID {
			if !errors.Is(res.Err, machine.ErrSessionExportFailed) {
				t.Errorf("m1 (sessions, blocked dir): got %v, want ErrSessionExportFailed", res.Err)
			}
		} else if res.Err != nil {
			t.Errorf("m2 (no sessions): unexpected error %v", res.Err)
		}
	}
	// m1 should still exist (blocked); m2 should be gone.
	if _, err := h.svc.GetByID(ctx, m1.ID); err != nil {
		t.Fatalf("m1 should still exist: %v", err)
	}
	if _, err := h.svc.GetByID(ctx, m2.ID); err != machine.ErrNoMachine {
		t.Fatalf("m2 should be gone: got %v", err)
	}

	// Retry the whole batch with force=true: m1 now goes too.
	results, err = h.svc.DestroyAll(ctx, h.userID, true)
	if err != nil {
		t.Fatalf("DestroyAll (forced): %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("forced retry results = %+v, want a single successful destroy of m1", results)
	}
	if _, err := h.svc.GetByID(ctx, m1.ID); err != machine.ErrNoMachine {
		t.Fatalf("m1 should be gone after forced retry: got %v", err)
	}
}

// Partial failure within one machine's sessions: some files export, others
// don't, and the aggregate error names which ones failed.
func TestDestroy_PartialSessionExportFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessWithExportDir(t, dir)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := seedSessions(t, h, m, 2)

	// Pre-create one of the two expected export files as a directory, so
	// os.WriteFile fails for that specific session while the other succeeds.
	blockedPath := filepath.Join(dir, m.Name+"_"+ids[0]+".json")
	if err := os.Mkdir(blockedPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err = h.svc.Destroy(ctx, h.userID, m.ID, false)
	if !errors.Is(err, machine.ErrSessionExportFailed) {
		t.Fatalf("destroy: got %v, want ErrSessionExportFailed", err)
	}
	if !strings.Contains(err.Error(), ids[0]) {
		t.Errorf("error %q should name the failed session %s", err.Error(), ids[0])
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Errorf("error %q should report 1/2 sessions failed", err.Error())
	}
	// The machine survives, and the other session's file was still written.
	if _, err := h.svc.GetByID(ctx, m.ID); err != nil {
		t.Fatalf("machine should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, m.Name+"_"+ids[1]+".json")); err != nil {
		t.Errorf("session %s should have exported despite the other failing: %v", ids[1], err)
	}
}
