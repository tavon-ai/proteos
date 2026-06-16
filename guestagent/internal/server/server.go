// Package server is the guest agent's HTTP/WebSocket front end. It exposes a
// single route, GET /terminal, that upgrades to the terminal WebSocket protocol
// (guestwire) and bridges the connection to a term.Session.
//
// Trust boundary (Phase 3 decision #10): this listener is NOT browser-facing.
// It is reached only over the per-VM private transport (vsock inside the VM /
// a 0600 unix socket in dev), which the node-agent alone can connect to. There
// is therefore no Origin check and no app-layer credential here this phase.
// Per-machine identity (OpenBao) is STILL unminted after Phase 7 (decision #2):
// it would authenticate guest-*initiated* calls, but Phase 7's control channel
// (GET /control, added below) is CP-*dialed*, and even the git credential
// request rides that CP-dialed channel guest→CP — so no guest-initiated
// transport exists to authenticate. Revisit when one appears (Phase 11 cross-
// host routing). The WebSocket Origin check that protects the browser leg lives
// in the control-plane gateway.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/term"
)

// pingInterval is how often each leg sends a WebSocket ping to keep the
// connection (and any NAT/idle timers along the tunnel) alive.
const pingInterval = 30 * time.Second

// readLimit bounds a single inbound WebSocket message. Terminal input is tiny;
// a large bound only matters for big pastes.
const readLimit = 1 << 20

// Persister is the persistence surface the server exposes over /resume and
// /info (Phase 4). Implemented by *persist.Persist; nil in tests/builds that
// run without persistence, in which case those routes report 503.
type Persister interface {
	// Resume applies the host clock + entropy after a snapshot restore and
	// returns the corrected skew in milliseconds.
	Resume(unixNanos int64, entropy []byte) (int64, error)
	// Info returns the current persistence/boot info.
	Info() guestwire.Info
}

// SecretStore is the provider-injection surface the server exposes over
// PUT /secrets (Phase 5). Implemented by *secrets.Store; nil in tests/builds
// that run without injection, in which case PUT /secrets reports 503 and agent
// sessions close with CloseProviderUnavailable.
type SecretStore interface {
	// Replace installs providers as the complete injected set.
	Replace(providers map[string]guestwire.ProviderDef) error
	// Get returns the injected definition for a provider key.
	Get(key string) (guestwire.ProviderDef, bool)
	// EnvList returns a provider's environment as KEY=VALUE pairs.
	EnvList(key string) ([]string, bool)
	// AwaitReady blocks (bounded by ctx) until a provider's setup_command has
	// settled, so the launch path sees a deterministic Degraded outcome.
	AwaitReady(ctx context.Context, key string)
	// Degraded reports whether a provider's setup_command failed on the current
	// push (Phase 6), making it unlaunchable until a successful re-push.
	Degraded(key string) bool
}

// Controller serves the Phase 7 control channel (GET /control): the CP-dialed
// bidirectional WebSocket carrying git.configure/git.clone/git.credential.
// Implemented by *ctlchan.Manager; nil disables the route.
type Controller interface {
	HandleControl(w http.ResponseWriter, r *http.Request)
}

// Server bridges terminal WebSockets to PTY sessions managed by mgr, and serves
// the Phase 4 control surface (/resume, /info) backed by persist, the Phase 5
// secret-injection surface (/secrets) backed by sec, and the Phase 7 control
// channel (/control) backed by control.
type Server struct {
	mgr     *term.Manager
	persist Persister
	sec     SecretStore
	control Controller

	// workspaceRoot is the directory tree a session's cwd must point into. Empty
	// ⇒ guestwire.WorkspaceRoot (production). Tests set it to a temp dir to
	// exercise cwd plumbing without a real /workspace.
	workspaceRoot string
}

// New returns a Server backed by mgr and (optionally) persist + sec + control.
// A nil persist disables /resume and /info (503); a nil sec disables /secrets
// (503) and makes every agent session close with CloseProviderUnavailable; a nil
// control disables /control (404).
func New(mgr *term.Manager, persist Persister, sec SecretStore, control Controller) *Server {
	return &Server{mgr: mgr, persist: persist, sec: sec, control: control}
}

