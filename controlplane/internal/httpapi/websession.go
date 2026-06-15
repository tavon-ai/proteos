package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/session"
)

// handleWebSession mints the one-shot, ≤60s web-session URL the SPA navigates the
// editor iframe to (Phase 8 decision #2). It runs on the MAIN origin behind
// requireAuth + the CSRF header; the minted token carries {machine_id, user_id,
// session_id} so the machine-web origin can set its subdomain cookie bound to
// this parent session. Resolution mirrors the terminal gateway: an empty
// ?machine= is the user's single machine; a provided id must be owned.
func (s *Server) handleWebSession(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID, _ := sessionIDFromContext(r.Context())

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	machineID := machine.UUIDString(m.ID)
	url := s.MachineWeb.MintWebSessionURL(externalScheme(r), machineID, sessionID, machine.UUIDString(user.ID))
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// externalScheme resolves the browser-facing scheme of the main origin, honoring
// the proxy layer's X-Forwarded-Proto (NPMplus / app-stack nginx terminate TLS).
func externalScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- gateway adapters -------------------------------------------------------
//
// The gateway machine-web handler depends on small string-typed interfaces so it
// stays decoupled from store/pgtype; the Server (which owns the session + machine
// services) supplies them.

// sessionOwnerAdapter satisfies gateway.SessionResolver via the session manager.
type sessionOwnerAdapter struct{ sessions *session.Manager }

func (a sessionOwnerAdapter) SessionOwner(ctx context.Context, sessionID string) (string, bool, error) {
	id, err := machine.ParseUUID(sessionID)
	if err != nil || !id.Valid {
		return "", false, nil
	}
	user, err := a.sessions.AliveByID(ctx, id)
	if err != nil {
		if errors.Is(err, session.ErrInvalidSession) {
			return "", false, nil
		}
		return "", false, err
	}
	return machine.UUIDString(user.ID), true, nil
}

// machineOwnerAdapter satisfies gateway.MachineResolver via the machine service.
type machineOwnerAdapter struct{ machines *machine.Service }

func (a machineOwnerAdapter) MachineOwner(ctx context.Context, machineID string) (string, bool, bool, error) {
	id, err := machine.ParseUUID(machineID)
	if err != nil || !id.Valid {
		return "", false, false, nil
	}
	m, err := a.machines.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, machine.ErrNoMachine) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	running := machine.State(m.State) == machine.StateRunning
	return machine.UUIDString(m.UserID), running, true, nil
}

// MachineWebResolvers builds the gateway resolver adapters from the session +
// machine services. Exported so main.go can wire NewMachineWeb without poking at
// the unexported adapter types.
func MachineWebResolvers(sessions *session.Manager, machines *machine.Service) (gateway.SessionResolver, gateway.MachineResolver) {
	return sessionOwnerAdapter{sessions: sessions}, machineOwnerAdapter{machines: machines}
}
