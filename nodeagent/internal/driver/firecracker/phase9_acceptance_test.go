//go:build firecracker && linux

package firecracker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/wsclient"
)

// TestGuestProjectsCwdAndKV is the Phase 9 host acceptance gate (task 9.6b). On a
// real Firecracker microVM booted from the baked rootfs it proves the guest agent
// honors the Phase 9 surface end-to-end, so a host that bakes a broken guest agent
// is caught before it serves:
//
//  1. a cwd-scoped terminal session starts in /workspace/<repo> (pwd over the PTY);
//  2. the control channel's projects.list reports the cloned repo; and
//  3. kv.set/kv.get round-trip the desktop layout through machine SQLite.
//
// It speaks the guest's WebSocket protocols with the node-agent's dependency-free
// wsclient (the production websocket libs are not available in this module). The
// control-channel frame shapes are inlined as JSON because guestwire is not an
// importable dependency here; they mirror guestagent/api.ControlFrame.
func TestGuestProjectsCwdAndKV(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real microVM; skipped in -short")
	}
	d, _, _ := testDriver(t)
	id := "dddddddd-0000-0000-0000-00000000000d"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	const repo = "/workspace/acceptrepo"

	// 1. Create a git repo under /workspace via a plain terminal session.
	setupRepo(t, d, id, repo)

	// 2. A cwd-scoped session must land in the repo. If the directory did not
	//    exist, the guest would reject the cwd pre-upgrade and the dial would fail
	//    — so a successful pwd here also confirms the setup landed.
	assertCwd(t, d, id, repo)

	// 3. The control channel: projects.list sees the repo; kv round-trips.
	assertControlChannel(t, d, id, "acceptrepo")
}

// guestDialer is the slice of *firecracker.Driver the acceptance helpers need.
// Its method set matches the driver's DialGuest exactly so *Driver satisfies it.
type guestDialer interface {
	DialGuest(ctx context.Context, id string, port uint32) (net.Conn, error)
}

// dialGuestWS opens a WebSocket to the guest's terminal port (which serves both
// /terminal and /control) over the node-agent tunnel. The caller closes the
// returned tunnel conn.
//
// api.StateRunning is the node-agent's VM lifecycle state and can precede the
// in-guest agent actually listening on the terminal port — until then the vsock
// CONNECT handshake EOFs ("vsock handshake read: EOF"). So, like the Phase 8 web
// test, retry the dial+handshake until the guest is serving (or a deadline). Once
// the first call succeeds the guest is up and later calls return immediately.
func dialGuestWS(t *testing.T, d guestDialer, id, path string) (*wsclient.Conn, net.Conn) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tunnel, err := d.DialGuest(dctx, id, api.GuestTerminalPort)
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		// Bound the WS handshake: DialGuest clears the conn deadline after the vsock
		// handshake, so without this a guest that accepted the tunnel but is not yet
		// serving HTTP would block the handshake read indefinitely.
		_ = tunnel.SetDeadline(time.Now().Add(10 * time.Second))
		c, err := wsclient.Dial(tunnel, "guest", path)
		if err != nil {
			_ = tunnel.Close()
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		return c, tunnel
	}
	t.Fatalf("ws dial %s never succeeded: %v", path, lastErr)
	return nil, nil
}

// setupRepo creates an initialized git repository at repo via a login-shell
// session, waiting for a completion marker echoed over the PTY.
func setupRepo(t *testing.T, d guestDialer, id, repo string) {
	t.Helper()
	c, tunnel := dialGuestWS(t, d, id, "/terminal?session=setup")
	defer tunnel.Close()
	defer c.Close()

	// hello (text) first.
	if _, isText, err := c.Read(); err != nil || !isText {
		t.Fatalf("setup hello: isText=%v err=%v", isText, err)
	}

	cmd := fmt.Sprintf(
		"mkdir -p %s && git -C %s init -q && echo hi > %s/f && "+
			"git -C %s add -A && git -C %s -c user.email=a@b -c user.name=a commit -q -m init && "+
			"echo SETUP_OK\n",
		repo, repo, repo, repo, repo)
	if err := c.WriteBinary([]byte(cmd)); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	if !readUntil(t, c, "SETUP_OK", 30*time.Second) {
		t.Fatal("setup never reported SETUP_OK")
	}
}

