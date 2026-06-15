package webfwd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Supervisor lazily starts and supervises a single local backend process
// (code-server) bound to a loopback address (Phase 8 decision #5). It is the
// "start on the first web-port connection, health-wait before forwarding,
// restart on crash with backoff" half of the web forward; the forward calls
// Ensure before every dial.
//
// Why lazy: ~200 MB of editor stays out of RAM for terminal-only sessions
// (2 GiB VMs), and Phase 11's idle logic gets one less always-on process.
//
// A nil *Supervisor is valid and means "no supervision" — Ensure is a no-op, so
// the forward simply assumes the backend is already up (dev/e2e with a stub).
type Supervisor struct {
	bin  string
	args []string
	env  []string
	dir  string
	cred *syscall.Credential
	addr string // host:port the process binds; the health probe target

	startTimeout time.Duration
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	dialBackend  func(ctx context.Context, addr string) (net.Conn, error)

	mu      sync.Mutex
	cmd     *exec.Cmd
	healthy bool
	fails   int       // consecutive start/health failures (drives backoff)
	nextTry time.Time // earliest next start attempt (crash-loop guard)
}

// SupervisorConfig is the immutable setup for a Supervisor.
type SupervisorConfig struct {
	Bin  string              // code-server binary (required)
	Args []string            // full argument vector
	Env  []string            // process environment (HOME/PATH/...)
	Dir  string              // working directory (empty ⇒ inherit)
	Cred *syscall.Credential // drop to this uid/gid (nil ⇒ same as the agent)
	Addr string              // loopback host:port to health-probe (required)
}

// NewSupervisor builds a Supervisor. Bin/Addr empty ⇒ nil (no supervision).
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.Bin == "" || cfg.Addr == "" {
		return nil
	}
	return &Supervisor{
		bin:          cfg.Bin,
		args:         cfg.Args,
		env:          cfg.Env,
		dir:          cfg.Dir,
		cred:         cfg.Cred,
		addr:         cfg.Addr,
		startTimeout: 30 * time.Second,
		baseBackoff:  500 * time.Millisecond,
		maxBackoff:   15 * time.Second,
		dialBackend:  defaultDial,
	}
}

func defaultDial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// Ensure guarantees code-server is running and accepting connections, starting
// it on first use and restarting it after a crash (rate-limited by backoff). It
// is safe for concurrent callers: startup is serialized, so a burst of web
// connections triggers exactly one start. A nil receiver is a no-op (the
// unsupervised dev/e2e path).
func (s *Supervisor) Ensure(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.alive() && s.healthy {
		return nil
	}
	if !s.alive() {
		if wait := time.Until(s.nextTry); wait > 0 {
			// Crash-loop guard: don't hammer a binary that keeps dying.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err := s.start(); err != nil {
			s.fail()
			return fmt.Errorf("start code-server: %w", err)
		}
	}
	if err := s.waitHealthy(ctx); err != nil {
		s.fail()
		return err
	}
	s.healthy = true
	s.fails = 0
	return nil
}

// alive reports whether the supervised process is currently running. The reaper
// goroutine nils s.cmd when the process exits, so a non-nil cmd means alive.
func (s *Supervisor) alive() bool { return s.cmd != nil }

// start launches the process and spawns a reaper that marks it dead on exit.
// Caller holds s.mu.
func (s *Supervisor) start() error {
	cmd := exec.Command(s.bin, s.args...)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if s.cred != nil {
		cmd.SysProcAttr.Credential = s.cred
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	s.healthy = false
	slog.Info("webfwd: started code-server", "pid", cmd.Process.Pid, "addr", s.addr)

	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		// Only clear if this is still the current process (not already replaced).
		if s.cmd == cmd {
			s.cmd = nil
			s.healthy = false
		}
		s.mu.Unlock()
		slog.Warn("webfwd: code-server exited", "pid", cmd.Process.Pid)
	}()
	return nil
}

// fail records a failed start/health attempt and schedules the next allowed
// attempt with exponential backoff. Caller holds s.mu.
func (s *Supervisor) fail() {
	s.fails++
	d := s.baseBackoff << min(s.fails-1, 16)
	if d > s.maxBackoff || d <= 0 {
		d = s.maxBackoff
	}
	s.nextTry = time.Now().Add(d)
}

// waitHealthy polls the backend address until it accepts a connection or the
// timeout/ctx fires. Caller holds s.mu (startup is serialized).
func (s *Supervisor) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(s.startTimeout)
	for {
		// Bail early if the process died during startup.
		if !s.alive() {
			return errors.New("code-server exited during startup")
		}
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := s.dialBackend(dctx, s.addr)
		cancel()
		if err == nil {
			conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("code-server not healthy on %s after %s", s.addr, s.startTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Shutdown stops the supervised process (best-effort), used on agent shutdown.
func (s *Supervisor) Shutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}