// Handler builds the HTTP handler. /terminal serves sessions; /resume + /info
// are the Phase 4 control surface; /secrets is the Phase 5 injection surface;
// /healthz is a trivial liveness probe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /terminal", s.handleTerminal)
	mux.HandleFunc(guestwire.RouteResume, s.handleResume)
	mux.HandleFunc(guestwire.RouteInfo, s.handleInfo)
	mux.HandleFunc(guestwire.RouteSecrets, s.handleSecrets)
	if s.control != nil {
		mux.HandleFunc(guestwire.RouteControl, s.control.HandleControl)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleSecrets installs the pushed provider set (replace-all). The body's
// secret values are never logged.
func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if s.sec == nil {
		http.Error(w, "secret injection disabled", http.StatusServiceUnavailable)
		return
	}
	var req guestwire.SecretsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.sec.Replace(req.Providers); err != nil {
		slog.Error("install secrets failed", "err", err)
		http.Error(w, "install failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResume applies the host-provided clock + entropy after a snapshot
// restore (decision #9) and returns the corrected skew.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if s.persist == nil {
		http.Error(w, "persistence disabled", http.StatusServiceUnavailable)
		return
	}
	var req guestwire.ResumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var entropy []byte
	if req.EntropyB64 != "" {
		b, err := base64.StdEncoding.DecodeString(req.EntropyB64)
		if err != nil {
			http.Error(w, "bad entropy_b64", http.StatusBadRequest)
			return
		}
		entropy = b
	}
	skewMS, err := s.persist.Resume(req.UnixNanos, entropy)
	if err != nil {
		slog.Error("resume failed", "err", err)
		http.Error(w, "resume failed", http.StatusInternalServerError)
		return
	}
	writeJSONResp(w, guestwire.ResumeResponse{SkewCorrectedMS: skewMS})
}

// handleInfo returns the guest agent version, persistence mode, and last boot.
func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	if s.persist == nil {
		http.Error(w, "persistence disabled", http.StatusServiceUnavailable)
		return
	}
	writeJSONResp(w, s.persist.Info())
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json failed", "err", err)
	}
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionName := q.Get(guestwire.QueryParamSession)
	if sessionName == "" {
		sessionName = guestwire.DefaultSession
	}
	if !guestwire.ValidSessionName(sessionName) {
		http.Error(w, "invalid session name", http.StatusBadRequest)
		return
	}

	// Working directory (Phase 9 decision #3). Absent ⇒ the session user $HOME
	// (existing behavior). Present ⇒ canonicalize, require the /workspace prefix,
	// and require an existing directory. The guest is not a trust boundary (it
	// acts with the owner's authority), but this is cheap defence in depth atop
	// the control plane's listable-project check.
	cwd, err := s.resolveCwd(q.Get(guestwire.QueryParamCwd))
	if err != nil {
		http.Error(w, "invalid cwd", http.StatusBadRequest)
		return
	}
	provider := q.Get(guestwire.QueryParamProvider)

	// Not browser-facing (see package doc): skip Origin verification.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		slog.Warn("terminal: ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(readLimit)

	if err := s.serve(r.Context(), c, sessionName, provider, cwd); err != nil {
		slog.Debug("terminal: session ended", "session", sessionName, "err", err)
	}
}

// resolveCwd validates a requested working directory from the handshake. An
// empty request yields an empty result (⇒ the manager's default dir). A
// non-empty request must clean to a path under WorkspaceRoot (guestwire.
// CleanWorkdir) and name an existing directory on the guest disk; otherwise it
// is an error the caller maps to 400.
func (s *Server) resolveCwd(req string) (string, error) {
	if req == "" {
		return "", nil
	}
	root := s.workspaceRoot
	if root == "" {
		root = guestwire.WorkspaceRoot
	}
	clean, ok := guestwire.CleanWorkdirUnder(req, root)
	if !ok {
		return "", errors.New("cwd outside workspace")
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", errors.New("cwd is not an existing directory")
	}
	return clean, nil
}

// errProviderUnavailable is returned by sessionFor when an agent session names a
// provider that has not been injected (or has no launch command). It maps to a
// CloseProviderUnavailable WebSocket close with CloseReasonNotInjected.
var errProviderUnavailable = errors.New("provider unavailable")

// errSetupFailed is returned by sessionFor when an agent session names a
// provider whose setup_command failed on the current push (Phase 6): it is
// injected but degraded. It maps to the same close code with the more specific
// CloseReasonSetupFailed reason so the browser can show an actionable message.
var errSetupFailed = errors.New("provider setup failed")

