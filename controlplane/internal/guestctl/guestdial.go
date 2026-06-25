package guestctl

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// dialControlWS speaks the WebSocket client handshake to the guest agent's
// /control endpoint over an already-established raw tunnel conn (the node-agent
// guest tunnel). It hands coder/websocket an http.Client whose transport returns
// the tunnel for its single connection, with keep-alives off so the client never
// tries to reuse or re-dial it — the same trick the terminal gateway and the
// secret injector use.
func dialControlWS(ctx context.Context, tunnel net.Conn) (*websocket.Conn, error) {
	used := false
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("guestctl: guest tunnel already consumed")
			}
			used = true
			return tunnel, nil
		},
	}
	httpClient := &http.Client{Transport: transport}

	c, _, err := websocket.Dial(ctx, "ws://guest"+guestwire.RouteControlPath, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}
