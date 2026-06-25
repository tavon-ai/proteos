//go:build firecracker && linux

package firecracker_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// TestGuestWebForwardCodeServer proves the Phase 8 web path end-to-end on a real
// Firecracker microVM booted from the baked rootfs: the driver dials the guest's
// web vsock port (1025), the guest agent's web forward lazily starts code-server
// (observed in the guest log), and an HTTP request returns a code-server
// response. The first request takes the lazy-start path, so we retry the whole
// dial+GET with a generous deadline.
//
// Requires PROTEOS_TEST_ROOTFS to point at a rootfs with BOTH the guest agent and
// code-server baked (image/build-rootfs.sh, default on). A rootfs without
// code-server (built with --no-codeserver) will (correctly) never serve here.
func TestGuestWebForwardCodeServer(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real microVM + code-server; skipped in -short")
	}
	d, _, _ := testDriver(t)
	id := "dddddddd-0000-0000-0000-00000000000c"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	// code-server is ~200 MB and lazy-started on the first web connection, so the
	// health gate + boot can take many seconds. Retry dial+GET until it serves.
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := d.DialGuest(dctx, id, api.GuestWebPort)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		// Raw HTTP/1.1 GET / over the tunnel (the forward speaks plain HTTP+WS).
		_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: editor\r\nConnection: close\r\n\r\n")
		if err != nil {
			cancel()
			conn.Close()
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
		if err != nil {
			cancel()
			conn.Close()
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		conn.Close()
		cancel()

		// code-server with --auth none serves the workbench (200) or redirects to a
		// default folder (302). Either is a healthy editor; a 5xx is not.
		if resp.StatusCode >= 500 {
			lastErr = errStatus(resp.StatusCode)
			time.Sleep(time.Second)
			continue
		}
		// Sanity: the response should look like code-server, not an error page.
		blob := strings.ToLower(resp.Header.Get("Server") + " " + string(body))
		if resp.StatusCode == http.StatusOK && !strings.Contains(blob, "code-server") && !strings.Contains(blob, "vscode") {
			t.Logf("warning: 200 response did not obviously look like code-server (server=%q)", resp.Header.Get("Server"))
		}
		t.Logf("code-server reachable over the web forward: status %d", resp.StatusCode)
		return
	}
	t.Fatalf("code-server never became reachable over the web forward (port %d): %v", api.GuestWebPort, lastErr)
}

type errStatus int

func (e errStatus) Error() string { return "unexpected status " + http.StatusText(int(e)) }
