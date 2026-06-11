//go:build !linux

package listen

import (
	"errors"
	"net"
)

// listenVsock is unavailable off Linux: AF_VSOCK is a Linux feature, and the
// guest agent only ever runs on vsock inside a (Linux) microVM. On macOS the
// dev/test paths use unix or tcp instead.
func listenVsock(port uint32) (net.Listener, error) {
	return nil, errors.New("listen: vsock is only supported on linux (use unix: or tcp: off-Linux)")
}
