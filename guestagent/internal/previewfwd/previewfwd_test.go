package previewfwd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// echoBackend starts a loopback TCP server that echoes bytes back, standing in
// for a dev server the user runs inside the VM. It returns the port it bound.
func echoBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// serveForwarder runs a Forwarder over a fresh loopback listener and returns its
// address, cancelling on test cleanup.
func serveForwarder(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := New(ln)
	go func() { _ = f.Serve(ctx) }()
	return ln.Addr().String()
}

// TestForwarderBridgesToPreamblePort verifies the forwarder reads "<port>\n" and
// bridges the rest of the stream to 127.0.0.1:<port>, preserving bytes that
// arrive in the same write as the preamble.
func TestForwarderBridgesToPreamblePort(t *testing.T) {
	backendPort := echoBackend(t)
	fwdAddr := serveForwarder(t)

	conn, err := net.Dial("tcp", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Preamble + payload in one write: the payload must survive the bufio reader
	// that consumed the preamble.
	if _, err := fmt.Fprintf(conn, "%d\nping", backendPort); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
}

// TestForwarderDropsBadPreamble verifies a malformed preamble closes the
// connection (the gateway maps that to a 502) rather than dialing anything.
func TestForwarderDropsBadPreamble(t *testing.T) {
	fwdAddr := serveForwarder(t)
	conn, err := net.Dial("tcp", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprint(conn, "not-a-port\n"); err != nil {
		t.Fatal(err)
	}
	// The forwarder should close without sending anything.
	if n, err := bufio.NewReader(conn).Read(make([]byte, 1)); err == nil {
		t.Fatalf("expected closed connection, read %d bytes", n)
	}
}

// TestParsePort covers the preamble port validation directly.
func TestParsePort(t *testing.T) {
	cases := []struct {
		in   string
		port uint32
		ok   bool
	}{
		{"3000\n", 3000, true},
		{"1026\n", 1026, true},
		{"65535\n", 65535, true},
		{"  8080  \n", 8080, true},
		{"0\n", 0, false},
		{"65536\n", 0, false},
		{"abc\n", 0, false},
		{"\n", 0, false},
	}
	for _, tc := range cases {
		got, ok := parsePort(tc.in)
		if ok != tc.ok || got != tc.port {
			t.Errorf("parsePort(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.port, tc.ok)
		}
	}
}
