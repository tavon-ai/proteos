//go:build firecracker && linux

package firecracker_test

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
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

func testDriver(t *testing.T) (*firecracker.Driver, string) {
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
	d := firecracker.New(firecracker.Config{
		FirecrackerBin: fcBin,
		JailerBin:      jailerBin,
		ChrootBaseDir:  t.TempDir(),
		ImagesDir:      imagesDir,
		JailUIDStart:   100000,
		JailUIDCount:   1000,
	}, store)
	return d, ""
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
	d, _ := testDriver(t)
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
	d, _ := testDriver(t)
	id := "bbbbbbbb-0000-0000-0000-000000000002"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	if err := d.Stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitState(t, d, id, api.StateStopped, 20*time.Second)
}

func TestEgressDefaultDeny(t *testing.T) {
	d, _ := testDriver(t)
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
