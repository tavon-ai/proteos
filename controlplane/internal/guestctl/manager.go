package guestctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// ErrNoChannel means the machine has no live control channel (it is not running,
// or the channel has not been (re)established yet). The HTTP layer maps it to
// 409 machine_not_running.
var ErrNoChannel = errors.New("guestctl: no control channel for machine")

// Tunings.
const (
	dialTimeout    = 20 * time.Second
	maxBackoff     = 30 * time.Second
	configTimeout  = 10 * time.Second
	requestTimeout = 30 * time.Second
)

// GuestDialer opens the opaque byte tunnel to a machine's guest agent.
// *nodeclient.Client satisfies it.
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string) (net.Conn, error)
}

// Manager maintains one control channel per running machine and serves guest →
// CP requests through a single authorization choke point.
type Manager struct {
	dialer  GuestDialer
	broker  *machine.Broker
	q       *store.Queries
	tokens  *github.TokenSource
	audit   *audit.Recorder
	gitHost string

	baseCtx context.Context

	mu          sync.Mutex
	supervisors map[string]context.CancelFunc // machineID → per-machine supervisor cancel
	conns       map[string]*conn              // machineID → live connection (nil while redialing)
}

// New wires a Manager. gitHost is the only host credentials are minted for and
// the only host clones target (config PROTEOS_GIT_HOST, default github.com).
func New(dialer GuestDialer, broker *machine.Broker, q *store.Queries, tokens *github.TokenSource, rec *audit.Recorder, gitHost string) *Manager {
	if gitHost == "" {
		gitHost = "github.com"
	}
	return &Manager{
		dialer:      dialer,
		broker:      broker,
		q:           q,
		tokens:      tokens,
		audit:       rec,
		gitHost:     gitHost,
		supervisors: map[string]context.CancelFunc{},
		conns:       map[string]*conn{},
	}
}

// Run watches machine state and maintains the per-machine channels until ctx is
// cancelled. It first connects to machines already running (e.g. after a CP
// restart), then reacts to broker transitions: → running connects, → stop/
// hibernate/stopped/error tears down. A resume (stopped → starting → running)
// re-establishes the channel as a normal reconnect.
func (m *Manager) Run(ctx context.Context) {
	m.baseCtx = ctx

	if running, err := m.q.ListMachinesInStates(ctx, []string{string(machine.StateRunning)}); err != nil {
		slog.Error("guestctl: list running machines", "err", err)
	} else {
		for _, mc := range running {
			m.ensure(mc)
		}
	}

	ch, cancel := m.broker.Subscribe()
	defer cancel()
	slog.Info("guestctl manager started")
	for {
		select {
		case <-ctx.Done():
			slog.Info("guestctl manager stopped")
			return
		case u := <-ch:
			switch machine.State(u.Machine.State) {
			case machine.StateRunning:
				m.ensure(u.Machine)
			case machine.StateStopping, machine.StateHibernating, machine.StateStopped, machine.StateError:
				m.teardown(machine.UUIDString(u.Machine.ID))
			}
		}
	}
}

