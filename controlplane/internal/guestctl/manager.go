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

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
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

// GuestDialer opens the opaque byte tunnel to a machine's guest agent at the
// given guest port. *nodeclient.Client satisfies it; the control channel rides
// the terminal port (agentapi.GuestTerminalPort).
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error)
}

// Manager maintains one control channel per running machine and serves guest →
// CP requests through a single authorization choke point.
type Manager struct {
	dialer  GuestDialer
	broker  *machine.Broker
	q       *store.Queries
	tokens  *github.TokenSource
	audit   *audit.Recorder
	tasks   *taskevents.Hub // AT2 live agent-event fan-out (may be nil)
	gitHost string

	baseCtx context.Context

	mu          sync.Mutex
	supervisors map[string]context.CancelFunc // machineID → per-machine supervisor cancel
	conns       map[string]*conn              // machineID → live connection (nil while redialing)
}

// New wires a Manager. gitHost is the only host credentials are minted for and
// the only host clones target (config PROTEOS_GIT_HOST, default github.com).
// tasks is the AT2 agent-event fan-out the headless run streams into (nil ⇒ live
// events are simply not relayed; agent.done still records the final result).
func New(dialer GuestDialer, broker *machine.Broker, q *store.Queries, tokens *github.TokenSource, rec *audit.Recorder, tasks *taskevents.Hub, gitHost string) *Manager {
	if gitHost == "" {
		gitHost = "github.com"
	}
	return &Manager{
		dialer:      dialer,
		broker:      broker,
		q:           q,
		tokens:      tokens,
		audit:       rec,
		tasks:       tasks,
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

	ch, cancel := m.broker.SubscribeAll()
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
	tunnel, err := m.dialer.DialGuest(dctx, machineID, agentapi.GuestTerminalPort)
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
	// Push git identity + helper wiring and the Claude preferences on every
	// (re)connect, concurrently with the read loop — the configure ops are
	// request/response, so they need c.run to be reading. Idempotent; a configure
	// failure does not drop the channel (credentials still work).
	go func() {
		if cfgErr := m.configure(ctx, c, userID); cfgErr != nil {
			slog.Warn("guestctl: configure failed", "machine", machineID, "err", cfgErr)
		}
	}()
	return true, c.run(ctx)
}

// configure sends git.configure with the owner's GitHub identity, then
// claude.configure with the owner's Claude Code preferences (attribution).
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
	// A portable-profile git identity (Phase 4) overrides the GitHub-derived
	// default. This keeps git.configure the single writer of ~/.gitconfig — the
	// profile only changes the identity it writes, so the two never fight.
	if gi, err := m.q.GetGitIdentity(ctx, uid); err == nil {
		if gi.Name != "" {
			name = gi.Name
		}
		if gi.Email != "" {
			email = gi.Email
		}
	}
	rctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	if _, err = c.request(rctx, guestwire.OpGitConfigure, guestwire.GitConfigurePayload{
		Name:   name,
		Email:  email,
		Helper: guestwire.HelperBinPath,
	}); err != nil {
		return err
	}

	cctx, ccancel := context.WithTimeout(ctx, configTimeout)
	defer ccancel()
	_, err = c.request(cctx, guestwire.OpClaudeConfigure, guestwire.ClaudeConfigurePayload{
		Attribution: u.ClaudeAttribution,
	})
	return err
}

