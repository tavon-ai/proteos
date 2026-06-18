package httpapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// handleGatewayTerminal authenticates (via requireAuth), resolves and authorizes
// the target machine, checks it is running, then hands the upgrade to the
// gateway proxy. Resolution is kept here (it owns the machine service) while the
// proxy owns the Origin check, upgrade, tunnel dial, and relay — the separable
// steps Phase 8 will recombine for subdomain auth + code-server targets.
func (s *Server) handleGatewayTerminal(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID, _ := sessionIDFromContext(r.Context())

	// Origin first (decision #8): reject cross-origin upgrades before touching
	// the machine, so a bad Origin never leaks machine existence via 404/409.
	if !s.Gateway.AllowsOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_forbidden")
		return
	}

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		// A foreign or absent machine both surface as 404 (no existence leak).
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	machineID := machine.UUIDString(m.ID)

	// Working directory (Phase 9 decision #3): validate ?cwd= against the
	// machine's listable projects before forwarding it to the guest handshake.
	cwd, errCode := s.resolveSessionCwd(r.Context(), machineID, r.URL.Query().Get(guestwire.QueryParamCwd))
	if errCode != "" {
		writeError(w, cwdErrorStatus(errCode), errCode)
		return
	}

	s.Gateway.Serve(w, r, gateway.ServeOpts{
		MachineID: machineID,
		SessionID: sessionID,
		Session:   r.URL.Query().Get(guestwire.QueryParamSession),
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

// resolveTerminalMachine returns the machine the user may attach to. An empty
// ?machine= resolves to the user's machine only when they own exactly one
// (ErrAmbiguous otherwise, so a multi-machine caller must name an id); a provided
// id must exist and be owned by the user (otherwise ErrNoMachine, which the
// caller maps to 404 to avoid leaking whether the id exists).
func (s *Server) resolveTerminalMachine(ctx context.Context, user store.User, machineParam string) (store.Machine, error) {
	if machineParam == "" {
		return s.Machines.OnlyMachine(ctx, user.ID)
	}
	id, err := machine.ParseUUID(machineParam)
	if err != nil || !id.Valid {
		return store.Machine{}, machine.ErrNoMachine
	}
	m, err := s.Machines.GetByID(ctx, id)
	if err != nil {
		return store.Machine{}, err
	}
	if !uuidEqual(m.UserID, user.ID) {
		return store.Machine{}, machine.ErrNoMachine
	}
	return m, nil
}

// uuidEqual reports whether two pgtype.UUIDs are both valid and identical.
func uuidEqual(a, b pgtype.UUID) bool {
	return a.Valid && b.Valid && a.Bytes == b.Bytes
}
