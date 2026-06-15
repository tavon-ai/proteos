package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	guestwire "github.com/tavon/proteos/guestagent/api"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// TestGatewayTerminalE2E exercises the whole Phase 3 path on a Mac/Linux dev
// stack with no hypervisor: a real node-agent (DevDriver) launches a real guest
// agent per machine; the control-plane gateway proxies a browser WebSocket
// through the node-agent tunnel to that guest. It rounds-trips a shell command
// and proves session revocation closes the live WS with 4001.
func TestGatewayTerminalE2E(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available to build agents")
	}
	root := repoRoot(t)
	guestBin := buildBinary(t, filepath.Join(root, "guestagent"), "./cmd/guestagent")
	agentBin := buildBinary(t, filepath.Join(root, "nodeagent"), "./cmd/nodeagent")

	const agentToken = "e2e-agent-token"
	nodeURL := startNodeAgent(t, agentBin, guestBin, agentToken)
	nodes := nodeclient.New(nodeURL, agentToken)

	// Control plane with a running machine for the seeded user.
	fx := setupCP(t, nodes, []string{testWSOrigin})

	// Bring the same machine up on the node-agent and wait for it to run (the
	// guest agent is launched at boot on the machine's guest.sock).
	ctx := context.Background()
	if _, err := nodes.Ensure(ctx, fx.machineID, agentapi.EnsureRequest{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"}); err != nil {
		t.Fatalf("ensure on node-agent: %v", err)
	}
	// Destroy on cleanup so the guest-agent child is killed (it would otherwise
	// outlive the node-agent). Runs before the node-agent is killed (LIFO).
	t.Cleanup(func() { _ = nodes.Destroy(context.Background(), fx.machineID) })
	waitNodeRunning(t, nodes, fx.machineID)
	// The machine reaches "running" before the freshly-exec'd guest agent has
	// bound its socket; in production the client's reconnect backoff covers this
	// gap. For a deterministic test, wait until the tunnel actually reaches the
	// guest before opening the browser WebSocket.
	waitGuestReachable(t, nodes, fx.machineID)

	// --- happy path: open the terminal and round-trip a command -------------
	c := dialBrowser(t, fx, "main")
	defer c.Close(websocket.StatusNormalClosure, "")

	if f := readControlFrame(t, c); f.Type != guestwire.FrameHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	writeBinary(t, c, "echo gw-e2e-marker\n")
	if got := readBinaryUntil(t, c, "gw-e2e-marker", 8*time.Second); !strings.Contains(got, "gw-e2e-marker") {
		t.Fatalf("did not see command output; got %q", got)
	}

	// --- revocation: logging out the session closes the live WS with 4001 ---
	c2 := dialBrowser(t, fx, "revoke-sess")
	defer c2.Close(websocket.StatusNormalClosure, "")
	readControlFrame(t, c2) // hello

	if err := fx.sessions.Revoke(ctx, fx.token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The next read should fail with the session_revoked close code.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c2.Read(rctx); err != nil {
			if code := websocket.CloseStatus(err); code != guestwire.CloseSessionRevoked {
				t.Fatalf("close code = %d, want %d (session_revoked)", code, guestwire.CloseSessionRevoked)
			}
			break
		}
	}
}

// --- test plumbing ----------------------------------------------------------

// repoRoot walks up from the test's working directory to the dir holding go.work.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found walking up from test dir")
		}
		dir = parent
	}
}

// buildBinary compiles pkg within moduleDir to a temp file and returns its path.
func buildBinary(t *testing.T, moduleDir, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = moduleDir
	cmd.Env = os.Environ()
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s in %s: %v\n%s", pkg, moduleDir, err, combined)
	}
	return out
}

// freeAddr returns a probably-free 127.0.0.1 host:port (small race window).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startNodeAgent runs the node-agent binary (DevDriver + the guest-agent binary)
// as a subprocess and returns its base URL once healthy.
func startNodeAgent(t *testing.T, agentBin, guestBin, token string, extraEnv ...string) string {
	t.Helper()
	addr := freeAddr(t)
	// Keep the data dir short: the guest agent's unix socket lives at
	// <dataDir>/machines/<uuid>/guest.sock, which must fit the ~104-char
	// sun_path limit (t.TempDir() on macOS is far too long).
	dataDir, err := os.MkdirTemp("/tmp", "pnode")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	cmd := exec.Command(agentBin)
	cmd.Env = append(os.Environ(),
		"PROTEOS_AGENT_ADDR="+addr,
		"PROTEOS_AGENT_TOKEN="+token,
		"PROTEOS_AGENT_DRIVER=dev",
		"PROTEOS_AGENT_DATA_DIR="+dataDir,
		"PROTEOS_DEV_GUESTAGENT_BIN="+guestBin,
		"PROTEOS_DEV_BOOT_DELAY=150ms",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	// Log to a regular file rather than the test's stdout: the node-agent
	// leaves "VMs" (here, guest-agent processes) running across its own
	// shutdown by design, and a grandchild inheriting the test binary's stdout
	// pipe would keep `go test` blocked after the test exits. (We also Destroy
	// the machine in the test cleanup to actually stop the guest agent.)
	logf, err := os.CreateTemp("", "nodeagent-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node-agent: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			if data, rerr := os.ReadFile(logf.Name()); rerr == nil {
				t.Logf("node-agent log:\n%s", data)
			}
		}
		logf.Close()
		_ = os.Remove(logf.Name())
	})

	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("node-agent did not become healthy")
	return ""
}

// waitNodeRunning polls the node-agent until the machine reports running.
func waitNodeRunning(t *testing.T, nodes *nodeclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := nodes.Status(context.Background(), id)
		if err == nil && st.State == agentapi.StateRunning {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("machine never reached running on the node-agent")
}

// waitGuestReachable polls the guest tunnel until it connects end-to-end (the
// guest agent has bound its socket), then drops the probe connection.
func waitGuestReachable(t *testing.T, nodes *nodeclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := nodes.DialGuest(ctx, id, agentapi.GuestTerminalPort)
		cancel()
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("guest agent never became reachable through the tunnel")
}

// dialBrowser opens the browser-side terminal WebSocket with a valid session
// cookie and allowed Origin.
func dialBrowser(t *testing.T, fx cpFixture, session string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Origin", testWSOrigin)
	hdr.Set("Cookie", auth.SessionCookieName+"="+fx.token)
	u := "ws" + strings.TrimPrefix(fx.url, "http") + "/gw/terminal?session=" + session
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("browser dial: %v", err)
	}
	c.SetReadLimit(8 << 20)
	return c
}

func readControlFrame(t *testing.T, c *websocket.Conn) guestwire.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read control frame: %v", err)
		}
		if typ == websocket.MessageText {
			var f guestwire.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			return f
		}
	}
}

func writeBinary(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readBinaryUntil(t *testing.T, c *websocket.Conn, want string, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var acc bytes.Buffer
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (acc=%q)", err, acc.String())
		}
		if typ == websocket.MessageBinary {
			acc.Write(data)
			if strings.Contains(acc.String(), want) {
				return acc.String()
			}
		}
	}
}
