//go:build !linux

package persist

import (
	"errors"
	"time"
)

// On non-Linux (dev/Mac) the disk-mount path is unsupported: only dir mode
// works. These stubs let the package build and the dir-mode + SQLite paths run
// under `go test` on a Mac.

func mountDisk(device, mountpoint string, wait time.Duration) error {
	return errors.New("disk mode is Linux-only; set PROTEOS_GUEST_PERSIST for dir mode")
}

func setupDiskBinds(mount string) error {
	return errors.New("bind mounts are Linux-only")
}

// applyResume is a no-op on non-Linux: there is no clock_settime/RNDADDENTROPY
// to drive. It reports zero corrected skew so the dir-mode server tests can
// exercise the /resume route end-to-end without root.
func applyResume(unixNanos int64, entropy []byte) (int64, error) {
	return 0, nil
}