// ensure starts a supervisor goroutine for a running machine if one is not
// already running.
func (m *Manager) ensure(mc store.Machine) {
	id := machine.UUIDString(mc.ID)
	userID := machine.UUIDString(mc.UserID)

	m.mu.Lock()
	if _, ok := m.supervisors[id]; ok {
		m.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(m.baseCtx)
	m.supervisors[id] = cancel
	m.mu.Unlock()

	go func() {
		m.supervise(cctx, id, userID)
		m.mu.Lock()
		delete(m.supervisors, id)
		m.mu.Unlock()
	}()
}

// teardown cancels a machine's supervisor (and thus its live channel).
func (m *Manager) teardown(id string) {
	m.mu.Lock()
	cancel := m.supervisors[id]
	delete(m.supervisors, id)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// supervise maintains one channel with backoff reconnect until ctx is cancelled.
func (m *Manager) supervise(ctx context.Context, machineID, userID string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		established, err := m.connectOnce(ctx, machineID, userID)
		if ctx.Err() != nil {
			return
		}
		if established {
			backoff = time.Second // a real session ran; reset backoff
			slog.Info("guestctl: channel ended, will reconnect", "machine", machineID, "err", err)
		} else {
			slog.Debug("guestctl: dial failed, backing off", "machine", machineID, "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// connectOnce dials the guest, runs one channel session, and returns whether a
// session was established (so the supervisor can reset its backoff).
func (m *Manager) connectOnce(ctx context.Context, machineID, userID string) (established bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	tunnel, err := m.dialer.DialGuest(dctx, machineID)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial guest: %w", err)
	}

	ws, err := dialControlWS(ctx, tunnel)
	if err != nil {
		_ = tunnel.Close()
		return false, fmt.Errorf("dial /control: %w", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	c := newConn(ws, m.makeHandler(machineID, userID))
	m.setConn(machineID, c)
	defer m.clearConn(machineID, c)

	slog.Info("guestctl: channel up", "machine", machineID)
	// Push git identity + helper wiring on every (re)connect, concurrently with
	// the read loop — git.configure is a request/response, so it needs c.run to be
	// reading. Idempotent; a configure failure does not drop the channel
	// (credentials still work).
	go func() {
		if cfgErr := m.configure(ctx, c, userID); cfgErr != nil {
			slog.Warn("guestctl: git.configure failed", "machine", machineID, "err", cfgErr)
		}
	}()
	return true, c.run(ctx)
}

// configure sends git.configure with the owner's GitHub identity.
func (m *Manager) configure(ctx context.Context, c *conn, userID string) error {
	uid, err := machine.ParseUUID(userID)
	if err != nil {
		return err
	}
	u, err := m.q.GetUserByID(ctx, uid)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	name := u.Login
	email := u.Email
	if email == "" {
		email = fmt.Sprintf("%s@users.noreply.github.com", u.Login)
	}
	rctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	_, err = c.request(rctx, guestwire.OpGitConfigure, guestwire.GitConfigurePayload{
		Name:   name,
		Email:  email,
		Helper: guestwire.HelperBinPath,
	})
	return err
}

// makeHandler returns the guest → CP request handler bound to this machine's
// owner. The bound machineID/userID — derived from the dial, never the payload —
// is the authorization context (decision #3).
func (m *Manager) makeHandler(machineID, userID string) reqHandler {
	return func(ctx context.Context, op string, payload json.RawMessage) (json.RawMessage, *guestwire.ControlErrorPayload) {
		switch op {
		case guestwire.OpGitCredential:
			return m.handleCredential(ctx, userID, payload)
		case guestwire.OpGitCloneDone:
			m.handleCloneDone(ctx, machineID, payload)
			return nil, nil
		default:
			slog.Warn("guestctl: unknown op from guest", "machine", machineID, "op", op)
			return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "unknown op"}
		}
	}
}

// handleCredential is the authorization choke point for git.credential: validate
// the host/protocol, resolve the owner's token, audit, and return the credential.
func (m *Manager) handleCredential(ctx context.Context, userID string, payload json.RawMessage) (json.RawMessage, *guestwire.ControlErrorPayload) {
	var req guestwire.GitCredentialRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "bad payload"}
	}
	if req.Host != m.gitHost || req.Protocol != "https" {
		slog.Warn("guestctl: credential refused for host", "host", req.Host, "protocol", req.Protocol)
		return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeForbiddenHost}
	}

	cred, err := m.tokens.Token(ctx, userID)
	if errors.Is(err, github.ErrReconnectGitHub) {
		return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeReconnectGitHub}
	}
	if err != nil {
		slog.Error("guestctl: token resolve failed", "err", err)
		return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable}
	}

	m.audit.Record(ctx, audit.Entry{
		UserID: userID,
		Actor:  audit.UserActor(userID),
		Action: audit.ActionGitCredential,
		Target: req.Host,
	})

	resp := guestwire.GitCredentialResponse{
		Username: "x-access-token",
		Password: cred.AccessToken,
	}
	if !cred.Expiry.IsZero() {
		resp.Expiry = cred.Expiry.UTC().Format(time.RFC3339)
	}
	return mustJSON(resp), nil
}

// handleCloneDone records a clone completion as a machine_events row (type
// git.clone) and publishes it so the SSE stream delivers it to the dashboard.
func (m *Manager) handleCloneDone(ctx context.Context, machineID string, payload json.RawMessage) {
	var done guestwire.GitCloneDonePayload
	if err := json.Unmarshal(payload, &done); err != nil {
		slog.Warn("guestctl: bad clone.done payload", "machine", machineID, "err", err)
		return
	}
	mid, err := machine.ParseUUID(machineID)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"op_id":  done.OpID,
		"ok":     done.OK,
		"detail": done.Detail,
	})
	ev, err := m.q.InsertMachineEvent(ctx, store.InsertMachineEventParams{
		MachineID: mid,
		Type:      audit.ActionGitClone, // "git.clone"
		Actor:     "system:guestctl",
		Payload:   body,
	})
	if err != nil {
		slog.Error("guestctl: record clone event", "machine", machineID, "err", err)
		return
	}
	mc, err := m.q.GetMachineByID(ctx, mid)
	if err != nil {
		slog.Error("guestctl: load machine for clone event", "machine", machineID, "err", err)
		return
	}
	m.broker.Publish(machine.Update{Machine: mc, Event: ev})
	slog.Info("guestctl: clone done", "machine", machineID, "op_id", done.OpID, "ok", done.OK)
}

// Clone sends a git.clone over the machine's channel and returns once the guest
// acks (the clone runs asynchronously; completion arrives as a git.clone.done →
// machine_events row). ErrNoChannel if the machine has no live channel.
func (m *Manager) Clone(ctx context.Context, machineID, url, dest, opID string) error {
	c := m.getConn(machineID)
	if c == nil {
		return ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.request(rctx, guestwire.OpGitClone, guestwire.GitClonePayload{URL: url, Dest: dest, OpID: opID})
	return err
}

// HasChannel reports whether a live channel exists for the machine.
func (m *Manager) HasChannel(machineID string) bool { return m.getConn(machineID) != nil }

func (m *Manager) setConn(id string, c *conn) {
	m.mu.Lock()
	m.conns[id] = c
	m.mu.Unlock()
}

func (m *Manager) clearConn(id string, c *conn) {
	m.mu.Lock()
	if m.conns[id] == c {
		delete(m.conns, id)
	}
	m.mu.Unlock()
	c.fail()
}

func (m *Manager) getConn(id string) *conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[id]
}
