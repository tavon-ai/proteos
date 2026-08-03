package httpapi

import (
	"context"
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// handleGatewaySSH authenticates (via requireAuth), resolves and authorizes the
// target machine, checks it is running, then hands the upgrade to the SSH
// gateway proxy. Mirrors handleGatewayTerminal's resolution/serve split, with
// one difference: the Origin check is only enforced when the request actually
// carries an Origin header. The CLI's bearer-token ProxyCommand path (the
// primary way this route is reached — see docs/plans/ssh-access.md §3.4) never
// sends one; it has no ambient browser credential for a hostile page to replay,
// so the forgery this check defends against does not apply (same reasoning as
// csrfHeader's bearer-token exemption for POST/DELETE mutations).
func (s *Server) handleGatewaySSH(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID, _ := sessionIDFromContext(r.Context())

	if r.Header.Get("Origin") != "" && !s.SSHGateway.AllowsOrigin(r) {
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
	s.SSHGateway.Serve(w, r, gateway.SSHServeOpts{
		MachineID: machineID,
		SessionID: sessionID,
		Refresh: func(ctx context.Context) (bool, error) {
			mm, err := s.Machines.GetByID(ctx, m.ID)
			if err != nil {
				return false, err
			}
			return machine.State(mm.State) == machine.StateRunning, nil
		},
	})
}
