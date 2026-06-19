// Package previewfwd is the guest agent's port-preview forward (PP1). It listens
// on a private transport (vsock:1026 in production, a unix socket in dev) that
// the node-agent tunnel reaches on agentapi.GuestPreviewPort, and bridges each
// accepted connection to an arbitrary in-VM loopback port.
//
// Unlike the code-server web forward, there is no fixed backend and no
// supervisor: the user's own process is the backend. The node-agent names the
// target port in a one-line preamble ("<port>\n") at the start of each
// connection; this forward reads that line, dials 127.0.0.1:<port>, and
// raw-copies bytes thereafter. A missing backend simply drops the connection —
// the gateway maps the closed tunnel to a 502, exactly as for an unreachable
// guest. It does no auth of its own: like the terminal and web listeners it is
// reachable only over the per-VM private transport, so the control-plane gateway
// is the sole authenticator (Phase 3 decision #10).
package previewfwd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// preambleTimeout bounds how long we wait for the node-agent to name the target
// port. A connection that opens but never sends the preamble is dropped rather
// than parking an accept goroutine forever.
const preambleTimeout = 10 * time.Second

// Forwarder accepts connections on a listener, reads the target-port preamble
// from each, and bridges it to the named loopback port.
type Forwarder struct {
	ln   net.Listener
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// New builds a Forwarder over ln.
func New(ln net.Listener) *Forwarder {
	return &Forwarder{ln: ln, dial: defaultDial}
}

// Serve runs the accept loop until ctx is cancelled or the listener errors. It
// closes the listener when ctx ends so a blocked Accept unblocks.
func (f *Forwarder) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = f.ln.Close()
	}()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go f.handle(ctx, conn)
	}
}

// handle reads the target-port preamble, dials the loopback backend, then
// bridges client↔backend. A bad preamble or an unreachable backend simply drops
// the client connection (→ 502 at the gateway).
func (f *Forwarder) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	// Read the "<port>\n" preamble under a deadline; the buffered reader may hold
	// application bytes that arrived in the same segment — they belong to the
	// client→backend stream and must not be lost.
	_ = client.SetReadDeadline(time.Now().Add(preambleTimeout))
	br := bufio.NewReader(client)
	line, err := br.ReadString('\n')
	if err != nil {
		slog.Warn("previewfwd: read preamble", "err", err)
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	port, ok := parsePort(line)
	if !ok {
		slog.Warn("previewfwd: bad preamble", "line", strings.TrimSpace(line))
		return
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10))
	backend, err := f.dial(ctx, addr)
	if err != nil {
		slog.Warn("previewfwd: dial backend", "addr", addr, "err", err)
		return
	}
	defer backend.Close()

	bridge(br, client, backend)
}

// parsePort extracts the decimal port from a preamble line. It must be a valid
// TCP port (1–65535); a 0 or out-of-range value is rejected.
func parsePort(line string) (uint32, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil || n == 0 || n > 65535 {
		return 0, false
	}
	return uint32(n), true
}

// bridge copies bytes both ways until either side closes, then tears the other
// down. The client→backend direction reads through clientRead (the buffered
// reader that consumed the preamble) so any bytes buffered past it are
// preserved; the backend→client direction writes to the raw client conn.
func bridge(clientRead io.Reader, client, backend net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, clientRead); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, backend); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = backend.Close()
	<-done
}

// defaultDial dials a loopback TCP backend.
func defaultDial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// ErrDisabled is returned by callers when no preview listener is configured.
var ErrDisabled = errors.New("previewfwd: disabled (no listen spec)")
