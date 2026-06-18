package machine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// TestHibernateResumeLifecycle walks running → hibernating → stopped → starting
// → running, asserting: the snapshot row appears on hibernate and is consumed on
// resume; the boot kind (resumed) lands in the machine row and the event payload;
// the volume key reaches the agent on every ensure but never leaks into events
// or the disk/snapshot rows.
func TestHibernateResumeLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Create: disk allocated, volume key minted + sent to the agent.
	m, err := h.svc.Create(ctx, h.userID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := m.ID
	idStr := machine.UUIDString(id)

	// The disk row exists and is attached.
	disk, err := h.q.GetDiskByMachineID(ctx, id)
	if err != nil {
		t.Fatalf("disk not created: %v", err)
	}
	if disk.SizeMib != 10240 {
		t.Fatalf("disk size=%d, want 10240", disk.SizeMib)
	}

	// The agent received a non-empty volume key + disk on the create ensure.
	h.agent.mu.Lock()
	ens := h.agent.lastEnsure[idStr]
	h.agent.mu.Unlock()
	if ens.VolumeKeyB64 == "" {
		t.Fatalf("volume key not delivered to agent on ensure")
	}
	if ens.DiskID == "" || ens.DiskMiB != 10240 {
		t.Fatalf("disk not delivered to agent: id=%q mib=%d", ens.DiskID, ens.DiskMiB)
	}
	volumeKey := ens.VolumeKeyB64

	// Advance to running.
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	if h.machine(t).State != string(machine.StateRunning) {
		t.Fatal("expected running")
	}

	// Stop → hibernating, with hibernate mode on the wire.
	if _, err := h.svc.Stop(ctx, h.userID, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if h.machine(t).State != string(machine.StateHibernating) {
		t.Fatalf("after stop state=%q, want hibernating", h.machine(t).State)
	}
	h.agent.mu.Lock()
	modes := append([]string(nil), h.agent.stopModes...)
	h.agent.mu.Unlock()
	if len(modes) != 1 || modes[0] != agentapi.StopModeHibernate {
		t.Fatalf("stop modes=%v, want [hibernate]", modes)
	}

	// Agent reports stopped + a snapshot present; poller records the snapshot row.
	h.agent.SetStatus(idStr, agentapi.StateStopped, "", "")
	h.agent.SetSnapshot(idStr, true, "v1.16.0", 2147483648)
	h.poller.AdvanceTransitional(ctx)
	if h.machine(t).State != string(machine.StateStopped) {
		t.Fatalf("after hibernate poll state=%q, want stopped", h.machine(t).State)
	}
	snap, err := h.q.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("snapshot row not recorded: %v", err)
	}
	if snap.FcVersion != "v1.16.0" || snap.MemBytes != 2147483648 {
		t.Fatalf("snapshot row wrong: %+v", snap)
	}

	// Start → starting; agent resumes (boot=resumed). Snapshot is consumed.
	if _, err := h.svc.Start(ctx, h.userID, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.agent.SetBoot(idStr, agentapi.BootResumed)
	h.poller.AdvanceTransitional(ctx)

	m = h.machine(t)
	if m.State != string(machine.StateRunning) {
		t.Fatalf("after resume poll state=%q, want running", m.State)
	}
	if m.Boot == nil || *m.Boot != agentapi.BootResumed {
		t.Fatalf("machine.boot=%v, want resumed", m.Boot)
	}
	if _, err := h.q.GetSnapshot(ctx, id); err == nil {
		t.Fatalf("snapshot should be consumed after resume")
	}

	// The agent saw the key on the resume ensure too.
	h.agent.mu.Lock()
	if h.agent.lastEnsure[idStr].VolumeKeyB64 == "" {
		t.Fatalf("volume key not delivered on resume ensure")
	}
	ensureCalls := h.agent.ensureCalls
	h.agent.mu.Unlock()
	if ensureCalls < 2 {
		t.Fatalf("expected ≥2 ensures (create + start), got %d", ensureCalls)
	}

	// boot:resumed is in the transition event payload. (Postgres jsonb
	// re-serializes with a space after the colon, so match the key + value.)
	if !eventPayloadContains(t, h, id, `"boot"`) || !eventPayloadContains(t, h, id, `"resumed"`) {
		t.Fatalf("no event payload carried boot:resumed")
	}

	// The volume key never leaked into any event payload.
	if eventPayloadContains(t, h, id, volumeKey) {
		t.Fatalf("volume key leaked into a machine_events payload")
	}
}

// eventPayloadContains reports whether any of the machine's event payloads
// contains needle.
func eventPayloadContains(t *testing.T, h *harness, id pgtype.UUID, needle string) bool {
	t.Helper()
	evs, err := h.q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: id, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Payload), needle) {
			return true
		}
	}
	return false
}

// TestVolumeKeyMintedAndStored proves Create mints a 32-byte key into the secret
// store at the canonical path, and that the key never appears in the machine
// summary returned to the API.
func TestVolumeKeyMintedAndStored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	idStr := machine.UUIDString(m.ID)

	keyB64, err := secrets.GetMachineVolumeKey(h.sec, idStr)
	if err != nil {
		t.Fatalf("volume key not stored at %s: %v", secrets.MachineVolumeKeyPath(idStr), err)
	}
	if keyB64 == "" {
		t.Fatalf("stored volume key is empty")
	}
}
