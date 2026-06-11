//go:build firecracker && linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: stop is a plain shutdown that leaves the chroot in place, so the
// next start's jailer would mknod /dev/net/tun into a jail that already has it
// (EEXIST). prepareColdJail must wipe the jail first so a restart starts clean.
// Phase 4: the rootfs is no longer copied into the jail (it lives on the
// encrypted volume's /state), so only the kernel + run dir are laid down.
func TestPrepareColdJailCleansStaleJail(t *testing.T) {
	base := t.TempDir()
	l := jailLayout{chrootBaseDir: base, id: "11111111-2222-3333-4444-555555555555"}

	// Simulate a previous boot's leftover device node inside the jail.
	staleDev := filepath.Join(l.root(), "dev", "net", "tun")
	if err := os.MkdirAll(filepath.Dir(staleDev), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleDev, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	kernelSrc := filepath.Join(base, "vmlinux")
	if err := os.WriteFile(kernelSrc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	kInJail, err := prepareColdJail(l, kernelSrc)
	if err != nil {
		t.Fatalf("prepareColdJail: %v", err)
	}
	if kInJail != "/vmlinux" {
		t.Fatalf("in-jail kernel path = %q", kInJail)
	}
	if _, err := os.Stat(staleDev); !os.IsNotExist(err) {
		t.Errorf("stale /dev/net/tun survived prepareColdJail (err=%v); jailer mknod would EEXIST on restart", err)
	}
	if _, err := os.Stat(filepath.Join(l.root(), "vmlinux")); err != nil {
		t.Errorf("missing kernel in fresh jail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.root(), "run")); err != nil {
		t.Errorf("missing run dir in fresh jail: %v", err)
	}
	// The rootfs must NOT be in the jail anymore (it lives on the volume).
	if _, err := os.Stat(filepath.Join(l.root(), "rootfs.ext4")); !os.IsNotExist(err) {
		t.Errorf("rootfs.ext4 should not be copied into the jail (Phase 4): err=%v", err)
	}
}
