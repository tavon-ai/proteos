//go:build firecracker && linux

// Package firecracker is the production VM backend: it boots jailed Firecracker
// microVMs over the raw Firecracker API (HTTP on a unix socket), per the Phase 2
// decision to use the raw API rather than firecracker-containerd. It is built
// only with `-tags=firecracker` on linux; the default node-agent build ships
// the dev driver instead. The implementation mirrors spike/firecracker/lib.sh
// and 06-jailer.sh, hardened to the plan's requirements (jailer from first boot,
// default-deny egress, fresh rootfs per boot, re-attach on restart).
package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

// stopGrace bounds how long a graceful SendCtrlAltDel is given before the VMM is
// hard-killed.
const stopGrace = 10 * time.Second

// Config carries the host paths and uid range the driver needs.
type Config struct {
	FirecrackerBin string
	JailerBin      string
	ChrootBaseDir  string
	ImagesDir      string
	JailUIDStart   int
	JailUIDCount   int
}

// Driver implements driver.Driver against jailed Firecracker VMMs.
type Driver struct {
	cfg   Config
	store *state.Store
}

// New constructs the Firecracker driver.
func New(cfg Config, store *state.Store) *Driver {
	return &Driver{cfg: cfg, store: store}
}

var _ driver.Driver = (*Driver)(nil)

// EnsureRunning reserves network resources, then boots the VM asynchronously
// (chroot prep + jailer + API config + InstanceStart). Idempotent: a machine
// already booting/running with a live VMM returns its handle unchanged.
func (d *Driver) EnsureRunning(ctx context.Context, spec driver.VMSpec) (string, error) {
	handle := state.Handle(spec.MachineID)

	rec, existed, err := d.store.Reserve(spec.MachineID, func(a state.Alloc) state.Record {
		return state.Record{
			MachineID: spec.MachineID,
			Handle:    handle,
			State:     api.StateCreating,
			Vcpus:     spec.Vcpus,
			MemMiB:    spec.MemMiB,
			KernelRef: spec.KernelRef,
			RootfsRef: spec.RootfsRef,
			TapName:   a.TapName,
			GuestIP:   a.GuestIP.String(),
			GatewayIP: a.GatewayIP.String(),
			MAC:       a.MAC,
		}
	})
	if err != nil {
		return "", err
	}

	if existed {
		if (rec.State == api.StateCreating || rec.State == api.StateRunning) && processAlive(rec.Pid) {
			return handle, nil // already up
		}
		// Re-boot a stopped/error/dead machine, reusing its allocation.
		rec, _, err = d.store.Update(spec.MachineID, func(r *state.Record) {
			r.State = api.StateCreating
			r.Reason = ""
			r.Vcpus = spec.Vcpus
			r.MemMiB = spec.MemMiB
			r.KernelRef = spec.KernelRef
			r.RootfsRef = spec.RootfsRef
		})
		if err != nil {
			return "", err
		}
	}

	go d.boot(rec)
	return handle, nil
}

// boot performs the full async boot for one reserved machine.
func (d *Driver) boot(rec state.Record) {
	if err := d.bootOnce(rec); err != nil {
		slog.Error("firecracker boot failed", "machine", rec.MachineID, "err", err)
		_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
			r.State = api.StateError
			r.Reason = err.Error()
		})
		// Best-effort cleanup of partial host state so a retry starts clean.
		d.cleanupHost(rec)
	}
}

func (d *Driver) bootOnce(rec state.Record) error {
	uid := d.uidFor(rec)
	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}

	// 1. chroot prep: kernel + fresh rootfs copy, chowned to the VMM uid.
	kernelSrc := filepath.Join(d.cfg.ImagesDir, rec.KernelRef)
	rootfsSrc := filepath.Join(d.cfg.ImagesDir, rec.RootfsRef)
	kernelInJail, rootfsInJail, err := prepareChroot(layout, kernelSrc, rootfsSrc, uid, uid)
	if err != nil {
		return err
	}

	// 2. host networking: tap + gateway + default-deny egress + masquerade.
	gwCIDR, guestCIDR, err := cidrs(rec)
	if err != nil {
		return err
	}
	if err := setupTap(rec.TapName, gwCIDR, guestCIDR); err != nil {
		return err
	}

	// 3. launch the jailed VMM.
	pid, err := launchJailer(d.cfg.JailerBin, d.cfg.FirecrackerBin, layout, uid, uid)
	if err != nil {
		return err
	}
	if _, _, err := d.store.Update(rec.MachineID, func(r *state.Record) { r.Pid = pid }); err != nil {
		return err
	}

	// 4. configure the VMM (all PUTs BEFORE InstanceStart — no hot-add).
	apiClient := newFCAPI(layout.socket())
	if err := waitForSocket(layout.socket(), 5*time.Second); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := apiClient.put(ctx, "/machine-config", machineConfig{VcpuCount: rec.Vcpus, MemSizeMiB: rec.MemMiB}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/boot-source", bootSource{
		KernelImagePath: kernelInJail,
		BootArgs:        bootArgs(rec),
	}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/drives/rootfs", drive{
		DriveID: "rootfs", PathOnHost: rootfsInJail, IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/network-interfaces/eth0", networkInterface{
		IfaceID: "eth0", GuestMAC: rec.MAC, HostDevName: rec.TapName,
	}); err != nil {
		return err
	}

	// 5. start the instance.
	if err := apiClient.put(ctx, "/actions", action{ActionType: "InstanceStart"}); err != nil {
		return err
	}

	_, _, err = d.store.Update(rec.MachineID, func(r *state.Record) {
		if r.State == api.StateCreating {
			r.State = api.StateRunning
		}
	})
	return err
}

