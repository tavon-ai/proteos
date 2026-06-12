//go:build linux

package persist

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// applyResume corrects the guest wall clock to the host-provided time and
// credits fresh entropy to the kernel CRNG after a snapshot restore (decision
// #9). It returns the skew it corrected (host − guest) in milliseconds.
//
// Clock resync is load-bearing: nothing else resets the wall clock after a
// restore, so it skews by the hibernated duration (spike 05). Entropy injection
// is belt-and-braces on a VMGenID-aware kernel and load-bearing otherwise.
func applyResume(unixNanos int64, entropy []byte) (int64, error) {
	guestBefore := time.Now().UnixNano()
	skewMS := (unixNanos - guestBefore) / int64(time.Millisecond)

	ts := unix.NsecToTimespec(unixNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		// EPERM means the process lacks CAP_SYS_TIME — the normal case in
		// unprivileged CI/dev runs. The guest agent runs as root in the microVM,
		// so this never fires in production; treat it as degraded (warn and
		// continue) rather than failing the whole resume.
		if errors.Is(err, unix.EPERM) {
			slog.Warn("persist: clock_settime not permitted (no CAP_SYS_TIME); skipping clock resync", "skew_ms", skewMS)
			return 0, nil
		}
		return 0, fmt.Errorf("clock_settime: %w", err)
	}

	if len(entropy) > 0 {
		if err := addEntropy(entropy); err != nil {
			// Non-fatal: the clock is the load-bearing half. Log and continue.
			slog.Warn("persist: RNDADDENTROPY failed", "err", err)
		}
	}
	return skewMS, nil
}

// addEntropy feeds buf into the kernel CRNG via RNDADDENTROPY on /dev/random,
// crediting all of it (len*8 bits). This both adds randomness and bumps the
// entropy estimate so /dev/random unblocks.
func addEntropy(buf []byte) error {
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(buf))
	binary.NativeEndian.PutUint32(info[0:4], uint32(len(buf)*8)) // entropy_count (bits)
	binary.NativeEndian.PutUint32(info[4:8], uint32(len(buf)))   // buf_size (bytes)
	copy(info[8:], buf)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&info[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
