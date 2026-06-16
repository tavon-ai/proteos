package term

import (
	"bytes"
	"testing"
	"time"
)

func testManager() *Manager {
	return NewManager(Config{Shell: "/bin/bash", ScrollbackKiB: 256})
}

// readUntil drains an attachment's live output until it contains want or the
// deadline passes, returning everything seen.
func readUntil(t *testing.T, a *Attachment, want []byte, d time.Duration) []byte {
	t.Helper()
	var acc []byte
	acc = append(acc, a.Replay...)
	if bytes.Contains(acc, want) {
		return acc
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case chunk, ok := <-a.Out():
			if !ok {
				t.Fatalf("attachment detached (lagged) before seeing %q; got %q", want, acc)
			}
			acc = append(acc, chunk...)
			if bytes.Contains(acc, want) {
				return acc
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for %q; got %q", want, acc)
		}
	}
}

func TestSessionEcho(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Attach()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Detach()

	if err := s.Write([]byte("echo proteos-echo-marker\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, a, []byte("proteos-echo-marker"), 5*time.Second)
}

func TestDetachedOutputCaptured(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}

	// Attach then immediately detach so no client is attached when the command
	// below runs — its output must still land in the scrollback ring.
	a1, err := s.Attach()
	if err != nil {
		t.Fatal(err)
	}
	a1.Detach()

	if err := s.Write([]byte("echo detached-marker-xyz\n")); err != nil {
		t.Fatal(err)
	}

	// Reattach (with retries) and assert the replay holds output produced while
	// nobody was attached.
	deadline := time.Now().Add(5 * time.Second)
	for {
		a2, err := s.Attach()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(a2.Replay, []byte("detached-marker-xyz")) {
			a2.Detach()
			return
		}
		a2.Detach()
		if time.Now().After(deadline) {
			t.Fatalf("scrollback ring did not capture detached output")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSessionResize(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Attach()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Detach()

	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	// `stty size` prints "rows cols"; after a 40x120 resize that is "40 120".
	if err := s.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, a, []byte("40 120"), 5*time.Second)
}

func TestSessionExitDetection(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(); err != nil {
		t.Fatal(err)
	}

	if err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shell exit not detected")
	}
	if got := s.ExitCode(); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}

	// Writing/attaching after exit is rejected.
	if err := s.Write([]byte("x")); err != ErrSessionExited {
		t.Fatalf("Write after exit = %v, want ErrSessionExited", err)
	}
	if _, err := s.Attach(); err != ErrSessionExited {
		t.Fatalf("Attach after exit = %v, want ErrSessionExited", err)
	}
}

func TestConcurrentAttaches(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}
	a1, err := s.Attach()
	if err != nil {
		t.Fatal(err)
	}
	defer a1.Detach()
	a2, err := s.Attach()
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Detach()

	if err := s.Write([]byte("echo fanout-marker\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, a1, []byte("fanout-marker"), 5*time.Second)
	readUntil(t, a2, []byte("fanout-marker"), 5*time.Second)
}

func TestManagerRespawnsAfterExit(t *testing.T) {
	m := testManager()
	defer m.Shutdown()

	s1, err := m.Get("main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Attach(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	<-s1.Done()

	// Give the auto-remove goroutine a moment, then Get must return a fresh one.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s2, err := m.Get("main", "")
		if err != nil {
			t.Fatal(err)
		}
		if s2 != s1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Manager did not respawn session after exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
