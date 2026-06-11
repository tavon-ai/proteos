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
	// GuestVsockPort is the fixed guest port the in-VM agent listens on; the
	// host reaches it through the jailed vsock uds via the CONNECT/OK handshake
	// (Phase 3 decision #3). Zero defaults to 1024.
	GuestVsockPort int

	// VolumesDir holds the per-machine LUKS container files (<id>.luks), OUTSIDE
	// the jail tree so jail teardown never deletes them (Phase 4 decision #1).
	VolumesDir string
	// CryptsetupBin is the cryptsetup binary; empty defaults to "cryptsetup".
	CryptsetupBin string
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
	diskID, diskMiB := "", 0
	if len(spec.Disks) > 0 {
		diskID, diskMiB = spec.Disks[0].ID, spec.Disks[0].SizeMiB
	}

	rec, existed, err := d.store.Reserve(spec.MachineID, func(a state.Alloc) state.Record {
		return state.Record{
			MachineID: spec.MachineID,
			Handle:    handle,
			State:     api.StateCreating,
			Vcpus:     spec.Vcpus,
			MemMiB:    spec.MemMiB,
			KernelRef: spec.KernelRef,
			RootfsRef: spec.RootfsRef,
			DiskID:    diskID,
			DiskMiB:   diskMiB,
			Boot:      api.BootCold,
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
		// Re-boot a stopped/error/dead machine, reusing its allocation. Keep the
		// disk + snapshot fields (the snapshot, if present, drives resume).
		rec, _, err = d.store.Update(spec.MachineID, func(r *state.Record) {
			r.State = api.StateCreating
			r.Reason = ""
			r.Vcpus = spec.Vcpus
			r.MemMiB = spec.MemMiB
			r.KernelRef = spec.KernelRef
			r.RootfsRef = spec.RootfsRef
			if diskID != "" {
				r.DiskID = diskID
				r.DiskMiB = diskMiB
			}
		})
		if err != nil {
			return "", err
		}
	}

	// The volume key is held only in memory for the duration of this boot — never
	// persisted, never logged (decision #2).
	go d.boot(rec, spec.VolumeKey)
	return handle, nil
}

// boot performs the full async boot (cold or resume) for one reserved machine.
func (d *Driver) boot(rec state.Record, key []byte) {
	if err := d.bootOnce(rec, key); err != nil {
		slog.Error("firecracker boot failed", "machine", rec.MachineID, "err", err)
		_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
			r.State = api.StateError
			r.Reason = err.Error()
		})
		// Best-effort cleanup of partial host state so a retry starts clean.
		d.cleanupHost(rec)
	}
}

// bootOnce decides between resuming from a snapshot and a cold boot, and applies
// the fallback chain: an incompatible/absent snapshot cold-boots; a restore
// error recreates rootfs.ext4 and cold-boots, keeping the disk untouched.
func (d *Driver) bootOnce(rec state.Record, key []byte) error {
	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}

	// A previous (now-dead) boot may have left the volume mounted/open; close it
	// so the upcoming jail wipe never recurses into a live mount. EnsureRunning
	// only reaches here when the VMM is not alive, so this is safe.
	d.closeVolume(rec.MachineID, layout)

	// Resume only when the record carries a snapshot AND the installed
	// Firecracker version matches the one that took it (spike: same-version).
	if rec.Snapshot.Present {
		if v := d.installedFCVersion(); rec.Snapshot.FCVersion != "" && rec.Snapshot.FCVersion == v {
			if err := d.resume(rec, key, layout); err != nil {
				slog.Error("firecracker resume failed; falling back to cold boot",
					"machine", rec.MachineID, "err", err)
				return d.coldBoot(rec, key, layout, "resume failed: "+err.Error())
			}
			return nil
		}
		slog.Warn("snapshot FC version mismatch; cold-booting",
			"machine", rec.MachineID, "snapshot_version", rec.Snapshot.FCVersion, "installed", d.installedFCVersion())
		return d.coldBoot(rec, key, layout, "snapshot fc-version mismatch")
	}
	return d.coldBoot(rec, key, layout, "")
}

