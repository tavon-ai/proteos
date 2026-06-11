// Package nodeclient is the control plane's HTTP client for the node-agent's
// agentapi wire contract. It is the only place the control plane talks to an
// agent; the direction of trust is one-way (control plane dials agent), so this
// holds the single shared bearer credential. The agentapi types come from the
// nodeagent module via the workspace (go.work) — no replace directive.
package nodeclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// ErrUnknownMachine is returned when the agent reports 404 unknown_machine.
var ErrUnknownMachine = errors.New("nodeclient: agent does not know this machine")

// ErrNotRunning is returned when the guest tunnel is requested for a machine
// the agent reports is not running (409).
var ErrNotRunning = errors.New("nodeclient: machine not running")

// ErrGuestUnreachable is returned when the agent cannot reach the guest (502).
var ErrGuestUnreachable = errors.New("nodeclient: guest unreachable")

// Client dials a single node-agent.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	tlsCfg  *tls.Config // pinned CA for HTTPS agents (Phase 4 decision #3); nil ⇒ system roots / plain HTTP
}

// New returns a client for the agent at baseURL authenticating with token over
// plain HTTP (dev) or HTTPS with the system trust store.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// NewPinned returns a client that verifies the agent's TLS certificate against
// the PEM CA/cert in caFile (decision #3: the channel now carries volume keys,
// so the agent cert is pinned rather than trusted via the system store). caFile
// empty ⇒ behaves like New.
func NewPinned(baseURL, token, caFile string) (*Client, error) {
	c := New(baseURL, token)
	if caFile == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read node CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("node CA file %q contains no usable certificates", caFile)
	}
	c.tlsCfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c.http = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: c.tlsCfg.Clone()},
	}
	return c, nil
}

// BaseURL returns the agent's base URL (no trailing slash).
func (c *Client) BaseURL() string { return c.baseURL }

// Ensure issues PUT /v1/machines/{id} (idempotent ensure-running).
func (c *Client) Ensure(ctx context.Context, id string, req agentapi.EnsureRequest) (agentapi.EnsureResponse, error) {
	var out agentapi.EnsureResponse
	err := c.do(ctx, http.MethodPut, "/v1/machines/"+id, req, &out, http.StatusAccepted)
	return out, err
}

// Stop issues POST /v1/machines/{id}/stop with the requested mode (Phase 4:
// "hibernate" pauses+snapshots, "poweroff" is a cold shutdown). An empty mode
// lets the agent default (hibernate).
func (c *Client) Stop(ctx context.Context, id, mode string) error {
	var body any
	if mode != "" {
		body = agentapi.StopRequest{Mode: mode}
	}
	return c.do(ctx, http.MethodPost, "/v1/machines/"+id+"/stop", body, nil, http.StatusAccepted)
}

// Status issues GET /v1/machines/{id}. A 404 maps to ErrUnknownMachine.
func (c *Client) Status(ctx context.Context, id string) (agentapi.MachineStatus, error) {
	var out agentapi.MachineStatus
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+id, nil, &out, http.StatusOK)
	return out, err
}

// List issues GET /v1/machines (reconciliation).
func (c *Client) List(ctx context.Context) ([]agentapi.MachineStatus, error) {
	var out agentapi.ListResponse
	err := c.do(ctx, http.MethodGet, "/v1/machines", nil, &out, http.StatusOK)
	return out.Machines, err
}

// Destroy issues DELETE /v1/machines/{id} (cleanup).
func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/machines/"+id, nil, nil, http.StatusNoContent)
}

// Health issues GET /healthz (unauthenticated on the agent, but we send the
// header anyway — it is ignored).
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil, http.StatusOK)
}

// DialGuest opens the opaque byte tunnel to a machine's guest agent. It dials
// the node-agent, performs the HTTP Upgrade handshake manually (net/http's
// client refuses non-WebSocket upgrades), and on 101 returns the hijacked
// connection — any bytes the agent buffered after the response headers are
// preserved. The control-plane gateway then speaks WebSocket to the guest over
// this conn. The returned conn is the caller's to close.
func (c *Client) DialGuest(ctx context.Context, machineID string) (net.Conn, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse node-agent url: %w", err)
	}

	conn, err := c.dialRaw(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("dial node-agent: %w", err)
	}

	// Honour the context deadline for the handshake.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/machines/"+machineID+"/guest", nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set(agentapi.AuthHeader, agentapi.BearerPrefix+c.token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", agentapi.UpgradeGuestProto)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write upgrade request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, ErrUnknownMachine
		case http.StatusConflict:
			return nil, ErrNotRunning
		case http.StatusBadGateway:
			return nil, ErrGuestUnreachable
		case http.StatusUnauthorized:
			return nil, errors.New("nodeclient: guest tunnel unauthorized")
		default:
			return nil, fmt.Errorf("guest tunnel: agent returned %d", resp.StatusCode)
		}
	}

	// Clear the handshake deadline; the gateway manages timeouts thereafter.
	_ = conn.SetDeadline(time.Time{})
	// Preserve any bytes buffered past the response headers.
	return &bufferedConn{Conn: conn, r: br}, nil
}

// dialRaw opens a TCP (or TLS) connection to the node-agent's host.
func (c *Client) dialRaw(ctx context.Context, u *url.URL) (net.Conn, error) {
	host := u.Host
	var d net.Dialer
	switch u.Scheme {
	case "http":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
		return d.DialContext(ctx, "tcp", host)
	case "https":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
		td := &tls.Dialer{NetDialer: &d}
		if c.tlsCfg != nil {
			td.Config = c.tlsCfg.Clone() // pin the agent cert (decision #3)
		}
		return td.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
}

// bufferedConn is a net.Conn whose reads first drain a bufio.Reader (which may
// hold bytes the peer sent immediately after the upgrade response) before
// falling through to the underlying connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// do performs one request, encoding body (if non-nil) and decoding into out (if
// non-nil), asserting the response status equals wantStatus. A 404 is mapped to
// ErrUnknownMachine before the status check so callers can branch on it.
func (c *Client) do(ctx context.Context, method, path string, body, out any, wantStatus int) error {
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(agentapi.AuthHeader, agentapi.BearerPrefix+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrUnknownMachine
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: agent returned %d (want %d)", method, path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
