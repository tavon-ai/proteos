package sshfwd

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// echoBackend starts a loopback TCP server that echoes bytes back, standing in
// for sshd. It returns its address.
func echoBackend(t *testing.T) string {
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
	return ln.Addr().String()
}

// serveForwarder runs a Forwarder over a fresh loopback listener pointed at
// backend and returns its address, cancelling on test cleanup.
func serveForwarder(t *testing.T, backend string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := New(ln, backend)
	go func() { _ = f.Serve(ctx) }()
	return ln.Addr().String()
}

// TestForwarderBridgesToBackend verifies every accepted connection is bridged
// straight to the configured backend — no preamble, no port choice by the
// client (unlike previewfwd, whose whole point is a client-chosen target).
func TestForwarderBridgesToBackend(t *testing.T) {
	backend := echoBackend(t)
	fwdAddr := serveForwarder(t, backend)

	conn, err := net.Dial("tcp", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte("ping")); err != nil {
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

// TestForwarderDropsOnUnreachableBackend verifies a dead backend (sshd not up,
// or not baked into the image) just closes the client connection rather than
// hanging — the gateway maps that to a 502.
func TestForwarderDropsOnUnreachableBackend(t *testing.T) {
	// A loopback address nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close() // now guaranteed unreachable

	fwdAddr := serveForwarder(t, dead)
	conn, err := net.Dial("tcp", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("expected closed connection, read %d bytes", n)
	}
}

// TestNewDefaultsBackend verifies an empty backend falls back to
// DefaultBackend (127.0.0.1:22), matching the guest's baked sshd_config.
func TestNewDefaultsBackend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := New(ln, "")
	if f.backend != DefaultBackend {
		t.Fatalf("backend = %q, want %q", f.backend, DefaultBackend)
	}
}