// coldBoot lays down a fresh rootfs copy on the (encrypted, persistent) volume,
// provisions the disk on first use, and boots the VM from scratch. fallbackNote,
// when non-empty, is why we cold-booted instead of resuming (recorded as the
// boot reason for the control plane's event payload). The persistent disk
// (data.ext4) is never recreated here — only rootfs.ext4 is.
func (d *Driver) coldBoot(rec state.Record, key []byte, layout jailLayout, fallbackNote string) error {
	uid := d.uidFor(rec)
	kernelSrc := filepath.Join(d.cfg.ImagesDir, rec.KernelRef)
	rootfsSrc := filepath.Join(d.cfg.ImagesDir, rec.RootfsRef)

	// 1. jail scaffolding (no rootfs in the jail — it lives on /state).
	kernelInJail, err := prepareColdJail(layout, kernelSrc)
	if err != nil {
		return err
	}

	// 2. provision (first use) + open + mount the machine volume at /state.
	fresh, err := d.ensureVolumeMounted(rec, key, layout, rootfsSrc)
	if err != nil {
		return err
	}
	if fresh {
		// First ever boot: lay down the persistent disk image the guest mounts as
		// /dev/vdb. The guest never formats — the host does (decision #7).
		if err := truncateFile(layout.statePath(dataArtifact), int64(rec.DiskMiB)*mib); err != nil {
			return err
		}
		if err := run("mkfs.ext4", "-q", "-F", layout.statePath(dataArtifact)); err != nil {
			return fmt.Errorf("mkfs.ext4 data disk: %w", err)
		}
	}
	// Fresh writable rootfs copy on the volume (cold boot = fresh rootfs); any
	// stale snapshot is now invalid.
	if err := copyFile(rootfsSrc, layout.statePath(rootfsArtifact), 0o644); err != nil {
		return fmt.Errorf("copy rootfs onto volume: %w", err)
	}
	consumeSnapshot(layout)

	// 3. chown the whole jail subtree (incl. /state files) to the VMM uid.
	if err := chownRecursive(filepath.Join(d.cfg.ChrootBaseDir, "firecracker", rec.MachineID), uid, uid); err != nil {
		return fmt.Errorf("chown jail: %w", err)
	}

	// 4. host networking.
	if err := d.setupNetworking(rec); err != nil {
		return err
	}

	// 5. launch the jailed VMM.
	pid, err := launchJailer(d.cfg.JailerBin, d.cfg.FirecrackerBin, layout, uid, uid)
	if err != nil {
		return err
	}
	if _, _, err := d.store.Update(rec.MachineID, func(r *state.Record) { r.Pid = pid }); err != nil {
		return err
	}

	// 6. configure the VMM (all PUTs BEFORE InstanceStart — no hot-add).
	apiClient := newFCAPI(layout.socket())
	if err := waitForSocket(layout.socket(), 5*time.Second); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := apiClient.put(ctx, "/machine-config", machineConfig{VcpuCount: rec.Vcpus, MemSizeMiB: rec.MemMiB}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/boot-source", bootSource{KernelImagePath: kernelInJail, BootArgs: bootArgs(rec)}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/drives/rootfs", drive{
		DriveID: "rootfs", PathOnHost: inJailState(rootfsArtifact), IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return err
	}
	// The persistent disk, presented to the guest as /dev/vdb (decision #1/#7).
	if err := apiClient.put(ctx, "/drives/data", drive{
		DriveID: "data", PathOnHost: inJailState(dataArtifact), IsRootDevice: false, IsReadOnly: false,
	}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/network-interfaces/eth0", networkInterface{
		IfaceID: "eth0", GuestMAC: rec.MAC, HostDevName: rec.TapName,
	}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/vsock", vsockDevice{GuestCID: guestCID, UDSPath: vsockUDSName}); err != nil {
		return err
	}
	if err := apiClient.put(ctx, "/actions", action{ActionType: "InstanceStart"}); err != nil {
		return err
	}

	_, _, err = d.store.Update(rec.MachineID, func(r *state.Record) {
		if r.State == api.StateCreating {
			r.State = api.StateRunning
		}
		r.Boot = api.BootCold
		r.Reason = fallbackNote // empty on a normal cold boot
		r.Snapshot = state.SnapshotRecord{}
	})
	return err
}

