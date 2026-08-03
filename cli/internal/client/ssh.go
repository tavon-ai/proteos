package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// DialSSH opens the control plane's inbound-SSH gateway (GET /gw/ssh) for
// machineID and returns it as a plain byte stream: every Write becomes one
// binary WebSocket message, every Read drains the next one (via
// websocket.NetConn — see its doc for the exact framing contract). The caller
// bridges this to a local SSH client's transport (stdio, when used as an
// `ssh -o ProxyCommand` helper) or to os.Stdin/Stdout directly.
//
// Authentication rides the same bearer token every other Client call uses;
// unlike a browser session this carries no Origin header at all, which the
// gateway's /gw/ssh handler treats as the CLI shape (not a rejection — see
// controlplane/internal/httpapi/gateway_ssh.go).
func (c *Client) DialSSH(ctx context.Context, machineID string) (io.ReadWriteCloser, error) {
	u, err := sshURL(c.BaseURL, machineID)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	header.Set("User-Agent", userAgent)

	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil {
			// A rejected upgrade (401/403/404/409) carries the usual {error,detail}
			// envelope; decode it the same way every other Client call does, so
			// ExitCodeFor/fail() handle it identically.
			return nil, decodeAPIError(resp)
		}
		return nil, fmt.Errorf("dial %s: %w", u, err)
	}
	return websocket.NetConn(ctx, conn, websocket.MessageBinary), nil
}

// sshURL builds the ws(s)://.../gw/ssh?machine=<id> URL from the client's
// http(s) base URL.
func sshURL(baseURL, machineID string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", baseURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("endpoint %q: unsupported scheme %q", baseURL, u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/gw/ssh"
	q := u.Query()
	q.Set("machine", machineID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
