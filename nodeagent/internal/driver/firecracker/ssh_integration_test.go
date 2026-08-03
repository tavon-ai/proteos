//go:build firecracker && linux

package firecracker_test

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// TestGuestSSHForward proves the inbound-SSH path end-to-end on a real
// Firecracker microVM booted from the baked rootfs: the driver dials the
// guest's SSH vsock port (1027), the guest agent's SSH forward bridges straight
// to the guest's own sshd (baked + enabled by image/build-rootfs.sh,
// loopback-only), and the first bytes back are an SSH protocol banner
// ("SSH-2.0-..."). No SSH handshake is completed here — just proof the raw byte
// path reaches a real sshd, matching the code-server web-forward proof above.
//
// Requires PROTEOS_TEST_ROOTFS to point at a rootfs with openssh-server baked
// (image/build-rootfs.sh, default on). A rootfs without it will (correctly)
// never serve here.
func TestGuestSSHForward(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real microVM + sshd; skipped in -short")
	}
	d, _, _ := testDriver(t)
	id := "dddddddd-0000-0000-0000-00000000000d"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	// sshd needs a moment after boot to generate host keys (ssh-keygen -A) and
	// start listening; retry the dial+banner-read with a generous deadline.
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := d.DialGuest(dctx, id, api.GuestSSHPort)
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		if !strings.HasPrefix(line, "SSH-2.0-") {
			t.Fatalf("banner = %q, want SSH-2.0- prefix", line)
		}
		t.Logf("sshd reachable over the SSH forward: banner %q", strings.TrimSpace(line))
		return
	}
	t.Fatalf("sshd never became reachable over the SSH forward (port %d): %v", api.GuestSSHPort, lastErr)
}
