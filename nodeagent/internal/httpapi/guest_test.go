package httpapi_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/httpapi"
)

// fakeGuestDriver is a Driver that reports a configurable state and dials a
// fixed address as "the guest", so the tunnel handler can be exercised without
// a real VM or guest agent.
type fakeGuestDriver struct {
	state    string
	guestNet string // network/addr to dial as the guest
	guestAdr string
	dialErr  error
	unknown  bool // Status returns ErrUnknownMachine
}

func (f *fakeGuestDriver) EnsureRunning(context.Context, driver.VMSpec) (string, error) {
	return "h", nil
}
func (f *fakeGuestDriver) Stop(context.Context, string, driver.StopMode) error { return nil }
func (f *fakeGuestDriver) Status(_ context.Context, id string) (driver.Status, error) {
	if f.unknown {
		return driver.Status{}, driver.ErrUnknownMachine
	}
	return driver.Status{MachineID: id, State: f.state}, nil
}
func (f *fakeGuestDriver) Destroy(context.Context, string) error          { return nil }
func (f *fakeGuestDriver) List(context.Context) ([]driver.Status, error)  { return nil, nil }
func (f *fakeGuestDriver) Reattach(context.Context) error                 { return nil }
func (f *fakeGuestDriver) DialGuest(ctx context.Context, _ string) (net.Conn, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	var d net.Dialer
	return d.DialContext(ctx, f.guestNet, f.guestAdr)
}

// echoServer accepts connections and echoes bytes back until closed. closeFn
// closes the listener AND every accepted connection (so "the guest went away"
// is simulated by dropping the live conn, which a plain listener close does
// not do), then waits for the handlers to drain.
func echoServer(t *testing.T) (network, addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	closeFn = func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}
	return "tcp", ln.Addr().String(), closeFn
}

// openTunnel performs the raw HTTP Upgrade handshake against the guest route and
// returns the hijacked connection plus a reader positioned after the response
// headers. headerOverride can mutate the request line/headers for negative tests.
func openTunnel(t *testing.T, ts *httptest.Server, id, token string, withUpgrade bool) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	u := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("GET /v1/machines/" + id + "/guest HTTP/1.1\r\n")
	b.WriteString("Host: " + u + "\r\n")
	if token != "" {
		b.WriteString("Authorization: " + api.BearerPrefix + token + "\r\n")
	}
	if withUpgrade {
		b.WriteString("Connection: Upgrade\r\n")
		b.WriteString("Upgrade: " + api.UpgradeGuestProto + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	return conn, br, statusLine
}

func TestGuestTunnelEchoRoundTrip(t *testing.T) {
	network, addr, closeEcho := echoServer(t)
	defer closeEcho()

	drv := &fakeGuestDriver{state: api.StateRunning, guestNet: network, guestAdr: addr}
	ts := httptest.NewServer(httpapi.New(testToken, drv).Handler())
	defer ts.Close()

	conn, br, status := openTunnel(t, ts, "m1", testToken, true)
	defer conn.Close()
	if !strings.Contains(status, "101") {
		t.Fatalf("want 101 Switching Protocols, got %q", status)
	}
	// Consume the rest of the upgrade response headers (until blank line).
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Bytes written now flow through the node-agent tunnel to the echo "guest".
	want := "hello-through-the-tunnel"
	if _, err := conn.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != want {
		t.Fatalf("echo = %q, want %q", buf, want)
	}
}

func TestGuestTunnelClosesWhenGuestGone(t *testing.T) {
	network, addr, closeEcho := echoServer(t)
	drv := &fakeGuestDriver{state: api.StateRunning, guestNet: network, guestAdr: addr}
	ts := httptest.NewServer(httpapi.New(testToken, drv).Handler())
	defer ts.Close()

	conn, br, status := openTunnel(t, ts, "m1", testToken, true)
	defer conn.Close()
	if !strings.Contains(status, "101") {
		t.Fatalf("want 101, got %q", status)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	// Tearing down the guest side closes the tunnel: the client read hits EOF.
	closeEcho()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected tunnel to close after guest went away")
	}
}

func TestGuestTunnelAuthz(t *testing.T) {
	network, addr, closeEcho := echoServer(t)
	defer closeEcho()

	cases := []struct {
		name        string
		token       string
		withUpgrade bool
		state       string
		unknown     bool
		wantCode    string
	}{
		{"no token", "", true, api.StateRunning, false, "401"},
		{"wrong token", "nope", true, api.StateRunning, false, "401"},
		{"missing upgrade header", testToken, false, api.StateRunning, false, "400"},
		{"not running", testToken, true, api.StateStopped, false, "409"},
		{"unknown machine", testToken, true, api.StateRunning, true, "404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := &fakeGuestDriver{state: tc.state, guestNet: network, guestAdr: addr, unknown: tc.unknown}
			ts := httptest.NewServer(httpapi.New(testToken, drv).Handler())
			defer ts.Close()
			conn, _, status := openTunnel(t, ts, "m1", tc.token, tc.withUpgrade)
			defer conn.Close()
			if !strings.Contains(status, tc.wantCode) {
				t.Fatalf("status = %q, want %s", status, tc.wantCode)
			}
		})
	}
}
