//go:build firecracker && linux

package firecracker_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// TestGuestVsockTerminal proves the Phase 3 vsock path end-to-end on a real
// Firecracker microVM booted from the baked rootfs (Task 3.7): the driver dials
// the guest agent through the jailed vsock uds, a WebSocket terminal session
// runs a command, and after dropping and reattaching the scrollback replay still
// contains the earlier output.
//
// It requires PROTEOS_TEST_ROOTFS to point at a rootfs with the guest agent
// baked in (image/build-rootfs.sh); a plain base rootfs has no guest agent and
// the test will (correctly) fail at the WebSocket handshake. The node-agent
// itself stays dependency-free — this test hand-rolls a minimal WS client over
// the dialed conn rather than importing a WebSocket library.
func TestGuestVsockTerminal(t *testing.T) {
	d, _, _ := testDriver(t)
	id := "dddddddd-0000-0000-0000-000000000004"
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	waitState(t, d, id, api.StateRunning, 30*time.Second)

	// The guest agent comes up via systemd shortly after boot; give the vsock
	// listener a moment, retrying DialGuest + handshake.
	marker := "vsock-e2e-marker"
	dialAndAttach := func() (*bufio.Reader, net.Conn) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			conn, err := d.DialGuest(dctx, id, api.GuestTerminalPort)
			cancel()
			if err == nil {
				if br, herr := wsHandshake(conn, "/terminal?session=main"); herr == nil {
					return br, conn
				}
				conn.Close()
			}
			if time.Now().After(deadline) {
				t.Fatalf("guest terminal never became reachable over vsock: %v", err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// First attach: run a command and see its output.
	br, conn := dialAndAttach()
	readWSUntil(t, br, "hello", 5*time.Second) // text hello frame
	if err := wsWriteFrame(conn, wsOpcodeBinary, []byte("echo "+marker+"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	readWSUntil(t, br, marker, 8*time.Second)
	conn.Close() // drop the connection; the session must survive

	// Reattach: the scrollback replay must still contain the earlier marker.
	br2, conn2 := dialAndAttach()
	defer conn2.Close()
	readWSUntil(t, br2, "hello", 5*time.Second)
	readWSUntil(t, br2, marker, 8*time.Second)
}

// --- minimal stdlib-only WebSocket client (gated test use only) -------------

const (
	wsOpcodeText   = 0x1
	wsOpcodeBinary = 0x2
	wsOpcodeClose  = 0x8
	wsOpcodePing   = 0x9
	wsOpcodePong   = 0xA
	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// wsHandshake performs the client upgrade over conn and returns a buffered
// reader positioned at the first WebSocket frame.
func wsHandshake(conn net.Conn, path string) (*bufio.Reader, error) {
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: guest\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("ws handshake: status %d", resp.StatusCode)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		return nil, fmt.Errorf("ws handshake: accept %q != %q", got, want)
	}
	return br, nil
}

// wsWriteFrame writes a single masked client frame (clients MUST mask).
func wsWriteFrame(conn net.Conn, opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode) // FIN + opcode
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	_, err := conn.Write(masked)
	return err
}

// wsReadFrame reads one server frame (server→client frames are unmasked).
func wsReadFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	h0, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode = h0 & 0x0f
	h1, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n := int(h1 & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// readWSUntil reads frames until the accumulated text/binary payload contains
// want, replying to pings and failing on close or timeout.
func readWSUntil(t *testing.T, br *bufio.Reader, want string, d time.Duration) {
	t.Helper()
	var acc strings.Builder
	deadline := time.Now().Add(d)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; got %q", want, acc.String())
		}
		op, payload, err := wsReadFrame(br)
		if err != nil {
			t.Fatalf("read frame: %v (acc=%q)", err, acc.String())
		}
		switch op {
		case wsOpcodeText, wsOpcodeBinary:
			acc.Write(payload)
			if strings.Contains(acc.String(), want) {
				return
			}
		case wsOpcodeClose:
			t.Fatalf("server closed before %q; got %q", want, acc.String())
		case wsOpcodePing:
			// ignore; the test does not need to keep the conn alive that long
		}
	}
}
