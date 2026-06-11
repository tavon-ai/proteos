//go:build firecracker && linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: stop is a plain shutdown that leaves the chroot in place, so the
// next start's jailer would mknod /dev/net/tun into a jail that already has it
// (EEXIST). prepareChroot must wipe the jail first so a restart starts clean.
// (uid/gid = current euid so chownRecursive works without root.)
func TestPrepareChrootCleansStaleJail(t *testing.T) {
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

	// Fake pinned kernel + rootfs sources to copy in.
	kernelSrc := filepath.Join(base, "vmlinux")
	rootfsSrc := filepath.Join(base, "rootfs.ext4")
	for _, f := range []string{kernelSrc, rootfsSrc} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	kInJail, rInJail, err := prepareChroot(l, kernelSrc, rootfsSrc, os.Geteuid(), os.Getegid())
	if err != nil {
		t.Fatalf("prepareChroot: %v", err)
	}
	if kInJail != "/vmlinux" || rInJail != "/rootfs.ext4" {
		t.Fatalf("in-jail paths = %q, %q", kInJail, rInJail)
	}
	if _, err := os.Stat(staleDev); !os.IsNotExist(err) {
		t.Errorf("stale /dev/net/tun survived prepareChroot (err=%v); jailer mknod would EEXIST on restart", err)
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4"} {
		if _, err := os.Stat(filepath.Join(l.root(), f)); err != nil {
			t.Errorf("missing %s in fresh jail: %v", f, err)
		}
	}
}
