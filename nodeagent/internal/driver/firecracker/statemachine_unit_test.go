//go:build firecracker && linux

package firecracker

// Unit tests for the driver state machine (boot/stop/reattach) that run
// WITHOUT root or KVM: the host toolchain is replaced through the commandRunner
// seam (exec.go) and the Firecracker API socket by an in-process HTTP server on
// a real unix socket. These are internal-package tests (they install fakes into
// the package seams), so they run in ordinary CI; the root+KVM integration
// tests keep covering the real jailer/nft/cryptsetup path.
//
// The tests mutate package-level seams (cmds, processAlive, ...), so none of
// them may call t.Parallel().

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
	"github.com/tavon-ai/proteos/nodeagent/internal/state"
)

// --- fakes -------------------------------------------------------------------

// fakeRunner records every command and answers via respond. The zero respond
// succeeds with empty output for everything.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(name string, args ...string) (string, error)
}

func (f *fakeRunner) exec(name string, args []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.respond == nil {
		return "", nil
	}
	return f.respond(name, args...)
}

func (f *fakeRunner) CombinedOutput(_ []byte, name string, args ...string) ([]byte, error) {
	out, err := f.exec(name, args)
	return []byte(out), err
}

func (f *fakeRunner) Output(name string, args ...string) ([]byte, error) {
	out, err := f.exec(name, args)
	return []byte(out), err
}

