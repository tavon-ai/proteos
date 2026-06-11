//go:build firecracker && linux

package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/tavon/proteos/nodeagent/internal/driver"
)

// guestCID is the vsock context id used for every VM (Phase 3 decision #3). The
// host never uses AF_VSOCK, so a shared CID is fine — each VM has its own jailed
// uds, and that uds (reachable only by host root) is the trust boundary.
const guestCID = 3

// defaultGuestPort is the fixed guest port the in-VM agent listens on when the
// config does not override it.
const defaultGuestPort = 1024

var _ driver.GuestDialer = (*Driver)(nil)

// DialGuest opens a byte stream to the machine's in-guest agent over the jailed
// virtio-vsock uds, performing Firecracker's hybrid CONNECT/OK handshake to
// reach the guest port. The returned conn carries the raw guest stream; the
// node-agent's HTTP layer bridges it to the control-plane gateway. The HTTP
// layer checks the machine is running before calling this; here we only verify
// the driver tracks it.
func (d *Driver) DialGuest(ctx context.Context, machineID string) (net.Conn, error) {
	if _, ok, err := d.store.Load(machineID); err != nil {
		return nil, err
	} else if !ok {
		return nil, driver.ErrUnknownMachine
	}

	port := d.cfg.GuestVsockPort
	if port == 0 {
		port = defaultGuestPort
	}

	layout := jailLayout{chrootBaseDir: d.cfg.ChrootBaseDir, id: machineID}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", layout.vsockUDS())
	if err != nil {
		return nil, fmt.Errorf("dial vsock uds: %w", err)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// Hybrid handshake: "CONNECT <port>\n" → "OK <assigned_port>\n".
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake read: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected reply %q", strings.TrimSpace(line))
	}

	// Clear the handshake deadline; the caller manages timeouts thereafter.
	_ = conn.SetDeadline(time.Time{})
	// Any bytes buffered past the handshake line belong to the guest stream.
	return &handshakeConn{Conn: conn, r: br}, nil
}

// handshakeConn drains the post-handshake buffer before reading from the wire.
type handshakeConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *handshakeConn) Read(p []byte) (int, error) { return c.r.Read(p) }