// assertCwd opens a cwd-scoped session and asserts pwd reports repo.
func assertCwd(t *testing.T, d guestDialer, id, repo string) {
	t.Helper()
	c, tunnel := dialGuestWS(t, d, id, "/terminal?session=scoped&cwd="+repo)
	defer tunnel.Close()
	defer c.Close()

	if _, isText, err := c.Read(); err != nil || !isText {
		t.Fatalf("scoped hello: isText=%v err=%v", isText, err)
	}
	if err := c.WriteBinary([]byte("pwd\n")); err != nil {
		t.Fatalf("pwd write: %v", err)
	}
	if !readUntil(t, c, repo, 15*time.Second) {
		t.Fatalf("pwd never reported %s", repo)
	}
}

// readUntil reads binary PTY output until it contains want or the deadline.
func readUntil(t *testing.T, c *wsclient.Conn, want string, d time.Duration) bool {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(d))
	var acc strings.Builder
	for {
		payload, isText, err := c.Read()
		if err != nil {
			return false
		}
		if isText {
			continue // control frames (hello/exit) — not PTY output
		}
		acc.Write(payload)
		if strings.Contains(acc.String(), want) {
			return true
		}
	}
}

// --- control channel ---------------------------------------------------------

type ctlFrame struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Op      string          `json:"op,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func assertControlChannel(t *testing.T, d guestDialer, id, wantRepo string) {
	t.Helper()
	c, tunnel := dialGuestWS(t, d, id, "/control")
	defer tunnel.Close()
	defer c.Close()

	// projects.list → the repo appears.
	resp := ctlRequest(t, c, 1, "projects.list", struct{}{})
	var pl struct {
		Projects []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(resp, &pl); err != nil {
		t.Fatalf("projects.list payload: %v", err)
	}
	found := false
	for _, p := range pl.Projects {
		if p.Name == wantRepo {
			found = true
		}
	}
	if !found {
		t.Fatalf("projects.list did not include %q: %+v", wantRepo, pl.Projects)
	}

	// kv.set then kv.get round-trips the value.
	const layout = `{"windows":[{"id":"w1"}]}`
	ctlRequest(t, c, 2, "kv.set", map[string]string{"key": "desktop.layout", "value": layout})
	getResp := ctlRequest(t, c, 3, "kv.get", map[string]string{"key": "desktop.layout"})
	var kv struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(getResp, &kv); err != nil {
		t.Fatalf("kv.get payload: %v", err)
	}
	if kv.Value == nil || *kv.Value != layout {
		t.Fatalf("kv round-trip = %v, want %q", kv.Value, layout)
	}
}

// ctlRequest sends a req frame and returns the matching resp payload, failing on
// an err frame or a timeout.
func ctlRequest(t *testing.T, c *wsclient.Conn, reqID int64, op string, payload any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", op, err)
	}
	frame, err := json.Marshal(ctlFrame{ID: reqID, Kind: "req", Op: op, Payload: body})
	if err != nil {
		t.Fatalf("marshal %s frame: %v", op, err)
	}
	if err := c.WriteText(frame); err != nil {
		t.Fatalf("write %s: %v", op, err)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	for {
		data, isText, err := c.Read()
		if err != nil {
			t.Fatalf("read %s response: %v", op, err)
		}
		if !isText {
			continue
		}
		var f ctlFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode %s response: %v", op, err)
		}
		if f.ID != reqID {
			continue // a frame for another request (or a guest-initiated req)
		}
		if f.Kind == "err" {
			t.Fatalf("%s returned err frame: %s", op, string(f.Payload))
		}
		return f.Payload
	}
}
