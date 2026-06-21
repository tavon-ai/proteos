package httpapi

import (
	"context"
	"net/http"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/machine"
)

// handleGatewayAgent serves WS /gw/agent/{provider}: it reuses the terminal
// gateway chain (auth → Origin → ownership → running) plus provider checks
// (registered+enabled else 404, key_set else 409), does an idempotent secret
// push, records an agent.launch audit row, then dials the guest agent session
// "agent-<key>" — which spawns the provider's injected launch command. The
// browser only ever names the provider key; the registry decides the rest.
func (s *Server) handleGatewayAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID, _ := sessionIDFromContext(r.Context())

	// Origin first (decision #8): reject cross-origin upgrades before any lookup.
	if !s.Gateway.AllowsOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_forbidden")
		return
	}

	providerKey := r.PathValue("provider")
	prov, err := s.Providers.Get(r.Context(), providerKey)
	if err != nil || !prov.Enabled {
		writeError(w, http.StatusNotFound, "unknown_provider")
		return
	}

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	// The user must have a stored key for this provider (pre-upgrade 409), unless
	// the provider can run on subscription creds in the image (Claude Code).
	uid := uuidString(user.ID)
	if !prov.AllowsSubscriptionAuth() && !s.providerKeySet(uid, providerKey) {
		writeError(w, http.StatusConflict, "no_provider_key")
		return
	}

	machineID := machine.UUIDString(m.ID)

	// Working directory (Phase 9 decision #3): validate ?cwd= against the
	// machine's listable projects so the agent CLI launches in the repo folder.
	cwd, errCode := s.resolveSessionCwd(r.Context(), machineID, r.URL.Query().Get(guestwire.QueryParamCwd))
	if errCode != "" {
		writeError(w, cwdErrorStatus(errCode), errCode)
		return
	}

	// Idempotent push before launch, so the guest has the latest key even if the
	// poller's start-time injection has not run (or failed) yet. Synchronous here
	// because the agent session about to spawn depends on it; a failure aborts the
	// upgrade with an internal error rather than spawning an unauthenticated CLI.
	if s.Injector != nil {
		if err := s.Injector.Inject(r.Context(), uid, machineID); err != nil {
			writeError(w, http.StatusBadGateway, "injection_failed")
			return
		}
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionAgentLaunch,
		Target: providerKey,
	})

	// The session name is now an opaque per-window id (decision #3); the provider
	// travels as its own handshake parameter rather than being encoded into the
	// session name. An absent ?session= falls back to the legacy "agent-<key>"
	// name so a pre-Phase-9 client still reconnects to its session.
	session := r.URL.Query().Get(guestwire.QueryParamSession)
	if session == "" {
		session = agentSessionName(providerKey)
	}

	s.Gateway.Serve(w, r, gateway.ServeOpts{
		MachineID: machineID,
		SessionID: sessionID,
		Session:   session,
		Provider:  providerKey,
		Cwd:       cwd,
		Refresh: func(ctx context.Context) (bool, error) {
			mm, err := s.Machines.GetByID(ctx, m.ID)
			if err != nil {
				return false, err
			}
			return machine.State(mm.State) == machine.StateRunning, nil
		},
	})
}

// agentSessionName builds the guest session name for a provider's agent session.
func agentSessionName(providerKey string) string {
	return guestwire.AgentSessionPrefix + providerKey
}

// Injector is the secret-push surface the agent route needs (satisfied by
// *injector.Injector). Defined as an interface so the route stays testable.
type Injector interface {
	Inject(ctx context.Context, userID, machineID string) error
}
