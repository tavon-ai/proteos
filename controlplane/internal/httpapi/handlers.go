package httpapi

import (
	"net/http"
)

// meResponse is the shape returned by GET /api/me. machines is all of the user's
// machines (possibly empty), seeding the SPA's first paint without a second
// round-trip; the SSE stream then keeps the list live.
type meResponse struct {
	User     meUser           `json:"user"`
	Machines []MachineSummary `json:"machines"`
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
		Machines: []MachineSummary{},
	}
	ms, err := s.Machines.List(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, m := range ms {
		resp.Machines = append(resp.Machines, s.summary(r.Context(), m))
	}
	writeJSON(w, http.StatusOK, resp)
}
