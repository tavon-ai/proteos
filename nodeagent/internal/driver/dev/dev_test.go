package dev_test

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/driver/dev"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

const fastBoot = 20 * time.Millisecond

func newDriver(t *testing.T, guestAgentBin string) (*dev.DevDriver, *state.Store) {
	t.Helper()
	store, err := state.NewStore(t.TempDir(), netip.MustParsePrefix("172.30.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	return dev.New(store, fastBoot, "", guestAgentBin, ""), store
}

func waitState(t *testing.T, d *dev.DevDriver, id, want string) driver.Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last driver.Status
	for time.Now().Before(deadline) {
		st, err := d.Status(context.Background(), id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("machine %s never reached %q (last=%q reason=%q)", id, want, last.State, last.Reason)
	return last
}

func spec(id string) driver.VMSpec {
	return driver.VMSpec{
		MachineID: id, Vcpus: 2, MemMiB: 512, KernelRef: "k", RootfsRef: "r",
		Disks:     []driver.Disk{{ID: "disk-" + id, SizeMiB: 10240}},
		VolumeKey: []byte("0123456789abcdef0123456789abcdef"),
	}
}

// TestHibernateResumeMetadata walks cold-boot → hibernate → resume and asserts
// the snapshot metadata and boot kind round-trip through the on-disk store.
func TestHibernateResumeMetadata(t *testing.T) {
	d, _ := newDriver(t, "")
	ctx := context.Background()
	id := "aaaaaaaa-0000-0000-0000-000000000001"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })

	st := waitState(t, d, id, api.StateRunning)
	if st.Boot != api.BootCold {
		t.Fatalf("first boot: got %q, want cold", st.Boot)
	}
	if st.DiskID != "disk-"+id {
		t.Fatalf("disk id: got %q", st.DiskID)
	}
	if st.Snapshot.Present {
		t.Fatalf("running machine should have no snapshot")
	}

	// Hibernate: → hibernating → stopped, with a fake snapshot recorded.
	if err := d.Stop(ctx, id, driver.StopModeHibernate); err != nil {
		t.Fatalf("stop hibernate: %v", err)
	}
	st = waitState(t, d, id, api.StateStopped)
	if !st.Snapshot.Present {
		t.Fatalf("hibernated machine should carry a snapshot")
	}
	if st.Snapshot.FCVersion != "dev" {
		t.Fatalf("snapshot fc_version: got %q, want dev", st.Snapshot.FCVersion)
	}
	if st.Snapshot.MemBytes != 512*1024*1024 {
		t.Fatalf("snapshot mem_bytes: got %d", st.Snapshot.MemBytes)
	}
	if st.Snapshot.CreatedAt == "" {
		t.Fatalf("snapshot created_at should be set")
	}

	// Resume: ensure again consumes the snapshot and reports boot=resumed.
	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, d, id, api.StateRunning)
	if st.Boot != api.BootResumed {
		t.Fatalf("after resume: boot=%q, want resumed", st.Boot)
	}
	if st.Snapshot.Present {
		t.Fatalf("snapshot should be consumed after resume")
	}
}

// TestPoweroffClearsSnapshot proves a cold stop (poweroff) leaves no snapshot,
// so the next ensure cold-boots rather than resuming.
func TestPoweroffClearsSnapshot(t *testing.T) {
	d, _ := newDriver(t, "")
	ctx := context.Background()
	id := "bbbbbbbb-0000-0000-0000-000000000002"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning)

	if err := d.Stop(ctx, id, driver.StopModePoweroff); err != nil {
		t.Fatalf("stop poweroff: %v", err)
	}
	st := waitState(t, d, id, api.StateStopped)
	if st.Snapshot.Present {
		t.Fatalf("poweroff should not record a snapshot")
	}

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, d, id, api.StateRunning)
	if st.Boot != api.BootCold {
		t.Fatalf("after poweroff+ensure: boot=%q, want cold", st.Boot)
	}
}

// TestPersistDirSurvivesHibernate proves the per-machine persist dir (and a file
// the guest writes into it) survives stop/start — the dev stand-in for the real
// driver's persistent disk. It uses a tiny shell script as the "guest agent"
// that touches a file under PROTEOS_GUEST_PERSIST and then sleeps.
func TestPersistDirSurvivesHibernate(t *testing.T) {
	bin := writeFakeGuestAgent(t)
	d, store := newDriver(t, bin)
	ctx := context.Background()
	id := "cccccccc-0000-0000-0000-000000000003"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning)

	proof := filepath.Join(store.MachineDir(id), "persist", "proof")
	waitFile(t, proof)

	if err := d.Stop(ctx, id, driver.StopModeHibernate); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitState(t, d, id, api.StateStopped)
	if _, err := os.Stat(proof); err != nil {
		t.Fatalf("persist proof gone after hibernate: %v", err)
	}

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, d, id, api.StateRunning)
	if st.Boot != api.BootResumed {
		t.Fatalf("boot=%q, want resumed", st.Boot)
	}
	if _, err := os.Stat(proof); err != nil {
		t.Fatalf("persist proof gone after resume: %v", err)
	}
}

func writeFakeGuestAgent(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-guest-agent.sh")
	script := "#!/bin/sh\ntouch \"$PROTEOS_GUEST_PERSIST/proof\"\nexec sleep 2147483647\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared", path)
}