// resume restores the VM from the snapshot on the persistent volume. The rootfs
// backing file (/state/rootfs.ext4) is the byte-identical one the snapshot
// references, so guest RAM is consistent. The tap is recreated with the SAME
// persisted name and the stale vsock uds removed before LoadSnapshot (spike).
func (d *Driver) resume(rec state.Record, key []byte, layout jailLayout) error {
	uid := d.uidFor(rec)

	// 1. fresh jail scaffolding (the kernel lives in the snapshotted RAM, so it
	// is not reloaded; only the run dir is needed for the API socket).
	if err := prepareResumeJail(layout); err != nil {
		return err
	}

	// 2. open + mount the existing volume; verify the snapshot is actually there.
	if _, err := d.ensureVolumeMounted(rec, key, layout, filepath.Join(d.cfg.ImagesDir, rec.RootfsRef)); err != nil {
		return err
	}
	if !snapshotPresent(layout) {
		return fmt.Errorf("snapshot files missing on volume")
	}

	if err := chownRecursive(filepath.Join(d.cfg.ChrootBaseDir, "firecracker", rec.MachineID), uid, uid); err != nil {
		return fmt.Errorf("chown jail: %w", err)
	}

	// 3. recreate the tap with the SAME name the snapshot's NIC references.
	if err := d.setupNetworking(rec); err != nil {
		return err
	}

	// 4. launch a fresh jailed VMM (no instance started yet).
	pid, err := launchJailer(d.cfg.JailerBin, d.cfg.FirecrackerBin, layout, uid, uid)
	if err != nil {
		return err
	}
	if _, _, err := d.store.Update(rec.MachineID, func(r *state.Record) { r.Pid = pid }); err != nil {
		return err
	}

	apiClient := newFCAPI(layout.socket())
	if err := waitForSocket(layout.socket(), 5*time.Second); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 5. Firecracker re-creates the vsock uds on load; a stale one yields
	// "Address in use" (spike 08), so remove it first.
	_ = os.Remove(layout.vsockUDS())

	if err := loadSnapshot(ctx, apiClient); err != nil {
		return err
	}

	// 6. resume hygiene (decision #9): clock + entropy. Best-effort — a restored
	// VM must not be torn down because the (possibly old) guest lacks /resume.
	if err := d.callGuestResume(ctx, rec.MachineID); err != nil {
		slog.Warn("guest resume hook failed (clock/entropy not corrected)",
			"machine", rec.MachineID, "err", err)
	}

	// 7. the snapshot is consumed: stale RAM must never be restored twice.
	consumeSnapshot(layout)

	_, _, err = d.store.Update(rec.MachineID, func(r *state.Record) {
		if r.State == api.StateCreating {
			r.State = api.StateRunning
		}
		r.Boot = api.BootResumed
		r.Reason = ""
		r.Snapshot = state.SnapshotRecord{} // consumed
	})
	return err
}

// setupNetworking installs the tap + egress policy for a machine.
func (d *Driver) setupNetworking(rec state.Record) error {
	gwCIDR, guestCIDR, err := cidrs(rec)
	if err != nil {
		return err
	}
	return setupTap(rec.TapName, gwCIDR, guestCIDR)
}

// Stop shuts down a running machine asynchronously. StopModeHibernate pauses the
// VM and takes a Full snapshot onto the encrypted volume before tearing down (so
// Start can resume); StopModePoweroff is the cold path (SendCtrlAltDel, then kill
// after stopGrace). Hibernate falls back to the cold path automatically if
// snapshotting fails, so the verb never wedges a machine.
func (d *Driver) Stop(ctx context.Context, machineID string, mode driver.StopMode) error {
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
	hibernate := mode != driver.StopModePoweroff // default + explicit hibernate
	transition := api.StateStopping
	if hibernate {
		transition = api.StateHibernating
	}
	if _, _, err := d.store.Update(machineID, func(r *state.Record) { r.State = transition }); err != nil {
		return err
	}
	go d.finishStop(rec, hibernate)
	return nil
}

