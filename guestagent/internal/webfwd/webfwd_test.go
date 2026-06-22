package webfwd

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestHelperProcess is the fake "code-server": when invoked with GO_WANT_HELPER
// set, it removes any stale socket, listens on the unix path in FAKE_ADDR, and
// echoes bytes until killed. The supervisor runs the test binary itself with
// this env (the standard os/exec helper-process pattern), so no separate binary
// has to be built.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") != "1" {
		return
	}
	addr := os.Getenv("FAKE_ADDR")
	_ = os.Remove(addr)
	ln, err := net.Listen("unix", addr)
	if err != nil {
		os.Exit(2)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) { _, _ = io.Copy(c, c); c.Close() }(c)
	}
}

// fakeSupervisorConfig points a Supervisor at the helper process over a unix
// socket (TCP would flake on restart due to TIME_WAIT), with test-short timeouts.
func fakeSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "webfwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := filepath.Join(dir, "cs.sock")

	sup := NewSupervisor(SupervisorConfig{
		Bin:  os.Args[0],
		Args: []string{"-test.run=TestHelperProcess"},
		Env:  append(os.Environ(), "GO_WANT_HELPER=1", "FAKE_ADDR="+addr),
		Addr: addr,
	})
	sup.startTimeout = 5 * time.Second
	sup.baseBackoff = 20 * time.Millisecond
	sup.maxBackoff = 100 * time.Millisecond
	// Health-probe + the eventual forward dial both go over the unix socket.
	sup.dialBackend = func(ctx context.Context, a string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", a)
	}
	t.Cleanup(sup.Shutdown)
	return sup
}

func TestSupervisorLazyStartAndHealthGate(t *testing.T) {
	sup := fakeSupervisor(t)
	if sup.alive() {
		t.Fatal("code-server should not be running before the first Ensure")
	}
	if err := sup.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !sup.alive() || !sup.healthy {
		t.Fatal("Ensure should leave code-server running and healthy")
	}
	// A second Ensure is a fast no-op (still healthy).
	if err := sup.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

func TestSupervisorRestartsAfterCrash(t *testing.T) {
	sup := fakeSupervisor(t)
	if err := sup.Ensure(context.Background()); err != nil {
		t.Fatalf("initial Ensure: %v", err)
	}
	sup.mu.Lock()
	pid := sup.cmd.Process.Pid
	sup.mu.Unlock()

	// kill -9 the process; the reaper marks it dead.
	if err := sup.cmd.Process.Signal(os.Kill); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sup.mu.Lock()
		dead := sup.cmd == nil
		sup.mu.Unlock()
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reaper never observed the crash")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Next Ensure restarts it (a fresh pid) and re-gates health.
	if err := sup.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after crash: %v", err)
	}
	sup.mu.Lock()
	newPid := sup.cmd.Process.Pid
	sup.mu.Unlock()
	if newPid == pid {
		t.Fatal("expected a new code-server process after the crash")
	}
}

func TestForwarderRoundTrip(t *testing.T) {
	// Stub backend: an echo server on a unix socket (stands in for code-server).
	dir, err := os.MkdirTemp("/tmp", "webfwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	backendAddr := filepath.Join(dir, "backend.sock")
	backendLn, err := net.Listen("unix", backendAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backendLn.Close() })
	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); c.Close() }(c)
		}
	}()

	// Forwarder over its own unix socket, nil supervisor (backend already up),
	// dialing the stub over unix.
	fwdAddr := filepath.Join(dir, "fwd.sock")
	fwdLn, err := net.Listen("unix", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	fwd := New(fwdLn, backendAddr, nil)
	fwd.dial = func(ctx context.Context, a string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = fwd.Serve(ctx) }()

	// Round-trip bytes through the forward.
	conn, err := net.Dial("unix", fwdAddr)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()
	want := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("echo mismatch: got %q want %q", got, want)
	}
}

func TestDefaultCodeServerArgsDisablesTrustAndWelcome(t *testing.T) {
	args := DefaultCodeServerArgs("127.0.0.1:13337", "/home/dev", "/workspace")
	for _, want := range []string{"--disable-workspace-trust", "--disable-getting-started-override"} {
		if !slices.Contains(args, want) {
			t.Fatalf("args missing %s: %v", want, args)
		}
	}
}

func TestSeedUserSettings(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "webfwd-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	settings := filepath.Join(home, ".local", "share", "code-server", "User", "settings.json")

	// First seed: creates the file with the defaults. uid/gid -1 ⇒ no-op chown,
	// so the test needn't run as root.
	if err := SeedUserSettings(home, -1, -1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(got) != userSettingsJSON {
		t.Fatalf("settings mismatch:\n got %q\nwant %q", got, userSettingsJSON)
	}

	// Second seed must not clobber a user edit.
	const edited = "{ \"workbench.colorTheme\": \"Light+\" }\n"
	if err := os.WriteFile(settings, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedUserSettings(home, -1, -1); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, _ = os.ReadFile(settings)
	if string(got) != edited {
		t.Fatalf("reseed clobbered user edit: got %q", got)
	}
}
