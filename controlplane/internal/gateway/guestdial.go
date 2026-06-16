package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// guestHandshake carries the per-session parameters the gateway forwards to the
// guest /terminal endpoint: the opaque session name, the validated working
// directory (cwd), and — for agent sessions — the provider key. All are
// forwarded verbatim from the browser request (after the control plane has
// validated them); the guest re-validates cwd before use (Phase 9 decision #3).
type guestHandshake struct {
	session  string
	cwd      string
	provider string
}

// dialGuestWS speaks the WebSocket client handshake to the guest agent's
// /terminal endpoint over an already-established raw tunnel conn (the
// node-agent guest tunnel). It hands coder/websocket an http.Client whose
// transport returns the tunnel for its single connection, with keep-alives off
// so the client never tries to reuse or re-dial it.
//
// The handshake parameters are forwarded opaquely from the browser request; the
// guest validates them. A handshake failure (e.g. the guest rejected the
// session or cwd) surfaces as an error the caller maps to an internal close.
func dialGuestWS(ctx context.Context, tunnel net.Conn, hs guestHandshake) (*websocket.Conn, error) {
	used := false
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("gateway: guest tunnel already consumed")
			}
			used = true
			return tunnel, nil
		},
	}
	httpClient := &http.Client{Transport: transport}

	q := url.Values{}
	if hs.session != "" {
		q.Set(guestwire.QueryParamSession, hs.session)
	}
	if hs.cwd != "" {
		q.Set(guestwire.QueryParamCwd, hs.cwd)
	}
	if hs.provider != "" {
		q.Set(guestwire.QueryParamProvider, hs.provider)
	}
	u := "ws://guest/terminal"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	// PTY output bursts can exceed the default 32 KiB read limit (esp. the
	// scrollback replay frame); lift it well above any single message.
	c.SetReadLimit(8 << 20)
	return c, nil
}