// ReconfigureUser re-pushes git.configure + claude.configure to every running
// machine the user owns that currently has a live control channel, so a portable
// git-identity or Claude-preference change takes effect without recreating the
// machine (Phase 4). Best-effort: machines without a channel pick the new
// settings up on their next (re)connect. A failure for one machine does not stop
// the others.
func (m *Manager) ReconfigureUser(ctx context.Context, userID string) {
	uid, err := machine.ParseUUID(userID)
	if err != nil {
		slog.Warn("guestctl: reconfigure bad user id", "err", err)
		return
	}
	ids, err := m.q.ListRunningMachineIDsByUserID(ctx, uid)
	if err != nil {
		slog.Warn("guestctl: reconfigure list machines", "err", err)
		return
	}
	for _, id := range ids {
		mid := machine.UUIDString(id)
		c := m.getConn(mid)
		if c == nil {
			continue
		}
		if err := m.configure(ctx, c, userID); err != nil {
			slog.Warn("guestctl: reconfigure failed", "machine", mid, "err", err)
		}
	}
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
		case guestwire.OpGitPushDone:
			m.handlePushDone(ctx, machineID, payload)
			return nil, nil
		case guestwire.OpAgentEvent:
			m.handleAgentEvent(payload)
			return nil, nil
		case guestwire.OpAgentDone:
			m.handleAgentDone(ctx, payload)
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

// handlePushDone records a push completion as a machine_events row (type
// git.push) and publishes it so the SSE stream delivers it to the dashboard.
// Mirrors handleCloneDone.
func (m *Manager) handlePushDone(ctx context.Context, machineID string, payload json.RawMessage) {
	var done guestwire.GitPushDonePayload
	if err := json.Unmarshal(payload, &done); err != nil {
		slog.Warn("guestctl: bad push.done payload", "machine", machineID, "err", err)
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
		Type:      audit.ActionGitPush, // "git.push"
		Actor:     "system:guestctl",
		Payload:   body,
	})
	if err != nil {
		slog.Error("guestctl: record push event", "machine", machineID, "err", err)
		return
	}
	mc, err := m.q.GetMachineByID(ctx, mid)
	if err != nil {
		slog.Error("guestctl: load machine for push event", "machine", machineID, "err", err)
		return
	}
	m.broker.Publish(machine.Update{Machine: mc, Event: ev})
	slog.Info("guestctl: push done", "machine", machineID, "op_id", done.OpID, "ok", done.OK)
}

// handleAgentEvent fans one normalized agent.event (AT2) out to the task's live
// SSE stream. The payload is already normalized + bounded by the guest and
// carries no secret, so the CP relays it verbatim (minus the task id, which the
// stream is keyed by). Best-effort — a malformed payload is dropped.
func (m *Manager) handleAgentEvent(payload json.RawMessage) {
	if m.tasks == nil {
		return
	}
	var ev guestwire.AgentEventPayload
	if err := json.Unmarshal(payload, &ev); err != nil || ev.TaskID == "" {
		slog.Warn("guestctl: bad agent.event payload", "err", err)
		return
	}
	taskID := ev.TaskID
	ev.TaskID = "" // the SSE stream is task-scoped; don't echo the id in every frame
	if m.tasks.Publish(taskID, mustJSON(ev), false) {
		slog.Info("guestctl: agent event history truncated", "task", taskID)
	}
}

// handleAgentDone records a headless agent-run completion (AT1) onto its
// agent_tasks row: terminal status, the agent's session id (for resume), usage,
// the result summary, and any sanitized error. It then publishes the terminal
// `result` frame onto the task's live stream (AT2) so subscribers close cleanly.
// Best-effort — a malformed payload or DB error is logged, not propagated (the
// guest does not retry).
func (m *Manager) handleAgentDone(ctx context.Context, payload json.RawMessage) {
	var done guestwire.AgentDonePayload
	if err := json.Unmarshal(payload, &done); err != nil {
		slog.Warn("guestctl: bad agent.done payload", "err", err)
		return
	}
	if done.TaskID == "" {
		slog.Warn("guestctl: agent.done with empty task id")
		return
	}
	status := "done"
	switch {
	case done.Canceled:
		status = "canceled"
	case !done.OK:
		status = "failed"
	}

	// Durable record (best effort). A bad id or DB blip must not stop the live
	// stream from closing — the result frame below is independent of this.
	if id, err := machine.ParseUUID(done.TaskID); err != nil {
		slog.Warn("guestctl: bad agent.done task id", "task", done.TaskID, "err", err)
	} else {
		usage, _ := json.Marshal(map[string]any{
			"cost_usd":    done.CostUSD,
			"num_turns":   done.NumTurns,
			"duration_ms": done.DurationMS,
		})
		if err := m.q.FinishAgentTask(ctx, store.FinishAgentTaskParams{
			ID:             id,
			Status:         status,
			AgentSessionID: done.SessionID,
			Usage:          usage,
			ResultSummary:  done.Summary,
			Error:          done.Error,
		}); err != nil {
			slog.Error("guestctl: finish agent task", "task", done.TaskID, "err", err)
		}
	}

	// Close the live stream with the single terminal result frame (always).
	m.publishTaskResult(done, status)
	slog.Info("guestctl: agent task done", "task", done.TaskID, "status", status)
}

