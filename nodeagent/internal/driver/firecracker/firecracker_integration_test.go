//go:build firecracker && linux

package firecracker_test

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/driver/firecracker"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

// These tests boot REAL Firecracker microVMs and therefore need KVM, root, the
// jailer/firecracker binaries, and pinned kernel+rootfs images. They run on the
// Proxmox self-hosted KVM runner (CI job in Task 2.8) and skip everywhere else.
//
// Required env:
//
//	PROTEOS_TEST_KERNEL   absolute path to a vmlinux
//	PROTEOS_TEST_ROOTFS   absolute path to an ext4 rootfs
//	PROTEOS_FIRECRACKER_BIN / PROTEOS_JAILER_BIN (optional; default /usr/local/bin/*)
//
// The images are copied into each VM's jail, so the originals are untouched.

// testDriver returns the driver plus the volumes dir and chroot base dir, which
// the hibernate test needs to locate the raw LUKS container and the mounted
// /state for its at-rest-encryption assertions.
func testDriver(t *testing.T) (d *firecracker.Driver, volumesDir, chrootBaseDir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("firecracker integration tests require root (jailer + netlink)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present")
	}
	kernel := os.Getenv("PROTEOS_TEST_KERNEL")
	rootfs := os.Getenv("PROTEOS_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set PROTEOS_TEST_KERNEL and PROTEOS_TEST_ROOTFS to run firecracker integration tests")
	}
	fcBin := envOr("PROTEOS_FIRECRACKER_BIN", "/usr/local/bin/firecracker")
	jailerBin := envOr("PROTEOS_JAILER_BIN", "/usr/local/bin/jailer")
	for _, b := range []string{fcBin, jailerBin} {
		if _, err := os.Stat(b); err != nil {
			t.Skipf("binary %s not found", b)
		}
	}

	// The driver copies images by their refs out of ImagesDir; point ImagesDir
	// at a temp dir holding symlinks named by the refs we pass in VMSpec.
	imagesDir := t.TempDir()
	mustSymlink(t, kernel, imagesDir+"/vmlinux")
	mustSymlink(t, rootfs, imagesDir+"/rootfs.ext4")

	store, err := state.NewStore(t.TempDir(), netip.MustParsePrefix("172.30.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	chrootBaseDir = t.TempDir()
	volumesDir = t.TempDir() // Phase 4: per-machine LUKS containers
	d = firecracker.New(firecracker.Config{
		FirecrackerBin: fcBin,
		JailerBin:      jailerBin,
		ChrootBaseDir:  chrootBaseDir,
		ImagesDir:      imagesDir,
		JailUIDStart:   100000,
		JailUIDCount:   1000,
		VolumesDir:     volumesDir,
		CryptsetupBin:  envOr("PROTEOS_CRYPTSETUP_BIN", "cryptsetup"),
	}, store)
	return d, volumesDir, chrootBaseDir
}

func waitState(t *testing.T, d *firecracker.Driver, id, want string, timeout time.Duration) driver.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var st driver.Status
	for time.Now().Before(deadline) {
		var err error
		st, err = d.Status(context.Background(), id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.State == want {
			return st
		}
		if st.State == api.StateError {
			t.Fatalf("machine entered error: %s", st.Reason)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("machine %s never reached %q (last=%q)", id, want, st.State)
	return st
}

func TestBootToRunning(t *testing.T) {
	d, _, _ := testDriver(t)
	id := "aaaaaaaa-0000-0000-0000-000000000001"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })

	st := waitState(t, d, id, api.StateRunning, 30*time.Second)
	if st.GuestIP == "" {
		t.Fatalf("running machine has no guest IP")
	}
}

func TestStopGraceful(t *testing.T) {
	d, _, _ := testDriver(t)
	id := "bbbbbbbb-0000-0000-0000-000000000002"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	if err := d.Stop(ctx, id, driver.StopModePoweroff); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitState(t, d, id, api.StateStopped, 20*time.Second)
}

// TestHibernateResumeCycle proves the Phase 4 driver mechanics on real
// Firecracker: cold boot → hibernate (Full snapshot onto the encrypted volume,
// volume closed afterward) → resume (boot=resumed, snapshot consumed). The
// in-guest file/process survival proof is the 4.6 acceptance pass (it needs a
// rootfs with the 4.3 guest agent baked in); here we verify the volume +
// snapshot lifecycle the control plane observes via Status.
func TestHibernateResumeCycle(t *testing.T) {
	d, volumesDir, chrootBaseDir := testDriver(t)
	id := "dddddddd-0000-0000-0000-000000000044"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	st := waitState(t, d, id, api.StateRunning, 30*time.Second)
	if st.Boot != api.BootCold {
		t.Fatalf("first boot=%q, want cold", st.Boot)
	}

	// Drop a known plaintext marker onto the mounted (open) encrypted volume, so
	// that after hibernate we can prove it is NOT readable in the raw container —
	// i.e. the disk + snapshot are encrypted at rest. The volume is mounted at the
	// jail's /state (<chrootBaseDir>/firecracker/<id>/root/state).
	const probe = "PROTEOS-PLAINTEXT-PROBE-dddddddd-d34db33f"
	mountPoint := filepath.Join(chrootBaseDir, "firecracker", id, "root", "state")
	probeFile := filepath.Join(mountPoint, "plaintext-probe.txt")
	if err := os.WriteFile(probeFile, []byte(probe+"\n"), 0o600); err != nil {
		t.Fatalf("write plaintext probe onto mounted volume %s: %v", mountPoint, err)
	}
	if b, err := os.ReadFile(probeFile); err != nil || !strings.Contains(string(b), probe) {
		t.Fatalf("probe not present in plaintext on the open volume: %v", err)
	}
	_ = exec.Command("sync").Run()

	// Hibernate: pause + snapshot, then volume closed.
	if err := d.Stop(ctx, id, driver.StopModeHibernate); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	st = waitState(t, d, id, api.StateStopped, 30*time.Second)
	if !st.Snapshot.Present || st.Snapshot.FCVersion == "" || st.Snapshot.MemBytes == 0 {
		t.Fatalf("snapshot metadata incomplete after hibernate: %+v", st.Snapshot)
	}

	// The LUKS mapper must be closed after stop (cryptsetup status != active).
	mapper := "proteos-dddddddd" // proteos-<id8>
	out, _ := exec.Command("cryptsetup", "status", mapper).CombinedOutput()
	if strings.Contains(string(out), "is active") {
		t.Fatalf("volume mapper %s still active after hibernate:\n%s", mapper, out)
	}

	// --- encrypted at rest (acceptance criterion) ---------------------------
	volPath := filepath.Join(volumesDir, id+".luks")
	// It must be a LUKS2 container, not a bare ext4.
	if luksOut, err := exec.Command("cryptsetup", "isLuks", "--type", "luks2", volPath).CombinedOutput(); err != nil {
		t.Fatalf("volume %s is not a LUKS2 container: %v: %s", volPath, err, luksOut)
	}
	// The plaintext probe written onto the open volume must NOT be greppable in
	// the raw, closed container. grep exits 1 (not found) on success.
	switch err := exec.Command("grep", "-a", "-q", probe, volPath).Run(); e := err.(type) {
	case nil:
		t.Fatalf("PLAINTEXT LEAK: probe %q found in raw closed container %s", probe, volPath)
	case *exec.ExitError:
		if e.ExitCode() != 1 {
			t.Fatalf("grep over container %s errored unexpectedly (exit %d)", volPath, e.ExitCode())
		}
	default:
		t.Fatalf("grep over container %s: %v", volPath, err)
	}

	// Resume from the snapshot.
	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("resume ensure: %v", err)
	}
	st = waitState(t, d, id, api.StateRunning, 30*time.Second)
	if st.Boot != api.BootResumed {
		t.Fatalf("after resume boot=%q, want resumed", st.Boot)
	}
	if st.Snapshot.Present {
		t.Fatalf("snapshot should be consumed after resume")
	}

	// --- resume reseeds entropy + resyncs the clock (acceptance criterion) ---
	// The guest /resume hook (decision #9) sets the wall clock and reseeds the
	// CRNG; a 200 from it lands as ResumeHygiene="ok". This needs a rootfs with
	// the 4.3 guest agent baked in (the ansible-baked image), so it is enforced
	// only when the runner declares that via the env flag; otherwise the bare
	// mechanics above still run.
	if os.Getenv("PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT") != "" {
		if st.ResumeHygiene != "ok" {
			t.Fatalf("resume hygiene not ok — clock/entropy not corrected: hygiene=%q skew=%dms",
				st.ResumeHygiene, st.ResumeSkewMS)
		}
		t.Logf("resume hygiene ok: guest corrected %dms of clock skew + reseeded entropy", st.ResumeSkewMS)
	} else {
		t.Log("clock/entropy assertion skipped; set PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1 (baked rootfs) to enforce")
	}
}

