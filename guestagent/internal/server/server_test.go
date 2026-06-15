package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/term"
)

// newTestServer starts an httptest server over the real WS handler.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mgr := term.NewManager(term.Config{Shell: "/bin/bash", ScrollbackKiB: 256})
	t.Cleanup(mgr.Shutdown)
	ts := httptest.NewServer(New(mgr, nil, nil, nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server, session string) string {
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/terminal"
	if session != "" {
		u += "?session=" + session
	}
	return u
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return c
}

// readHello reads and asserts the first frame is a hello, returning it.
func readHello(t *testing.T, c *websocket.Conn) guestwire.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("hello frame type = %v, want text", typ)
	}
	var f guestwire.Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if f.Type != guestwire.FrameHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	return f
}

// readBinaryUntil accumulates binary output until it contains want.
func readBinaryUntil(t *testing.T, c *websocket.Conn, want string, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var acc strings.Builder
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (acc so far: %q)", err, acc.String())
		}
		if typ == websocket.MessageBinary {
			acc.Write(data)
			if strings.Contains(acc.String(), want) {
				return acc.String()
			}
		}
	}
}

func sendInput(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

func TestInteractiveShell(t *testing.T) {
	ts := newTestServer(t)
	c := dial(t, wsURL(ts, "main"))
	defer c.Close(websocket.StatusNormalClosure, "")

	readHello(t, c)
	sendInput(t, c, "echo interactive-marker\n")
	readBinaryUntil(t, c, "interactive-marker", 5*time.Second)
}

func TestReattachReplaysScrollback(t *testing.T) {
	ts := newTestServer(t)

	// First attach: produce output, then drop the connection.
	c1 := dial(t, wsURL(ts, "main"))
	readHello(t, c1)
	sendInput(t, c1, "echo persist-me-123\n")
	readBinaryUntil(t, c1, "persist-me-123", 5*time.Second)
	c1.Close(websocket.StatusNormalClosure, "")

	// Reattach: the hello announces a non-empty replay, and the replayed bytes
	// contain the earlier output (session survived the WS drop).
	c2 := dial(t, wsURL(ts, "main"))
	defer c2.Close(websocket.StatusNormalClosure, "")
	hello := readHello(t, c2)
	if hello.ReplayBytes == 0 {
		t.Fatal("reattach hello reported zero replay bytes")
	}
	readBinaryUntil(t, c2, "persist-me-123", 5*time.Second)
}

func TestConcurrentAttachesBothReceive(t *testing.T) {
	ts := newTestServer(t)

	c1 := dial(t, wsURL(ts, "main"))
	defer c1.Close(websocket.StatusNormalClosure, "")
	readHello(t, c1)

	c2 := dial(t, wsURL(ts, "main"))
	defer c2.Close(websocket.StatusNormalClosure, "")
	readHello(t, c2)

	// Input on c1 reaches the shared shell; both attachments see the output.
	sendInput(t, c1, "echo shared-fanout\n")
	readBinaryUntil(t, c1, "shared-fanout", 5*time.Second)
	readBinaryUntil(t, c2, "shared-fanout", 5*time.Second)
}

func TestResizeControlFrame(t *testing.T) {
	ts := newTestServer(t)
	c := dial(t, wsURL(ts, "main"))
	defer c.Close(websocket.StatusNormalClosure, "")
	readHello(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resize, _ := json.Marshal(guestwire.Frame{Type: guestwire.FrameResize, Cols: 120, Rows: 40})
	if err := c.Write(ctx, websocket.MessageText, resize); err != nil {
		t.Fatal(err)
	}
	sendInput(t, c, "stty size\n")
	readBinaryUntil(t, c, "40 120", 5*time.Second)
}

func TestExitFrameOnShellExit(t *testing.T) {
	ts := newTestServer(t)
	c := dial(t, wsURL(ts, "exit-sess"))
	defer c.Close(websocket.StatusNormalClosure, "")
	readHello(t, c)

	sendInput(t, c, "exit 3\n")

	// Eventually an exit frame with code 3 arrives, then the socket closes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("expected exit frame before close: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var f guestwire.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if f.Type == guestwire.FrameExit {
			if f.ExitCode == nil || *f.ExitCode != 3 {
				t.Fatalf("exit code = %v, want 3", f.ExitCode)
			}
			return
		}
	}
}

func TestInvalidSessionNameRejected(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Upper-case is outside [a-z0-9-]; the upgrade must fail (HTTP 400).
	_, _, err := websocket.Dial(ctx, wsURL(ts, "BadName"), nil)
	if err == nil {
		t.Fatal("expected dial to fail for invalid session name")
	}
}
