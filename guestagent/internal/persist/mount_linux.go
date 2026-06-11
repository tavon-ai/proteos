//go:build linux

package persist

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	// fsck -p (preen): auto-repair safe issues. Exit codes 0 (clean) and 1
	// (errors corrected) are fine; ≥2 means we should not mount.
	if err := preen(device); err != nil {
		return err
	}
	if err := unix.Mount(device, mountpoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", device, mountpoint, err)
	}
	return nil
}

// setupDiskBinds ensures home/ + workspace/ on the mounted disk and bind-mounts
// them over /root and /workspace so the root shell's $HOME and the workspace
// live on the disk (decision #7).
func setupDiskBinds(mount string) error {
	homeSrc := filepath.Join(mount, "home")
	workSrc := filepath.Join(mount, "workspace")
	if err := ensureDirs(homeSrc, workSrc, "/root", "/workspace"); err != nil {
		return err
	}
	if err := bindMount(homeSrc, "/root"); err != nil {
		return fmt.Errorf("bind home: %w", err)
	}
	if err := bindMount(workSrc, "/workspace"); err != nil {
		return fmt.Errorf("bind workspace: %w", err)
	}
	return nil
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

// preen runs fsck -p, tolerating exit code 1 (errors were corrected).
func preen(device string) error {
	cmd := exec.Command("fsck", "-p", device)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		slog.Info("persist: fsck corrected errors", "device", device)
		return nil
	}
	return fmt.Errorf("fsck %s: %w", device, err)
}
