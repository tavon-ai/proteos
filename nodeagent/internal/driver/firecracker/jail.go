//go:build firecracker && linux

package firecracker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// The jailer chroots the VMM into <chroot-base>/firecracker/<id>/root and drops
// to a dedicated uid/gid, exactly as in the spike's 06-jailer.sh. Production
// runs *every* VMM this way (the plan's "jailer from the first boot"). The
// kernel and a FRESH writable rootfs copy must be placed inside the jail before
// launch, because the chrooted process can only see paths under its root.

// jailLayout resolves the on-host paths for one machine's jail.
type jailLayout struct {
	chrootBaseDir string // jailer --chroot-base-dir
	id            string // jail id (machine id)
}

func (l jailLayout) root() string {
	return filepath.Join(l.chrootBaseDir, "firecracker", l.id, "root")
}

func (l jailLayout) socket() string {
	// Jailer creates the API socket at <root>/run/firecracker.socket because we
	// pass --api-sock /run/firecracker.socket to the chrooted firecracker.
	return filepath.Join(l.root(), "run", "firecracker.socket")
}

// vsockUDSName is the vsock unix socket's path *relative to the jail root* (what
// we pass to PUT /vsock). Firecracker, running as the jail uid, creates it.
const vsockUDSName = "v.sock"

// vsockUDS is the host-side path of the vsock socket inside the chroot — what
// DialGuest connects to.
func (l jailLayout) vsockUDS() string {
	return filepath.Join(l.root(), vsockUDSName)
}

// prepareChroot creates the jail root, copies the pinned kernel and a fresh
// writable copy of the base rootfs into it, and chowns everything to the
// per-VM uid/gid. Returns the in-jail paths the API calls must reference.
func prepareChroot(l jailLayout, kernelSrc, rootfsSrc string, uid, gid int) (kernelInJail, rootfsInJail string, err error) {
	// Start from a clean jail. stop is a plain shutdown that leaves the chroot in
	// place, so on a restart the jailer's mknod of /dev/net/tun (and /dev/kvm)
	// would fail with EEXIST. Wiping first also realises the plan's "fresh
	// writable rootfs copy per boot". On a first boot this is a no-op.
	if err := removeJail(l); err != nil {
		return "", "", fmt.Errorf("clean jail: %w", err)
	}

	root := l.root()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir jail: %w", err)
	}

	if err := copyFile(kernelSrc, filepath.Join(root, "vmlinux"), 0o644); err != nil {
		return "", "", fmt.Errorf("copy kernel: %w", err)
	}
	// A fresh per-boot copy of the rootfs (writable, discarded on destroy) —
	// the plan's "rootfs = fresh writable copy of the pinned base image".
	if err := copyFile(rootfsSrc, filepath.Join(root, "rootfs.ext4"), 0o644); err != nil {
		return "", "", fmt.Errorf("copy rootfs: %w", err)
	}

	// chown the whole jail subtree to the unprivileged VMM user.
	if err := chownRecursive(filepath.Join(l.chrootBaseDir, "firecracker", l.id), uid, gid); err != nil {
		return "", "", fmt.Errorf("chown jail: %w", err)
	}

	// Paths as the chrooted VMM sees them (relative to its root).
	return "/vmlinux", "/rootfs.ext4", nil
}

// launchJailer execs the jailer to start a daemonized, chrooted, uid-dropped
// firecracker, mirroring 06-jailer.sh. Returns the jailer/firecracker pid.
func launchJailer(jailerBin, firecrackerBin string, l jailLayout, uid, gid int) (int, error) {
	cmd := exec.Command(jailerBin,
		"--id", l.id,
		"--exec-file", firecrackerBin,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", l.chrootBaseDir,
		"--cgroup-version", "2",
		"--cgroup", "cpu.weight=512",
		"--daemonize",
		"--", "--api-sock", "/run/firecracker.socket",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("jailer: %w: %s", err, out)
	}
	// With --daemonize the jailer forks firecracker and exits; find the pid by
	// the dedicated uid (one VMM per uid in our allocation scheme).
	return firecrackerPidForUID(uid)
}

// firecrackerPidForUID returns the pid of the firecracker process owned by uid.
func firecrackerPidForUID(uid int) (int, error) {
	out, err := runOut("pgrep", "-u", strconv.Itoa(uid), "-f", "firecracker")
	if err != nil {
		return 0, fmt.Errorf("locate jailed firecracker (uid %d): %w", uid, err)
	}
	// pgrep may return multiple lines; take the first.
	pid, err := strconv.Atoi(firstLine(out))
	if err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", out, err)
	}
	return pid, nil
}

// removeJail deletes a machine's entire jail subtree.
func removeJail(l jailLayout) error {
	return os.RemoveAll(filepath.Join(l.chrootBaseDir, "firecracker", l.id))
}

// --- small fs helpers --------------------------------------------------------

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func chownRecursive(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
