//go:build firecracker && linux

package firecracker

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tavon/proteos/nodeagent/internal/state"
)

// The machine volume (Phase 4 decision #1): one LUKS2 container file per machine
// at <VolumesDir>/<id>.luks, opened to /dev/mapper/proteos-<id8>, formatted ext4,
// and mounted by the node-agent at <jail-root>/state. Because the mount is in the
// node-agent's mount namespace and lives *under* the jail root, the chrooted VMM
// sees it as /state and Firecracker only ever opens regular files there
// (path_on_host=/state/...). The container holds ALL mutable per-machine state:
//
//	state/rootfs.ext4   the writable rootfs (fresh copy on cold boot, preserved
//	                    byte-for-byte across hibernate so a snapshot can restore)
//	state/data.ext4     the persistent disk presented to the guest as /dev/vdb
//	state/snap/vmstate  Firecracker Full-snapshot device/vm state (hibernate only)
//	state/snap/mem      guest RAM image (hibernate only)
//
// The key is held by the control plane and delivered on every ensure; it is never
// persisted host-side. cryptsetup reads it from stdin (--key-file=-) so it never
// appears in argv.

const mib = 1024 * 1024

// volumeArtifact names, relative to the volume mount point.
const (
	rootfsArtifact = "rootfs.ext4"
	dataArtifact   = "data.ext4"
	snapDir        = "snap"
	snapVMState    = "snap/vmstate"
	snapMem        = "snap/mem"
)

// volumeFile is the LUKS container path for a machine (outside the jail tree, so
// jail teardown never deletes it).
func (d *Driver) volumeFile(machineID string) string {
	return filepath.Join(d.cfg.VolumesDir, machineID+".luks")
}

// mapperName is the device-mapper name for a machine's opened volume. It must be
// stable across boots (resume reuses it) and fit DM's name limits: proteos-<id8>.
func mapperName(machineID string) string { return "proteos-" + state.ID8(machineID) }

// mapperPath is the /dev/mapper path for an opened volume.
func mapperPath(machineID string) string { return "/dev/mapper/" + mapperName(machineID) }

// mountPoint is where the volume is mounted inside the jail (the VMM sees /state).
func (l jailLayout) mountPoint() string { return filepath.Join(l.root(), "state") }

// statePath joins a volume-relative artifact onto the host mount point.
func (l jailLayout) statePath(artifact string) string {
	return filepath.Join(l.mountPoint(), artifact)
}

// inJail is the artifact path as the chrooted VMM sees it (/state/...).
func inJailState(artifact string) string { return "/state/" + artifact }

// volumeSizeBytes sizes the container to hold the rootfs copy, the persistent
// disk, a full memory snapshot, and slack for ext4 + LUKS overhead (decision #1).
func volumeSizeBytes(rootfsBytes int64, memMiB, diskMiB int) int64 {
	return rootfsBytes + int64(memMiB)*mib + int64(diskMiB)*mib + 512*mib
}

