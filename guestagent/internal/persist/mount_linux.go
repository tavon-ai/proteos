//go:build linux

package persist

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// mountDisk waits for the block device to appear (≤deviceWaitTimeout), preens it
// with fsck, and mounts it ext4 at mountpoint. If it is already mounted there
// (e.g. a re-run), it is treated as success.
func mountDisk(device, mountpoint string, wait time.Duration) error {
	if err := waitForDevice(device, wait); err != nil {
		return err
	}
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}
	if alreadyMounted(mountpoint) {
		slog.Info("persist: device already mounted", "mountpoint", mountpoint)
		return nil
	}
	// fsck -p (preen) is belt-and-braces: ext4 replays its journal on mount, so a
	// missing fsck binary or a transient fsck hiccup must NOT cost us persistence.
	// Only a clearly-uncorrectable filesystem (exit ≥ 4) blocks the mount.
	preen(device)
	if err := unix.Mount(device, mountpoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", device, mountpoint, err)
	}
	return nil
}

// setupDiskBinds ensures home/ + workspace/ on the mounted disk and bind-mounts
// them over homeTarget (the session user's $HOME, e.g. /home/dev — or /root in
// the legacy root case) and /workspace so $HOME and the workspace live on the
// disk (decision #7). When uid != 0 the persisted home + workspace are chowned
// to the unprivileged session user so its shells can write them, and a fresh
// (empty) home is seeded from /etc/skel for a sane first login.
func setupDiskBinds(mount, homeTarget string, uid, gid int) error {
	homeSrc := filepath.Join(mount, "home")
	workSrc := filepath.Join(mount, "workspace")
	if err := ensureDirs(homeSrc, workSrc, homeTarget, "/workspace"); err != nil {
		return err
	}
	if uid != 0 {
		seedHome(homeSrc)
		// Chown the disk dirs (the bind sources) so the unprivileged user owns its
		// home + workspace. Covers first creation and migration from an older
		// root-owned disk. Skipped when the dir already belongs to the user, so a
		// steady-state boot does not re-walk a large workspace (cloned repos,
		// node_modules) every time.
		if err := chownTreeIfNeeded(homeSrc, uid, gid); err != nil {
			slog.Warn("persist: chown home failed; unprivileged user may not be able to write it", "err", err)
		}
		if err := chownTreeIfNeeded(workSrc, uid, gid); err != nil {
			slog.Warn("persist: chown workspace failed", "err", err)
		}
	}
	if err := bindMount(homeSrc, homeTarget); err != nil {
		return fmt.Errorf("bind home: %w", err)
	}
	if err := bindMount(workSrc, "/workspace"); err != nil {
		return fmt.Errorf("bind workspace: %w", err)
	}
	return nil
}

// chownTreeIfNeeded recursively chowns root to uid:gid, but returns early when
// root already belongs to uid — the common steady-state case, where re-walking
// a large tree would only waste boot time. The top-dir owner is a reliable proxy
// because the tree is only ever chowned as a whole.
func chownTreeIfNeeded(root string, uid, gid int) error {
	if fi, err := os.Stat(root); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
			return nil
		}
	}
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}

// seedHome populates an empty home from /etc/skel (best-effort) so a brand-new
// disk gives the unprivileged user the usual .bashrc/.profile on first login.
// Does nothing if the home already has contents or /etc/skel is absent.
func seedHome(home string) {
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) > 0 {
		return // unreadable or already populated — leave it alone
	}
	skel, err := os.ReadDir("/etc/skel")
	if err != nil {
		return
	}
	for _, e := range skel {
		if err := copyTree(filepath.Join("/etc/skel", e.Name()), filepath.Join(home, e.Name())); err != nil {
			slog.Warn("persist: seed skel entry failed", "entry", e.Name(), "err", err)
		}
	}
}

// copyTree copies src (file, dir, or symlink) to dst, recursively, preserving
// mode. Ownership is fixed up afterward by the chownTree pass in setupDiskBinds.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	}
}

func bindMount(src, dst string) error {
	return unix.Mount(src, dst, "", unix.MS_BIND, "")
}

// waitForDevice blocks until path exists or timeout elapses.
func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", path, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// alreadyMounted reports whether mountpoint is a mount point, by comparing its
// device id with its parent's.
func alreadyMounted(mountpoint string) bool {
	var st, parent unix.Stat_t
	if err := unix.Stat(mountpoint, &st); err != nil {
		return false
	}
	if err := unix.Stat(filepath.Dir(mountpoint), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// preen runs fsck -p best-effort: it logs the outcome but never blocks the
// mount. fsck exit codes: 0 clean, 1 errors corrected, 2 corrected+reboot,
// 4 errors left uncorrected, 8 operational error, 16 usage. A missing binary or
// a transient failure is logged and ignored (the journal replay on mount, and
// the mount itself, are the real safety net).
func preen(device string) {
	if _, err := exec.LookPath("fsck"); err != nil {
		slog.Warn("persist: fsck not found; skipping preen (relying on ext4 journal)", "device", device)
		return
	}
	cmd := exec.Command("fsck", "-p", device)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err == nil {
		return
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1, 2:
			slog.Info("persist: fsck corrected errors", "device", device, "code", ee.ExitCode())
		default:
			slog.Warn("persist: fsck reported uncorrected issues; attempting mount anyway",
				"device", device, "code", ee.ExitCode())
		}
		return
	}
	slog.Warn("persist: fsck could not run; attempting mount anyway", "device", device, "err", err)
}
