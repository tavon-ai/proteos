package wsclient

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestAcceptKey pins the RFC 6455 §1.3 worked example so the handshake check can
// never silently regress.
func TestAcceptKey(t *testing.T) {
	if got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q, want the RFC example", got)
	}
}

// testServer runs a minimal WebSocket server on conn using this package's own
// frame helpers: it completes the handshake, then echoes each client message
// back (text→text, binary→binary) until the client closes. Running both ends
// through the same codec validates the handshake + masked framing end-to-end on
// any OS, without the production websocket library the node-agent can't depend on.
func testServer(t *testing.T, conn net.Conn) {
	t.Helper()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := io.WriteString(conn, resp); err != nil {
		return
	}
	for {
		op, payload, err := readFrame(br)
		if err != nil {
			return
		}
		switch op {
		case opText, opBinary:
			// Echo unmasked (server→client frames are never masked).
			if err := writeFrame(conn, op, payload, false); err != nil {
				return
			}
		case opClose:
			return
		}
	}
}

func TestClientServerRoundTrip(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go testServer(t, serverConn)

	c, err := Dial(clientConn, "guest", "/terminal?session=x&cwd=/workspace/a")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	// Text round-trip.
	if err := c.WriteText([]byte(`{"op":"hi"}`)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got, isText, err := c.Read()
	if err != nil {
		t.Fatalf("Read text: %v", err)
	}
	if !isText || string(got) != `{"op":"hi"}` {
		t.Fatalf("text round-trip = (%q, isText=%v)", got, isText)
	}

	// Binary round-trip, including a payload that exercises the 16-bit length path.
	big := make([]byte, 1000)
	for i := range big {
		big[i] = byte(i)
	}
	if err := c.WriteBinary(big); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	got, isText, err = c.Read()
	if err != nil {
		t.Fatalf("Read binary: %v", err)
	}
	if isText || len(got) != len(big) {
		t.Fatalf("binary round-trip len = %d (isText=%v)", len(got), isText)
	}
	for i := range big {
		if got[i] != big[i] {
			t.Fatalf("binary payload mismatch at %d", i)
		}
	}
}

func TestDialRejectsBadAccept(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// A server that returns a wrong accept key must fail the client handshake.
	go func() {
		br := bufio.NewReader(serverConn)
		_, _ = http.ReadRequest(br)
		_, _ = io.WriteString(serverConn,
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n")
	}()

	if _, err := Dial(clientConn, "guest", "/control"); err == nil {
		t.Fatal("Dial accepted a bad accept key")
	}
}

func TestDialRejectsNon101(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		br := bufio.NewReader(serverConn)
		_, _ = http.ReadRequest(br)
		_, _ = io.WriteString(serverConn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
	}()

	if _, err := Dial(clientConn, "guest", "/terminal?cwd=/etc"); err == nil {
		t.Fatal("Dial accepted a non-101 response")
	}
}
