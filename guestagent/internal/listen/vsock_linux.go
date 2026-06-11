//go:build linux

package listen

import (
	"net"

	"github.com/mdlayher/vsock"
)

// listenVsock listens on AF_VSOCK at the given port for any context id. Inside
// the microVM the guest CID is fixed at 3 (see Phase 3 decision #3); the
// node-agent reaches this listener via Firecracker's hybrid-handshake uds, so
// the guest only ever needs to accept — it never dials.
func listenVsock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}