// Stop gracefully shuts down a running machine (SendCtrlAltDel, then kill after
// stopGrace), asynchronously.
func (d *Driver) Stop(ctx context.Context, machineID string) error {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return err
	}
	if !ok {
		return driver.ErrUnknownMachine
	}
	if rec.State == api.StateStopped {
		return nil
	}
	if _, _, err := d.store.Update(machineID, func(r *state.Record) { r.State = api.StateStopping }); err != nil {
		return err
	}
	go d.finishStop(rec)
	return nil
}

func (d *Driver) finishStop(rec state.Record) {
	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
	defer cancel()

	// Graceful: Ctrl+Alt+Del triggers an orderly guest shutdown.
	apiClient := newFCAPI(layout.socket())
	_ = apiClient.put(ctx, "/actions", action{ActionType: "SendCtrlAltDel"})

	// Wait for the VMM to exit, hard-killing if it overstays the grace window.
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if !processAlive(rec.Pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if processAlive(rec.Pid) {
		_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
	}

	_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
		r.State = api.StateStopped
		r.Pid = 0
	})
}

// Status returns the driver-level status from the persisted record.
func (d *Driver) Status(ctx context.Context, machineID string) (driver.Status, error) {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return driver.Status{}, err
	}
	if !ok {
		return driver.Status{}, driver.ErrUnknownMachine
	}
	return statusOf(rec), nil
}

// Destroy stops the VMM and removes the tap, egress rules, jail, and state.
func (d *Driver) Destroy(ctx context.Context, machineID string) error {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return err
	}
	if ok {
		if rec.Pid > 0 && processAlive(rec.Pid) {
			_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
		}
		d.cleanupHost(rec)
	}
	return d.store.Delete(machineID)
}

// cleanupHost removes the tap + egress rules and the jail subtree for rec.
func (d *Driver) cleanupHost(rec state.Record) {
	teardownTap(rec.TapName)
	_ = removeJail(jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID})
}

// List returns the status of every tracked machine.
func (d *Driver) List(ctx context.Context) ([]driver.Status, error) {
	recs, err := d.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]driver.Status, 0, len(recs))
	for _, r := range recs {
		out = append(out, statusOf(r))
	}
	return out, nil
}

// Reattach reconciles persisted records with the live VMMs after an agent
// restart: a jailed firecracker whose pid is still alive is re-adopted; a dead
// one becomes error (crash) or stopped (mid-shutdown).
func (d *Driver) Reattach(ctx context.Context) error {
	recs, err := d.store.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		alive := rec.Pid > 0 && processAlive(rec.Pid)
		switch rec.State {
		case api.StateCreating, api.StateRunning:
			if !alive {
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = api.StateError
					r.Reason = "VMM not running after agent restart"
					r.Pid = 0
				})
			}
		case api.StateStopping:
			if alive {
				go d.finishStop(rec)
			} else {
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = api.StateStopped
					r.Pid = 0
				})
			}
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

// uidFor maps a machine to a per-VM uid inside the configured range, keyed off
// the last octet of its guest IP (unique per host subnet).
func (d *Driver) uidFor(rec state.Record) int {
	addr, err := netip.ParseAddr(rec.GuestIP)
	if err != nil || !addr.Is4() {
		return d.cfg.JailUIDStart
	}
	last := int(addr.As4()[3])
	return d.cfg.JailUIDStart + (last % max(1, d.cfg.JailUIDCount))
}

// cidrs returns the gateway address as host-CIDR (e.g. 172.30.0.1/24) and the
// guest's /32, derived from the persisted allocation.
func cidrs(rec state.Record) (gatewayCIDR, guestCIDR string, err error) {
	gw, err := netip.ParseAddr(rec.GatewayIP)
	if err != nil {
		return "", "", fmt.Errorf("bad gateway %q: %w", rec.GatewayIP, err)
	}
	guest, err := netip.ParseAddr(rec.GuestIP)
	if err != nil {
		return "", "", fmt.Errorf("bad guest ip %q: %w", rec.GuestIP, err)
	}
	return gw.String() + "/24", guest.String() + "/32", nil
}

// bootArgs builds the kernel command line with a static IP (spike's scheme), so
// the guest needs no DHCP: ip=<guest>::<gw>:<mask>::eth0:off.
func bootArgs(rec state.Record) string {
	return fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.0::eth0:off",
		rec.GuestIP, rec.GatewayIP)
}

func statusOf(rec state.Record) driver.Status {
	return driver.Status{
		MachineID: rec.MachineID,
		State:     rec.State,
		Reason:    rec.Reason,
		Handle:    rec.Handle,
		GuestIP:   rec.GuestIP,
	}
}

// waitForSocket blocks until the jailed API socket exists or timeout elapses.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for API socket %s", path)
}

// processAlive reports whether pid refers to a live process (signal 0 probe).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
