package httpapi

import (
	"errors"
	"net/http"

	"github.com/tavon/proteos/controlplane/internal/machine"
)

// meResponse is the shape returned by GET /api/me. The machine summary is the
// user's machine, or null if they have none.
type meResponse struct {
	User    meUser          `json:"user"`
	Machine *MachineSummary `json:"machine"`
}

type meUser struct {
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// handleMe returns the authenticated user and their machine summary (or null).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	resp := meResponse{
		User: meUser{
			Login:     user.Login,
			Email:     user.Email,
			AvatarURL: user.AvatarUrl,
		},
	}
	m, err := s.Machines.Get(r.Context(), user.ID)
	switch {
	case err == nil:
		summary := toSummary(m)
		resp.Machine = &summary
	case errors.Is(err, machine.ErrNoMachine):
		// leave Machine nil
	default:
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