// publishTaskResult emits the single terminal `result` frame for a finished run
// onto the task's live stream (AT2). It is the only source of the result kind —
// the guest deliberately does not relay the stream's result line — so a live
// subscriber sees exactly one result and then the stream closes.
func (m *Manager) publishTaskResult(done guestwire.AgentDonePayload, status string) {
	if m.tasks == nil {
		return
	}
	result := map[string]any{
		"kind":        guestwire.AgentEventResult,
		"status":      status,
		"is_error":    !done.OK,
		"text":        done.Summary,
		"cost_usd":    done.CostUSD,
		"num_turns":   done.NumTurns,
		"duration_ms": done.DurationMS,
	}
	if done.Error != "" {
		result["error"] = done.Error
	}
	m.tasks.Publish(done.TaskID, mustJSON(result), true)
}

// RunAgent dispatches a headless agent run (AT1) and returns once the guest acks
// (the run is asynchronous; completion arrives as agent.done → agent_tasks
// update). ErrNoChannel if the machine has no live channel.
func (m *Manager) RunAgent(ctx context.Context, machineID, taskID, repoPath, prompt, provider, sessionID string) error {
	c := m.getConn(machineID)
	if c == nil {
		return ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.request(rctx, guestwire.OpAgentRun, guestwire.AgentRunPayload{
		TaskID: taskID, Path: repoPath, Prompt: prompt, Provider: provider, SessionID: sessionID,
	})
	return err
}

// CancelAgent signals a running task to stop (AT3) and returns once the guest
// acks. The cancel is idempotent on the guest; the terminated run reports
// agent.done(Canceled) which flips the task to canceled. ErrNoChannel if the
// machine has no live channel.
func (m *Manager) CancelAgent(ctx context.Context, machineID, taskID string) error {
	c := m.getConn(machineID)
	if c == nil {
		return ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.request(rctx, guestwire.OpAgentCancel, guestwire.AgentCancelPayload{TaskID: taskID})
	return err
}

// Push sends a git.push over the machine's channel and returns once the guest
// acks (the push runs asynchronously; completion arrives as a git.push.done →
// machine_events row). ErrNoChannel if the machine has no live channel.
func (m *Manager) Push(ctx context.Context, machineID, repoPath, branch string, setUpstream bool, opID string) error {
	c := m.getConn(machineID)
	if c == nil {
		return ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.request(rctx, guestwire.OpGitPush, guestwire.GitPushPayload{
		Path: repoPath, Branch: branch, SetUpstream: setUpstream, OpID: opID,
	})
	return err
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

// ListProjects asks the guest to scan its workspace and return the cloned repos
// (Phase 9 decision #4). The disk is the source of truth; the CP holds no
// project table. ErrNoChannel if the machine has no live channel.
func (m *Manager) ListProjects(ctx context.Context, machineID string) ([]guestwire.Project, error) {
	c := m.getConn(machineID)
	if c == nil {
		return nil, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpProjectsList, struct{}{})
	if err != nil {
		return nil, err
	}
	var resp guestwire.ProjectsListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// GitStatus reads a project's working-tree change set over the channel (GR1).
// repoPath is the absolute repo path the CP resolved from a listable project.
// ErrNoChannel if the machine has no live channel.
func (m *Manager) GitStatus(ctx context.Context, machineID, repoPath string) (guestwire.GitStatusResponse, error) {
	c := m.getConn(machineID)
	if c == nil {
		return guestwire.GitStatusResponse{}, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpGitStatus, guestwire.GitStatusPayload{Path: repoPath})
	if err != nil {
		return guestwire.GitStatusResponse{}, err
	}
	var resp guestwire.GitStatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return guestwire.GitStatusResponse{}, err
	}
	return resp, nil
}

// GitDiff reads a project's unified diff over the channel (GR1). staged selects
// the index diff over the worktree diff. ErrNoChannel if the machine has no live
// channel.
func (m *Manager) GitDiff(ctx context.Context, machineID, repoPath string, staged bool) (guestwire.GitDiffResponse, error) {
	c := m.getConn(machineID)
	if c == nil {
		return guestwire.GitDiffResponse{}, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpGitDiff, guestwire.GitDiffPayload{Path: repoPath, Staged: staged})
	if err != nil {
		return guestwire.GitDiffResponse{}, err
	}
	var resp guestwire.GitDiffResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return guestwire.GitDiffResponse{}, err
	}
	return resp, nil
}

// GitBranch creates (and optionally checks out) a branch in a project over the
// channel (GR2). On failure the error is a *ControlError whose Code the HTTP
// layer maps (branch_exists, invalid_branch, git_failed). ErrNoChannel if the
// machine has no live channel.
func (m *Manager) GitBranch(ctx context.Context, machineID, repoPath, name string, checkout bool, from string) (guestwire.GitBranchResponse, error) {
	c := m.getConn(machineID)
	if c == nil {
		return guestwire.GitBranchResponse{}, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpGitBranch, guestwire.GitBranchPayload{
		Path: repoPath, Name: name, Checkout: checkout, From: from,
	})
	if err != nil {
		return guestwire.GitBranchResponse{}, err
	}
	var resp guestwire.GitBranchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return guestwire.GitBranchResponse{}, err
	}
	return resp, nil
}

// GitCommit stages the requested paths (or all changes) and commits them in a
// project over the channel (GR3). On failure the error is a *ControlError whose
// Code the HTTP layer maps (empty_message, nothing_to_commit, git_failed).
// ErrNoChannel if the machine has no live channel.
func (m *Manager) GitCommit(ctx context.Context, machineID, repoPath, message string, paths []string) (guestwire.GitCommitResponse, error) {
	c := m.getConn(machineID)
	if c == nil {
		return guestwire.GitCommitResponse{}, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpGitCommit, guestwire.GitCommitPayload{
		Path: repoPath, Message: message, Paths: paths,
	})
	if err != nil {
		return guestwire.GitCommitResponse{}, err
	}
	var resp guestwire.GitCommitResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return guestwire.GitCommitResponse{}, err
	}
	return resp, nil
}

// KVGet reads a value from the machine's SQLite kv table over the channel
// (Phase 9 decision #6). A nil return with nil error means the key is unset.
// ErrNoChannel if the machine has no live channel.
func (m *Manager) KVGet(ctx context.Context, machineID, key string) (*string, error) {
	c := m.getConn(machineID)
	if c == nil {
		return nil, ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := c.request(rctx, guestwire.OpKVGet, guestwire.KVGetPayload{Key: key})
	if err != nil {
		return nil, err
	}
	var resp guestwire.KVGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Value, nil
}

// KVSet writes a value into the machine's SQLite kv table over the channel. On a
// diskless stack the guest acks but does not persist (the desktop's debounced
// save must not error). ErrNoChannel if the machine has no live channel.
func (m *Manager) KVSet(ctx context.Context, machineID, key, value string) error {
	c := m.getConn(machineID)
	if c == nil {
		return ErrNoChannel
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.request(rctx, guestwire.OpKVSet, guestwire.KVSetPayload{Key: key, Value: value})
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