// saw reports whether any recorded command starts with the given tokens.
func (f *fakeRunner) saw(prefix ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if c[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// hostToolchain answers the probe commands the boot path issues the way a
// pristine host would: no prior taps, chains, or rules; pgrep finds the "VMM"
// (this test process, so the real liveness probe sees it alive).
func hostToolchain(name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	switch {
	case cmd == "ip route get 8.8.8.8":
		return "8.8.8.8 via 192.0.2.1 dev eth0 src 192.0.2.10 uid 0", nil
	case name == "pgrep":
		return strconv.Itoa(os.Getpid()) + "\n", nil
	case strings.HasPrefix(cmd, "nft -a list chain"): // deleteRulesByComment: nothing to delete
		return "", errors.New("No such file or directory")
	case cmd == "nft list chain ip filter FORWARD": // no Docker/ufw FORWARD chain
		return "", errors.New("No such file or directory")
	case strings.HasPrefix(cmd, "ip link show"): // tap does not exist yet
		return "", errors.New("does not exist")
	default:
		return "", nil
	}
}

// installFakes swaps every seam for the test's lifetime. alive is the fake
// process-liveness probe; nil keeps the real signal-0 probe.
func installFakes(t *testing.T, r *fakeRunner, alive func(pid int) bool) {
	t.Helper()
	oldCmds, oldAlive, oldKill, oldMounted, oldMapper := cmds, processAlive, killProcess, isMounted, mapperExists
	cmds = r
	if alive != nil {
		processAlive = alive
	}
	killProcess = func(int) error { return nil }
	isMounted = func(string) bool { return false }
	mapperExists = func(string) bool { return false }
	t.Cleanup(func() {
		cmds, processAlive, killProcess, isMounted, mapperExists = oldCmds, oldAlive, oldKill, oldMounted, oldMapper
	})
}

// newUnitDriver builds a Driver over temp dirs with dummy images. The chroot
// base must stay short: the jailed API socket path has to fit in sun_path.
func newUnitDriver(t *testing.T) (*Driver, *state.Store) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "fcunit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	imagesDir := filepath.Join(base, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4"} {
		if err := os.WriteFile(filepath.Join(imagesDir, f), []byte(f+"-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := state.NewStore(filepath.Join(base, "data"), netip.MustParsePrefix("172.30.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	d := New(Config{
		FirecrackerBin: "firecracker",
		JailerBin:      "jailer",
		ChrootBaseDir:  filepath.Join(base, "jail"),
		ImagesDir:      imagesDir,
		// Our own uid so the boot path's chownRecursive works without root.
		JailUIDStart: os.Getuid(),
		JailUIDCount: 1,
		VolumesDir:   filepath.Join(base, "volumes"),
		AgentPort:    "9090",
		MgmtIfaces:   []string{"eth0"},
	}, store)
	return d, store
}

func unitSpec(id string) driver.VMSpec {
	return driver.VMSpec{
		MachineID: id,
		Vcpus:     1,
		MemMiB:    128,
		KernelRef: "vmlinux",
		RootfsRef: "rootfs.ext4",
		Disks:     []driver.Disk{{ID: "disk-1", SizeMiB: 4}},
		VolumeKey: []byte("0123456789abcdef0123456789abcdef"),
	}
}

// seedRecord reserves a record and forces it into the given state/pid.
func seedRecord(t *testing.T, s *state.Store, id, st string, pid int, snap state.SnapshotRecord) state.Record {
	t.Helper()
	spec := unitSpec(id)
	_, _, err := s.Reserve(id, func(a state.Alloc) state.Record {
		return state.Record{
			MachineID: id, Handle: state.Handle(id), State: api.StateCreating,
			Vcpus: spec.Vcpus, MemMiB: spec.MemMiB,
			KernelRef: spec.KernelRef, RootfsRef: spec.RootfsRef,
			DiskID: spec.Disks[0].ID, DiskMiB: spec.Disks[0].SizeMiB,
			Boot: api.BootCold, TapName: a.TapName,
			GuestIP: a.GuestIP.String(), GatewayIP: a.GatewayIP.String(), MAC: a.MAC,
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := s.Update(id, func(r *state.Record) {
		r.State = st
		r.Pid = pid
		r.Snapshot = snap
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// fcAPIRecorder is a fake Firecracker API: as soon as the jail run dir exists
// it listens on the jailed socket path and 204s every request, recording
// "METHOD /path" in order.
type fcAPIRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *fcAPIRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func serveFakeFCAPI(t *testing.T, layout jailLayout) *fcAPIRecorder {
	t.Helper()
	rec := &fcAPIRecorder{}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			default:
			}
			if _, err := os.Stat(filepath.Dir(layout.socket())); err == nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		l, err := net.Listen("unix", layout.socket())
		if err != nil {
			t.Logf("fake fc api: listen: %v", err)
			return
		}
		go func() {
			<-done
			_ = l.Close()
		}()
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rec.mu.Lock()
			rec.requests = append(rec.requests, req.Method+" "+req.URL.Path)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
	}()
	return rec
}

// waitRecordState polls the store until the machine reaches want.
func waitRecordState(t *testing.T, s *state.Store, id, want string) state.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last state.Record
	for time.Now().Before(deadline) {
		rec, ok, err := s.Load(id)
		if err != nil || !ok {
			t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
		}
		last = rec
		if rec.State == want {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("machine %s never reached %q (last=%q reason=%q)", id, want, last.State, last.Reason)
	return last
}

// --- tests -------------------------------------------------------------------

// TestUnitColdBootReachesRunning drives EnsureRunning through the full cold
// boot: jail prep, volume provisioning, networking, jailer launch, and the
// pre-InstanceStart API configuration sequence — all against fakes.
func TestUnitColdBootReachesRunning(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{respond: hostToolchain}
	installFakes(t, run, nil) // real processAlive: the fake pgrep reports our own pid

	const id = "aaaaaaaa-0000-0000-0000-000000000001"
	fcapi := serveFakeFCAPI(t, jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: id})

	if _, err := d.EnsureRunning(context.Background(), unitSpec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	rec := waitRecordState(t, s, id, api.StateRunning)

	if rec.Boot != api.BootCold {
		t.Errorf("boot = %q, want cold", rec.Boot)
	}
	if rec.Pid != os.Getpid() {
		t.Errorf("pid = %d, want %d (from fake pgrep)", rec.Pid, os.Getpid())
	}

	// The state machine must configure everything BEFORE InstanceStart.
	reqs := fcapi.list()
	wantOrder := []string{
		"PUT /machine-config", "PUT /boot-source", "PUT /drives/rootfs",
		"PUT /drives/data", "PUT /network-interfaces/eth0", "PUT /vsock",
		"PUT /actions",
	}
	pos := -1
	for _, want := range wantOrder {
		found := -1
		for i, r := range reqs {
			if r == want {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("API never saw %q (got %v)", want, reqs)
		}
		if found < pos {
			t.Errorf("API request %q arrived out of order (got %v)", want, reqs)
		}
		pos = found
	}

	// Host side-effects: jailed launch, tap creation, and the default-deny
	// egress policy for this tap.
	if !run.saw("jailer", "--id", id) {
		t.Error("jailer was never launched")
	}
	if !run.saw("ip", "tuntap", "add", rec.TapName) {
		t.Error("tap was never created")
	}
	if !run.saw("nft", "add", "table", "ip", "proteos") {
		t.Error("nft table was never ensured")
	}
	if !run.saw("cryptsetup", "luksFormat") {
		t.Error("volume was never luksFormat-ed")
	}
	if !run.saw("mount", "-t", "ext4") {
		t.Error("volume was never mounted")
	}
}

// TestUnitBootFailureSetsError proves a failed boot lands in error with the
// cause recorded, instead of wedging in creating.
func TestUnitBootFailureSetsError(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{respond: func(name string, args ...string) (string, error) {
		if name == "jailer" {
			return "jailer exploded", errors.New("exit status 1")
		}
		return hostToolchain(name, args...)
	}}
	installFakes(t, run, nil)

	const id = "aaaaaaaa-0000-0000-0000-000000000002"
	if _, err := d.EnsureRunning(context.Background(), unitSpec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	rec := waitRecordState(t, s, id, api.StateError)
	if !strings.Contains(rec.Reason, "jailer") {
		t.Errorf("reason = %q, want the jailer failure surfaced", rec.Reason)
	}
}

// TestUnitEnsureRunningAdoptsLiveVMM: a record in running with a live VMM pid
// is re-adopted as-is — no second boot, no host commands.
func TestUnitEnsureRunningAdoptsLiveVMM(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{}
	const livePid = 4242
	installFakes(t, run, func(pid int) bool { return pid == livePid })

	const id = "aaaaaaaa-0000-0000-0000-000000000003"
	seedRecord(t, s, id, api.StateRunning, livePid, state.SnapshotRecord{})

	handle, err := d.EnsureRunning(context.Background(), unitSpec(id))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if handle != state.Handle(id) {
		t.Errorf("handle = %q, want %q", handle, state.Handle(id))
	}
	// Give a mistaken boot goroutine a beat to do damage, then check none did.
	time.Sleep(50 * time.Millisecond)
	rec, _, _ := s.Load(id)
	if rec.State != api.StateRunning || rec.Pid != livePid {
		t.Errorf("record mutated: state=%q pid=%d, want running/%d", rec.State, rec.Pid, livePid)
	}
	if n := run.callCount(); n != 0 {
		t.Errorf("adopting a live VMM ran %d host commands, want 0", n)
	}
}

// TestUnitStopPoweroff: a poweroff of a machine whose VMM is already gone
// settles in stopped with the pid and snapshot cleared.
func TestUnitStopPoweroff(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{respond: hostToolchain}
	installFakes(t, run, func(int) bool { return false })

	const id = "aaaaaaaa-0000-0000-0000-000000000004"
	seedRecord(t, s, id, api.StateRunning, 12345,
		state.SnapshotRecord{Present: true, FCVersion: "v1.0.0"})

	if err := d.Stop(context.Background(), id, driver.StopModePoweroff); err != nil {
		t.Fatalf("stop: %v", err)
	}
	rec := waitRecordState(t, s, id, api.StateStopped)
	if rec.Pid != 0 {
		t.Errorf("pid = %d, want 0", rec.Pid)
	}
	if rec.Snapshot.Present {
		t.Error("poweroff must clear the snapshot record")
	}
}

// TestUnitStopHibernateFallsBackToPoweroff: when the snapshot cannot be taken
// (here: the VMM API socket is gone), hibernate falls back to the cold path and
// records NO snapshot — the verb never wedges the machine.
func TestUnitStopHibernateFallsBackToPoweroff(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{respond: hostToolchain}
	installFakes(t, run, func(int) bool { return false })

	const id = "aaaaaaaa-0000-0000-0000-000000000005"
	seedRecord(t, s, id, api.StateRunning, 12345, state.SnapshotRecord{})

	if err := d.Stop(context.Background(), id, driver.StopModeHibernate); err != nil {
		t.Fatalf("stop: %v", err)
	}
	rec := waitRecordState(t, s, id, api.StateStopped)
	if rec.Snapshot.Present {
		t.Error("failed hibernate must not record a snapshot")
	}
}

// TestUnitReattach reconciles persisted records against (fake) process
// liveness after an agent restart.
func TestUnitReattach(t *testing.T) {
	d, s := newUnitDriver(t)
	run := &fakeRunner{respond: hostToolchain}
	const livePid = 777
	installFakes(t, run, func(pid int) bool { return pid == livePid })

	const (
		crashed     = "aaaaaaaa-0000-0000-0000-00000000000a" // running, VMM died
		adopted     = "aaaaaaaa-0000-0000-0000-00000000000b" // running, VMM alive
		interrupted = "aaaaaaaa-0000-0000-0000-00000000000c" // hibernating, VMM died mid-stop
	)
	seedRecord(t, s, crashed, api.StateRunning, 1234, state.SnapshotRecord{})
	seedRecord(t, s, adopted, api.StateRunning, livePid, state.SnapshotRecord{})
	seedRecord(t, s, interrupted, api.StateHibernating, 1234,
		state.SnapshotRecord{Present: true, FCVersion: "v1.0.0"})

	if err := d.Reattach(context.Background()); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	rec, _, _ := s.Load(crashed)
	if rec.State != api.StateError || rec.Pid != 0 || rec.Reason == "" {
		t.Errorf("crashed: state=%q pid=%d reason=%q, want error/0/non-empty", rec.State, rec.Pid, rec.Reason)
	}
	rec, _, _ = s.Load(adopted)
	if rec.State != api.StateRunning || rec.Pid != livePid {
		t.Errorf("adopted: state=%q pid=%d, want running/%d", rec.State, rec.Pid, livePid)
	}
	// The interrupted stop settles as stopped with the unconfirmed snapshot
	// cleared — the next boot must be cold.
	rec = waitRecordState(t, s, interrupted, api.StateStopped)
	if rec.Snapshot.Present {
		t.Error("interrupted hibernate must clear the unconfirmed snapshot")
	}
}
