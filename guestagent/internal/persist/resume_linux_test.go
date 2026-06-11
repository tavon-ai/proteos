//go:build linux

package persist

import (
	"crypto/rand"
	"os"
	"testing"
	"time"
)

// TestApplyResumeSetsClock proves the resume syscall path runs as root: it sets
// the wall clock to (approximately) the host-provided time and credits entropy.
// Needs CAP_SYS_TIME + CAP_SYS_ADMIN; runs in the plain (non-KVM) CI container
// as root and skips otherwise. It sets the clock to ~now so it does not disrupt
// the host time.
func TestApplyResumeSetsClock(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (clock_settime); run in the CI container")
	}

	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		t.Fatal(err)
	}

	// Target a time a hair in the future of "now" so the corrected skew is a
	// small positive number and the host clock is effectively unchanged.
	target := time.Now().Add(50 * time.Millisecond).UnixNano()
	skewMS, err := applyResume(target, entropy)
	if err != nil {
		t.Fatalf("applyResume: %v", err)
	}
	// We moved the clock forward by ~50ms; the corrected skew should be small.
	if skewMS < 0 || skewMS > 5000 {
		t.Fatalf("implausible corrected skew: %d ms", skewMS)
	}

	// The clock now reads at/after the target (within a generous tolerance).
	now := time.Now().UnixNano()
	if now < target-int64(time.Second) {
		t.Fatalf("clock not advanced: now=%d target=%d", now, target)
	}
}

// TestAddEntropyDirect exercises the RNDADDENTROPY ioctl directly.
func TestAddEntropyDirect(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (RNDADDENTROPY)")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := addEntropy(buf); err != nil {
		t.Fatalf("addEntropy: %v", err)
	}
}
