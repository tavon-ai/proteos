package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// SSHProxy is the inbound-SSH gateway (plan: docs/plans/ssh-access.md §3.4
// Option A). It terminates a raw-byte WebSocket at GET /gw/ssh and bridges it
// 1:1 to the machine's guest agent through the node-agent tunnel at
// agentapi.GuestSSHPort. Unlike the terminal proxy there is no JSON handshake
// or message-type framing to preserve: SSH is already a self-framing binary
// protocol, and the guest side is a plain byte forwarder
// (guestagent/internal/sshfwd) that dials sshd directly — not a nested
// WebSocket server the way the terminal's guest endpoint is. So Serve dials the
// tunnel and raw-bridges bytes with no intermediate guest-side WS handshake.
// Construct it with NewSSHProxy and register Serve behind requireAuth.
type SSHProxy struct {
	allowedOrigins []string
	guests         GuestDialer
	registry       *Registry
}

// NewSSHProxy builds an SSHProxy. allowedOrigins is the exact-match Origin
// allowlist (enforced only for browser-originated requests — see AllowsOrigin);
// guests dials the node-agent tunnel; registry tracks conns for revocation
// (shared with the terminal proxy's Registry, so a session revoke closes both).
func NewSSHProxy(allowedOrigins []string, guests GuestDialer, registry *Registry) *SSHProxy {
	return &SSHProxy{allowedOrigins: allowedOrigins, guests: guests, registry: registry}
}

// AllowsOrigin applies the same exact-match allowlist the terminal proxy uses.
// Unlike the terminal (browser-only), this route also serves the CLI's
// bearer-token ProxyCommand path, which never sends an Origin header at all —
// the route handler only calls this when a request DOES carry one, mirroring
// csrfHeader's bearer-token exemption (no ambient browser credential, so the
// forgery this check defends against does not apply).
func (p *SSHProxy) AllowsOrigin(r *http.Request) bool {
	return originAllowed(p.allowedOrigins, r)
}

// SSHServeOpts carries the per-request inputs the caller has already resolved.
type SSHServeOpts struct {
	MachineID string
	SessionID string

	Refresh func(ctx context.Context) (running bool, err error)
}

// Serve handles one /gw/ssh request: upgrade, tunnel dial, and raw byte relay
// until either side ends. The caller (the route handler) owns auth and the
// conditional Origin check — Serve trusts it has already happened, same
// division of responsibility as the terminal proxy's Serve/handler split.
func (p *SSHProxy) Serve(w http.ResponseWriter, r *http.Request, opts SSHServeOpts) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		// Accept already wrote a response on failure.
		return
	}

	// Register for revocation: a logout/revoke of this session closes the conn
	// with 4001 out from under the relay.
	unregister := p.registry.Register(opts.SessionID, func() {
		_ = c.Close(guestwire.CloseSessionRevoked, "session_revoked")
	})
	defer unregister()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	tunnel, err := p.guests.DialGuest(dialCtx, opts.MachineID, agentapi.GuestSSHPort)
	dialCancel()
	if err != nil {
		code, reason := closeForDialError(ctx, err, opts.Refresh)
		_ = c.Close(code, reason)
		return
	}
	defer tunnel.Close()

	res := relaySSH(ctx, c, tunnel)

	running := false
	if res.side == sideGuestSSH && opts.Refresh != nil {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		running, _ = opts.Refresh(rctx)
		rcancel()
	}
	code, reason := sshCloseFor(res, running)
	_ = c.Close(code, reason)
	cancel()
}

// sshSide identifies which leg of the relay ended first.
type sshSide int

const (
	sideClientSSH sshSide = iota
	sideGuestSSH
)

type sshRelayResult struct {
	side sshSide
	err  error
}

// relaySSH pumps bytes in both directions between the client WebSocket and the
// raw guest tunnel until either side ends, returning which side ended first.
func relaySSH(ctx context.Context, client *websocket.Conn, guest net.Conn) sshRelayResult {
	done := make(chan sshRelayResult, 2)
	go func() { done <- sshRelayResult{sideClientSSH, copyWSToConn(ctx, client, guest)} }()
	go func() { done <- sshRelayResult{sideGuestSSH, copyConnToWS(ctx, guest, client)} }()
	return <-done
}

// copyWSToConn forwards binary WebSocket messages from src to dst until a read
// or write error occurs. Non-binary frames (there should be none — the client
// speaks raw SSH bytes only) are ignored rather than treated as fatal, so an
// errant ping/pong doesn't tear down the tunnel.
func copyWSToConn(ctx context.Context, src *websocket.Conn, dst net.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if _, err := dst.Write(data); err != nil {
			return err
		}
	}
}

// copyConnToWS forwards raw bytes from src as binary WebSocket messages to dst
// until a read or write error occurs.
func copyConnToWS(ctx context.Context, src net.Conn, dst *websocket.Conn) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if werr := dst.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// sshCloseFor maps a relay outcome onto the close code sent to the client.
// running reports whether the machine is still running, used to distinguish a
// normal SSH session end (the user logged out, sshd closed the connection)
// from the machine stopping out from under the session — there is no exit-frame
// protocol here (unlike the terminal) to tell the two apart directly, since the
// guest leg is a raw byte stream.
func sshCloseFor(res sshRelayResult, running bool) (websocket.StatusCode, string) {
	if res.side == sideClientSSH {
		// The client closed first (ssh exited / network dropped). Nothing to
		// tell it; close normally so we tear down the guest leg cleanly.
		return websocket.StatusNormalClosure, ""
	}
	// The guest leg ended. If the machine is no longer running it stopped under
	// us; otherwise this is an ordinary sshd-closed-the-connection end (e.g. the
	// user typed `exit`), not a fault.
	if !running {
		return guestwire.CloseMachineStopped, "machine_stopped"
	}
	return websocket.StatusNormalClosure, ""
}