// sessionFor resolves a session name to a live Session, starting it in dir (the
// validated working directory; empty ⇒ the manager default). The session is an
// agent session when provider is non-empty (Phase 9 decision #3) or — for
// backward compatibility — when the opaque name still carries the legacy
// "agent-<provider>" prefix; otherwise it is a plain login shell. An agent
// session for an un-injected provider returns errProviderUnavailable; for a
// degraded (setup-failed) provider it returns errSetupFailed. For providers with
// a setup_command it first waits for that command to settle, so the launch
// decision sees a deterministic outcome rather than racing the async setup.
func (s *Server) sessionFor(ctx context.Context, name, provider, dir string) (*term.Session, error) {
	key := provider
	if key == "" {
		// Legacy encoding: provider key in the session name (pre-Phase-9 clients).
		if k, isAgent := guestwire.ProviderKeyFromSession(name); isAgent {
			key = k
		}
	}
	if key == "" {
		return s.mgr.Get(name, dir)
	}
	if s.sec == nil {
		return nil, errProviderUnavailable
	}
	def, ok := s.sec.Get(key)
	if !ok {
		return nil, errProviderUnavailable
	}
	s.sec.AwaitReady(ctx, key)
	if s.sec.Degraded(key) {
		return nil, errSetupFailed
	}
	command := strings.Fields(def.Command)
	if len(command) == 0 {
		return nil, errProviderUnavailable
	}
	env, _ := s.sec.EnvList(key)
	return s.mgr.GetAgent(name, command, env, dir)
}

// serve runs one attachment: hello → scrollback replay → live I/O until the
// shell exits, the client disconnects, or an error occurs.
func (s *Server) serve(ctx context.Context, c *websocket.Conn, sessionName, provider, cwd string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, err := s.sessionFor(ctx, sessionName, provider, cwd)
	if err != nil {
		switch {
		case errors.Is(err, errSetupFailed):
			c.Close(websocket.StatusCode(guestwire.CloseProviderUnavailable), guestwire.CloseReasonSetupFailed)
			return err
		case errors.Is(err, errProviderUnavailable):
			c.Close(websocket.StatusCode(guestwire.CloseProviderUnavailable), guestwire.CloseReasonNotInjected)
			return err
		default:
			c.Close(websocket.StatusInternalError, "spawn failed")
			return err
		}
	}
	att, err := sess.Attach()
	if err != nil {
		// Shell already exited between Get and Attach; let the client retry.
		c.Close(websocket.StatusInternalError, "session exited")
		return err
	}
	defer att.Detach()

	// hello, then the scrollback replay as one binary message.
	hello := guestwire.Frame{Type: guestwire.FrameHello, Session: sessionName, ReplayBytes: len(att.Replay)}
	if err := writeJSON(ctx, c, hello); err != nil {
		return err
	}
	if len(att.Replay) > 0 {
		if err := c.Write(ctx, websocket.MessageBinary, att.Replay); err != nil {
			return err
		}
	}

	go s.readPump(ctx, cancel, c, sess)
	go pingLoop(ctx, c)

	return s.writePump(ctx, c, sess, att)
}

// readPump reads client→guest messages: binary frames are PTY input, text
// frames are JSON control messages (resize). It cancels the context when the
// client disconnects so the write side tears down.
func (s *Server) readPump(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, sess *term.Session) {
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if err := sess.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var f guestwire.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue // ignore malformed control frames
			}
			if f.Type == guestwire.FrameResize && f.Cols > 0 && f.Rows > 0 {
				_ = sess.Resize(f.Cols, f.Rows)
			}
		}
	}
}

// writePump streams live PTY output to the client and handles shell exit. It
// returns when the client disconnects (ctx cancelled), the attachment lags
// (Out closed), or the shell exits (exit frame + normal close).
func (s *Server) writePump(ctx context.Context, c *websocket.Conn, sess *term.Session, att *term.Attachment) error {
	out := att.Out()
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "")
			return ctx.Err()

		case chunk, ok := <-out:
			if !ok {
				// Detached for lagging: drop the connection so the client
				// reconnects and replays from the ring.
				c.Close(websocket.StatusInternalError, "output overflow")
				return errors.New("attachment lagged")
			}
			if err := c.Write(ctx, websocket.MessageBinary, chunk); err != nil {
				return err
			}

		case <-sess.Done():
			// Flush any output buffered before exit, then announce the code.
			drain(ctx, c, out)
			code := sess.ExitCode()
			_ = writeJSON(ctx, c, guestwire.Frame{Type: guestwire.FrameExit, ExitCode: &code})
			c.Close(websocket.StatusNormalClosure, "")
			return nil
		}
	}
}

// drain writes any output still queued on a closed/closing session, without
// blocking once the queue is empty.
func drain(ctx context.Context, c *websocket.Conn, out <-chan []byte) {
	for {
		select {
		case chunk, ok := <-out:
			if !ok {
				return
			}
			if err := c.Write(ctx, websocket.MessageBinary, chunk); err != nil {
				return
			}
		default:
			return
		}
	}
}

// pingLoop sends WebSocket pings every pingInterval until the context ends.
func pingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Ping(ctx); err != nil {
				return
			}
		}
	}
}

// writeJSON marshals f and writes it as a single text frame.
func writeJSON(ctx context.Context, c *websocket.Conn, f guestwire.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
