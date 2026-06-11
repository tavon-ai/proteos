package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

// dialGuestWS speaks the WebSocket client handshake to the guest agent's
// /terminal endpoint over an already-established raw tunnel conn (the
// node-agent guest tunnel). It hands coder/websocket an http.Client whose
// transport returns the tunnel for its single connection, with keep-alives off
// so the client never tries to reuse or re-dial it.
//
// The session name is forwarded opaquely from the browser request; the guest
// validates it. A handshake failure (e.g. the guest rejected the session)
// surfaces as an error the caller maps to an internal close.
func dialGuestWS(ctx context.Context, tunnel net.Conn, session string) (*websocket.Conn, error) {
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

	u := "ws://guest/terminal"
	if session != "" {
		u += "?session=" + url.QueryEscape(session)
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