// ensureVolumeMounted provisions the LUKS container on first use, opens it, and
// mounts it at the jail's /state. It is idempotent: an existing container is
// reused, an already-open mapper is not re-opened, and an existing mount is left
// in place. Returns whether the volume was freshly provisioned (the caller then
// lays down data.ext4). The mount point (<root>/state) must already exist.
func (d *Driver) ensureVolumeMounted(rec state.Record, key []byte, layout jailLayout, rootfsSrc string) (fresh bool, err error) {
	if len(key) == 0 {
		return false, fmt.Errorf("volume key missing (control plane must send volume_key_b64 on ensure)")
	}
	volFile := d.volumeFile(rec.MachineID)
	mapper := mapperName(rec.MachineID)

	if !fileExists(volFile) {
		fi, statErr := os.Stat(rootfsSrc)
		if statErr != nil {
			return false, fmt.Errorf("stat rootfs %s: %w", rootfsSrc, statErr)
		}
		size := volumeSizeBytes(fi.Size(), rec.MemMiB, rec.DiskMiB)
		if err := os.MkdirAll(d.cfg.VolumesDir, 0o700); err != nil {
			return false, fmt.Errorf("mkdir volumes dir: %w", err)
		}
		if err := truncateFile(volFile, size); err != nil {
			return false, err
		}
		if err := d.luksFormat(volFile, key); err != nil {
			_ = os.Remove(volFile) // don't leave a half-formatted container
			return false, err
		}
		fresh = true
	}

	if !fileExists(mapperPath(rec.MachineID)) {
		if err := d.luksOpen(volFile, mapper, key); err != nil {
			return false, err
		}
	}
	if fresh {
		if err := run("mkfs.ext4", "-q", "-F", mapperPath(rec.MachineID)); err != nil {
			return false, fmt.Errorf("mkfs.ext4 volume: %w", err)
		}
	}

	mp := layout.mountPoint()
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return false, fmt.Errorf("mkdir mount point: %w", err)
	}
	if !isMounted(mp) {
		if err := run("mount", "-t", "ext4", mapperPath(rec.MachineID), mp); err != nil {
			return false, fmt.Errorf("mount volume: %w", err)
		}
	}
	return fresh, nil
}

// closeVolume unmounts the jail's /state (if mounted) and closes the LUKS mapper
// (if open). Best-effort and idempotent — safe to call before a re-boot or on
// teardown, and safe when nothing is mounted/open.
func (d *Driver) closeVolume(machineID string, layout jailLayout) {
	mp := layout.mountPoint()
	if isMounted(mp) {
		// lazy unmount tolerates a VMM that has not fully released the files yet.
		if err := run("umount", mp); err != nil {
			_ = run("umount", "-l", mp)
		}
	}
	if !fileExists(mapperPath(machineID)) {
		return
	}
	// The device-mapper / loop backing can hold its last reference for a beat
	// after the mount detaches and the VMM's fds close, so a single luksClose can
	// still fail with "device still in use". Retry briefly rather than leak the
	// mapper (and its loop device) — a dangling mapper blocks the next open.
	var lastErr error
	for i := 0; i < 30; i++ {
		if !fileExists(mapperPath(machineID)) {
			return
		}
		if lastErr = d.cryptsetup(nil, "close", mapperName(machineID)); lastErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	slog.Error("luksClose failed after retries; volume mapper left open",
		"machine", machineID, "mapper", mapperName(machineID), "err", lastErr)
}

// destroyVolume closes the mapper/mount and deletes the container file.
func (d *Driver) destroyVolume(machineID string, layout jailLayout) {
	d.closeVolume(machineID, layout)
	_ = os.Remove(d.volumeFile(machineID))
}

// --- cryptsetup wrappers (key via stdin; never in argv) ----------------------

func (d *Driver) cryptsetupBin() string {
	if d.cfg.CryptsetupBin != "" {
		return d.cfg.CryptsetupBin
	}
	return "cryptsetup"
}

// cryptsetup runs the configured binary with key (if any) on stdin.
func (d *Driver) cryptsetup(key []byte, args ...string) error {
	cmd := exec.Command(d.cryptsetupBin(), args...)
	if key != nil {
		cmd.Stdin = strings.NewReader(string(key))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Redact: the key is on stdin, not argv, but be defensive in the message.
		return fmt.Errorf("cryptsetup %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) luksFormat(volFile string, key []byte) error {
	return d.cryptsetup(key, "luksFormat", "--type", "luks2", "-q", "--key-file=-", volFile)
}

func (d *Driver) luksOpen(volFile, mapper string, key []byte) error {
	return d.cryptsetup(key, "open", "--type", "luks2", "--key-file=-", volFile, mapper)
}

// --- small fs helpers --------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}
	return nil
}

// isMounted reports whether target is a mount point, by scanning /proc/mounts.
func isMounted(target string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[1] == target {
			return true
		}
	}
	return false
}
