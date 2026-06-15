// Package term owns the guest's PTY sessions. A Session is a long-lived shell
// running on a pseudo-terminal whose output is (a) appended to a bounded
// scrollback ring at all times — attached or not — and (b) fanned out to every
// currently attached client; input from any attached client is merged onto the
// PTY. Sessions outlive the WebSocket connections that attach to them (the
// tmux-like property), so a dropped-and-reconnected browser sees the same shell
// with intact scrollback. Manager is the named registry of live Sessions.
package term

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// subChanSize bounds how many output chunks a single attachment may buffer
// before it is considered a laggard and detached (its WebSocket is then closed;
// the client reconnects and replays from the ring). It keeps one slow consumer
// from stalling the PTY reader or other attachments.
const subChanSize = 256

// ErrSessionExited is returned by Write/Resize after the shell has exited.
var ErrSessionExited = errors.New("term: session has exited")

// subscriber is one attached client's output queue. The PTY reader does a
// non-blocking send to ch under the session lock; a full ch means the consumer
// fell behind, so the reader closes ch (signalling the consumer to stop) and
// drops the subscriber.
type subscriber struct {
	ch chan []byte
}

// Attachment is a live attachment to a Session. Replay is the scrollback
// snapshot taken atomically at attach time (write it before reading Out). Out
// delivers live output chunks; it is closed if this attachment is dropped for
// lagging. Detach removes the attachment; call it once when done.
type Attachment struct {
	Replay []byte
	sub    *subscriber
	sess   *Session
	once   sync.Once
}

// Out is the live-output channel. A receive of (_, false) means this attachment
// was detached (lagged) and the caller should stop.
func (a *Attachment) Out() <-chan []byte { return a.sub.ch }

// Detach unsubscribes this attachment. Idempotent.
func (a *Attachment) Detach() {
	a.once.Do(func() { a.sess.detach(a.sub) })
}

// Session is a shell on a PTY with scrollback and output fan-out.
type Session struct {
	name string
	ptmx *os.File
	cmd  *exec.Cmd

	mu       sync.Mutex
	ring     *ring
	subs     map[*subscriber]struct{}
	exited   bool
	exitCode int

	done chan struct{} // closed when the shell exits and reader has drained
}

// Config is the shape of a Session to spawn.
type Config struct {
	Name          string
	Shell         string // executable, run as `<shell> -l`
	ScrollbackKiB int    // ring size in KiB
	Env           []string

	// Command, when non-empty, is the argv run instead of the login shell — used
	// for agent sessions, which spawn a provider's injected launch command
	// (Phase 5 decision #9) rather than `<shell> -l`. Env still applies as an
	// overlay so the command sees its provider's secret environment.
	Command []string

	// Credential, when non-nil, runs the session process under this uid/gid (the
	// unprivileged `dev` user) instead of inheriting the agent's root identity
	// (Phase 8). Nil ⇒ run as the agent (root).
	Credential *syscall.Credential

	// Dir, when non-empty, is the working directory the session starts in. Used
	// to drop the unprivileged session into its own $HOME (which it can write)
	// rather than the agent's cwd.
	Dir string
}

// newSession spawns the shell (or, for agent sessions, the configured command)
// on a fresh PTY and starts the reader goroutine.
func newSession(cfg Config) (*Session, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	scroll := cfg.ScrollbackKiB
	if scroll < 1 {
		scroll = 256
	}

	var cmd *exec.Cmd
	if len(cfg.Command) > 0 {
		cmd = exec.Command(cfg.Command[0], cfg.Command[1:]...)
	} else {
		cmd = exec.Command(shell, "-l")
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if len(cfg.Env) > 0 {
		cmd.Env = append(cmd.Env, cfg.Env...)
	}
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	// Drop to the unprivileged session user when configured. pty.Start fills in
	// Setsid/Setctty on this same SysProcAttr, so pre-setting Credential here is
	// compatible with PTY allocation.
	if cfg.Credential != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cfg.Credential}
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	s := &Session{
		name: cfg.Name,
		ptmx: ptmx,
		cmd:  cmd,
		ring: newRing(scroll * 1024),
		subs: make(map[*subscriber]struct{}),
		done: make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// readLoop drains the PTY into the ring and fans output out to subscribers
// until the shell exits (read error/EOF), then reaps the child and closes done.
func (s *Session) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.fanout(data)
		}
		if err != nil {
			break
		}
	}
	s.finish()
}

// fanout appends chunk to the ring and delivers it to every subscriber,
// dropping any whose queue is full.
func (s *Session) fanout(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring.Write(chunk)
	for sub := range s.subs {
		select {
		case sub.ch <- chunk:
		default:
			// Laggard: signal the consumer to stop and drop it.
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
}

// finish reaps the child, records the exit code, and tears down subscribers.
func (s *Session) finish() {
	err := s.cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	_ = s.ptmx.Close()

	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	s.mu.Unlock()
	// Deliberately do NOT close subscriber channels here: a closed channel must
	// unambiguously mean "this attachment lagged" (see fanout). Attached
	// consumers learn about exit by selecting on Done instead, after which they
	// detach themselves.
	close(s.done)
}

// Attach atomically snapshots the scrollback ring and registers a subscriber,
// so no output is lost or duplicated between the snapshot and the live stream.
// Attaching to an already-exited session returns ErrSessionExited.
func (s *Session) Attach() (*Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return nil, ErrSessionExited
	}
	sub := &subscriber{ch: make(chan []byte, subChanSize)}
	s.subs[sub] = struct{}{}
	return &Attachment{
		Replay: s.ring.snapshot(),
		sub:    sub,
		sess:   s,
	}, nil
}

// detach removes a subscriber if still present.
func (s *Session) detach(sub *subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.ch)
	}
}

// Write merges client input onto the PTY. Returns ErrSessionExited after exit.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return ErrSessionExited
	}
	_, err := s.ptmx.Write(p)
	return err
}

// Resize applies new terminal dimensions to the PTY.
func (s *Session) Resize(cols, rows int) error {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return ErrSessionExited
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Done is closed when the shell has exited and output has been fully drained.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitCode returns the shell's exit code; valid only after Done is closed.
func (s *Session) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// Name is the session's registry name.
func (s *Session) Name() string { return s.name }

// close terminates the shell (used by Manager shutdown / tests).
func (s *Session) close() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.done
}