func TestEgressDefaultDeny(t *testing.T) {
	d, _, _ := testDriver(t)
	id := "cccccccc-0000-0000-0000-000000000003"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	st := waitState(t, d, id, api.StateRunning, 30*time.Second)

	// The default-deny egress policy must be installed in the proteos nft table:
	// guest→RFC1918 drops plus a masquerade for the guest. We assert the rules
	// exist (the in-guest reachability check is part of the Proxmox acceptance
	// pass, which needs guest console/SSH access).
	rules, err := exec.Command("nft", "list", "table", "ip", "proteos").CombinedOutput()
	if err != nil {
		t.Fatalf("nft list: %v: %s", err, rules)
	}
	ruleset := string(rules)
	for _, want := range []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.0.0/16", "drop", "masquerade"} {
		if !strings.Contains(ruleset, want) {
			t.Fatalf("egress policy missing %q in:\n%s", want, ruleset)
		}
	}
	_ = st
}

func spec(id string) driver.VMSpec {
	return driver.VMSpec{
		MachineID: id, Vcpus: 1, MemMiB: 256, KernelRef: "vmlinux", RootfsRef: "rootfs.ext4",
		// Phase 4: a persistent disk + the volume key the driver luksOpens with.
		Disks:     []driver.Disk{{ID: "disk-" + id, SizeMiB: 512}},
		VolumeKey: []byte("0123456789abcdef0123456789abcdef"),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatal(err)
	}
}
