// Package listen turns a listener spec string into a net.Listener. Three
// schemes are supported:
//
//	vsock:<port>        AF_VSOCK on the given guest port (Linux only; the
//	                    production transport — the node-agent connects through
//	                    Firecracker's virtio-vsock device).
//	unix:<path>         a unix-domain socket (the dev driver's transport, and
//	                    handy for tests).
//	tcp:<host:port>     a TCP listener (tests; never used in production).
//
// vsock lives behind a Linux build tag (vsock_linux.go / vsock_other.go) so the
// agent still builds and tests on macOS, where unix/tcp cover everything.
package listen

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Listen opens a listener for spec. For unix sockets it removes a stale socket
// file first and tightens the permissions to 0600 (only the owner — root in the
// guest, the node-agent's uid in dev — may connect).
func Listen(spec string) (net.Listener, error) {
	scheme, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("listen: malformed spec %q (want scheme:value)", spec)
	}
	switch scheme {
	case "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("listen: bad vsock port %q: %w", rest, err)
		}
		return listenVsock(uint32(port))
	case "unix":
		if err := os.Remove(rest); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("listen: remove stale socket %q: %w", rest, err)
		}
		ln, err := net.Listen("unix", rest)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(rest, 0o600); err != nil {
			ln.Close()
			return nil, fmt.Errorf("listen: chmod socket: %w", err)
		}
		return ln, nil
	case "tcp":
		return net.Listen("tcp", rest)
	default:
		return nil, fmt.Errorf("listen: unknown scheme %q", scheme)
	}
}
