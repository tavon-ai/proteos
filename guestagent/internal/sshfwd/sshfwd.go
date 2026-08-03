// Package sshfwd is the guest agent's inbound-SSH forward. It listens on a
// private transport (vsock:1027 in production, a unix socket in dev) that the
// node-agent tunnel reaches on agentapi.GuestSSHPort, and bridges each accepted
// connection to the guest's own sshd on a fixed loopback address
// (127.0.0.1:22).
//
// It is previewfwd's shape with the preamble step deleted and the target port
// hardcoded: sshd is a system service, not a user-chosen preview backend, so it
// gets its own fixed vsock port instead of riding the generic preview
// forwarder (keeping port 22 out of the user-choosable preview range — see
// agentapi.IsSystemGuestPort). Like the terminal/web/preview listeners it does
// no auth of its own: sshd binds loopback-only, so the only way to reach it at
// all is through this forward, and the only way to reach THIS forward is over
// the per-VM private transport — the control-plane gateway is the sole
// authenticator (same trust argument as the code-server and preview forwards).
package sshfwd

import (
	"context"
	"io"
	"log/slog"
	"net"
)

// DefaultBackend is the loopback address the guest's sshd binds (matching the
// baked sshd_config's ListenAddress 127.0.0.1).
const DefaultBackend = "127.0.0.1:22"

// Forwarder accepts connections on a listener and bridges each to backend.
type Forwarder struct {
	ln      net.Listener
	backend string
	dial    func(ctx context.Context, addr string) (net.Conn, error)
}

// New builds a Forwarder over ln, dialing backend (DefaultBackend in
// production; tests may point it elsewhere) for each connection.
func New(ln net.Listener, backend string) *Forwarder {
	if backend == "" {
		backend = DefaultBackend
	}
	return &Forwarder{ln: ln, backend: backend, dial: defaultDial}
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

// handle dials sshd and bridges client<->backend. An unreachable backend (sshd
// not yet up, or not baked into this image) simply drops the client connection
// — the gateway maps the closed tunnel to a 502, exactly as for an unreachable
// guest.
func (f *Forwarder) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	backend, err := f.dial(ctx, f.backend)
	if err != nil {
		slog.Warn("sshfwd: dial sshd", "addr", f.backend, "err", err)
		return
	}
	defer backend.Close()

	bridge(client, backend)
}

// bridge copies bytes both ways until either side closes, then tears the other
// down. Same dumb-pipe shape as previewfwd/webfwd's bridge.
func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// defaultDial dials a loopback TCP backend.
func defaultDial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