// finishStop tears a machine down. On hibernate it pauses + snapshots to the
// volume, then kills the VMM and closes the volume, recording the snapshot
// metadata. On poweroff (or a hibernate that failed to snapshot) it does a cold
// SendCtrlAltDel + kill and clears any snapshot. The volume is always closed.
func (d *Driver) finishStop(rec state.Record, hibernate bool) {
	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}
	apiClient := newFCAPI(layout.socket())

	snap := state.SnapshotRecord{}
	if hibernate {
		ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
		memBytes, err := pauseAndSnapshot(ctx, apiClient, layout)
		cancel()
		if err != nil {
			slog.Error("hibernate snapshot failed; falling back to cold poweroff",
				"machine", rec.MachineID, "err", err)
		} else {
			snap = state.SnapshotRecord{
				Present:   true,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				FCVersion: d.installedFCVersion(),
				MemBytes:  memBytes,
			}
		}
	}

	if !snap.Present {
		// Cold path: orderly guest shutdown, then hard-kill on grace expiry.
		ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
		_ = apiClient.put(ctx, "/actions", action{ActionType: "SendCtrlAltDel"})
		cancel()
		deadline := time.Now().Add(stopGrace)
		for time.Now().Before(deadline) {
			if !processAlive(rec.Pid) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Kill the VMM (paused-after-snapshot or unresponsive-after-CtrlAltDel).
	if processAlive(rec.Pid) {
		_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
	}

	// Close the volume: umount /state + luksClose. The snapshot (if any) is now
	// sealed inside the encrypted container.
	d.closeVolume(rec.MachineID, layout)

	_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
		r.State = api.StateStopped
		r.Pid = 0
		r.Snapshot = snap
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

// Destroy stops the VMM and removes the tap, egress rules, jail, the encrypted
// volume (disk + snapshot), and state.
func (d *Driver) Destroy(ctx context.Context, machineID string) error {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return err
	}
	if ok {
		if rec.Pid > 0 && processAlive(rec.Pid) {
			_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
		}
		layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}
		d.cleanupHost(rec)
		d.destroyVolume(rec.MachineID, layout) // also removes the .luks container
	}
	return d.store.Delete(machineID)
}

// cleanupHost closes the volume (umount /state + luksClose) and removes the tap +
// egress rules and the jail subtree for rec. It does NOT delete the volume
// container — the persistent disk survives a failed boot (only Destroy removes
// it). Closing before removeJail ensures the rm never recurses into a live mount.
func (d *Driver) cleanupHost(rec state.Record) {
	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}
	d.closeVolume(rec.MachineID, layout)
	teardownTap(rec.TapName)
	_ = removeJail(layout)
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
		layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: rec.MachineID}
		switch rec.State {
		case api.StateCreating, api.StateRunning:
			// A live VMM keeps its volume mount + mapper (kernel state, not process
			// state), so it stays adoptable across an agent restart — nothing to do.
			if !alive {
				// The VMM died while we were down: release its volume so a later
				// re-boot/Destroy starts clean, and mark it errored.
				d.closeVolume(rec.MachineID, layout)
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = api.StateError
					r.Reason = "VMM not running after agent restart"
					r.Pid = 0
				})
			}
		case api.StateStopping, api.StateHibernating:
			hibernate := rec.State == api.StateHibernating
			if alive {
				go d.finishStop(rec, hibernate)
			} else {
				// Stop was interrupted mid-flight. The VMM is gone but the volume
				// may still be mounted; close it and settle as stopped. The
				// snapshot was never confirmed (finishStop records it only on
				// success), so clear it — the next boot is a cold boot.
				d.closeVolume(rec.MachineID, layout)
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = api.StateStopped
					r.Pid = 0
					r.Snapshot = state.SnapshotRecord{}
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
		Boot:      rec.Boot,
		DiskID:    rec.DiskID,
		Snapshot: driver.Snapshot{
			Present:   rec.Snapshot.Present,
			CreatedAt: rec.Snapshot.CreatedAt,
			FCVersion: rec.Snapshot.FCVersion,
			MemBytes:  rec.Snapshot.MemBytes,
		},
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
