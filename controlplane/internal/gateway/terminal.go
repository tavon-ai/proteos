// Package gateway is the control-plane terminal proxy. It terminates the
// browser's terminal WebSocket at GET /gw/terminal, authenticates and
// authorizes it (the surrounding requireAuth middleware + an exact Origin
// check + ownership resolution done by the caller), dials the machine's guest
// agent through the node-agent byte tunnel, and relays the WebSocket messages
// 1:1 between the two legs.
//
// The auth / target-resolution / dial steps are kept deliberately separable so
// Phase 8 can swap in per-subdomain token auth and code-server targets without
// touching the proxy core (decision #8).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
)

// pingInterval keeps the browser leg alive (and any idle timers between us and
// the client). The guest leg is kept alive by the guest agent's own pings,
// which coder/websocket auto-pongs.
const pingInterval = 30 * time.Second

// dialTimeout bounds the guest tunnel + WebSocket handshake.
const dialTimeout = 10 * time.Second

// GuestDialer opens the opaque byte tunnel to a machine's guest agent. The
// nodeclient.Client satisfies this; keeping it an interface makes the proxy
// testable against a fake agent.
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string) (net.Conn, error)
}

// Proxy is the terminal gateway. Construct it with NewProxy and register Serve
// behind requireAuth.
type Proxy struct {
	allowedOrigins []string
	guests         GuestDialer
	registry       *Registry
}

// NewProxy builds a Proxy. allowedOrigins is the exact-match Origin allowlist;
// guests dials the node-agent tunnel; registry tracks conns for revocation.
func NewProxy(allowedOrigins []string, guests GuestDialer, registry *Registry) *Proxy {
	return &Proxy{allowedOrigins: allowedOrigins, guests: guests, registry: registry}
}

// ServeOpts carries the per-request inputs the caller has already resolved:
// the target machine (confirmed running), the auth session id (for revocation),
// the session name to forward to the guest, and a Refresh hook that re-reads
// the machine's running state for mid-session close-code mapping.
type ServeOpts struct {
	MachineID string
	SessionID string
	Session   string
	Refresh   func(ctx context.Context) (running bool, err error)
}

// Serve handles one /gw/terminal request: Origin check (pre-upgrade 403), then
// upgrade, tunnel + guest dial, and message relay until either side ends.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, opts ServeOpts) {
	// Defense in depth: the route handler already rejects a bad Origin (403)
	// before resolving the machine, but re-check here so Serve is never an open
	// upgrade if a future caller forgets.
	if !p.AllowsOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "origin_forbidden")
		return
	}

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

	// 1. Dial the node-agent guest tunnel.
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	tunnel, err := p.guests.DialGuest(dialCtx, opts.MachineID)
	if err != nil {
		dialCancel()
		code, reason := closeForDialError(ctx, err, opts.Refresh)
		_ = c.Close(code, reason)
		return
	}
	defer tunnel.Close()

	// 2. Speak the WebSocket handshake to the guest across the tunnel.
	guestWS, err := dialGuestWS(dialCtx, tunnel, opts.Session)
	dialCancel()
	if err != nil {
		slog.Warn("gateway: guest ws handshake failed", "machine", opts.MachineID, "err", err)
		_ = c.Close(guestwire.CloseInternal, "internal")
		return
	}
	defer guestWS.CloseNow()

	// 3. Keep the browser leg alive while relaying.
	go pingLoop(ctx, c)

	res := relay(ctx, c, guestWS)
	cancel() // unblock the other relay direction

	running := false
	if res.side == sideGuest && websocket.CloseStatus(res.err) != websocket.StatusNormalClosure && opts.Refresh != nil {
		// Only the abnormal-guest-close path needs to know the machine state.
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		running, _ = opts.Refresh(rctx)
		rcancel()
	}
	code, reason := browserCloseFor(res, running)
	_ = c.Close(code, reason)
}

// closeForDialError maps a guest-tunnel dial failure to a browser close code:
// a not-running agent reply (or a refresh that shows the machine stopped) is
// machine_stopped; anything else is an internal fault.
func closeForDialError(ctx context.Context, err error, refresh func(context.Context) (bool, error)) (websocket.StatusCode, string) {
	if errors.Is(err, nodeclient.ErrNotRunning) {
		return guestwire.CloseMachineStopped, "machine_stopped"
	}
	if refresh != nil {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if running, rerr := refresh(rctx); rerr == nil && !running {
			return guestwire.CloseMachineStopped, "machine_stopped"
		}
	}
	return guestwire.CloseInternal, "internal"
}

// pingLoop sends WebSocket pings to c every pingInterval until ctx ends.
func pingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Ping(ctx); err != nil {
				return
			}
		}
	}
}

// writeJSONError writes the {"error": code} envelope used across the API, so
// the SPA's terminal socket can branch on the upgrade failure status/body.
func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
