package httpapi

import "net/http"

// meResponse is the shape returned by GET /api/me. The machine summary is
// hardcoded null this phase; the field exists so Phase 2 fills it in without a
// contract change on the client.
type meResponse struct {
	User    meUser `json:"user"`
	Machine *any   `json:"machine"`
}

type meUser struct {
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// handleMe returns the authenticated user and (for now) a null machine.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, meResponse{
		User: meUser{
			Login:     user.Login,
			Email:     user.Email,
			AvatarURL: user.AvatarUrl,
		},
		Machine: nil,
	})
}

// handleGetMachine reports that the user has no machine yet. Phase 2 replaces
// this with a real lookup.
func (s *Server) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no_machine")
}

// handleNotImplemented is the placeholder for machine mutations until Phase 2.
func (s *Server) handleNotImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented")
}
