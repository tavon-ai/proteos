package httpapi

import (
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
)

// meResponse is the shape returned by GET /api/me. machines is all of the user's
// machines (possibly empty), seeding the SPA's first paint without a second
// round-trip; the SSE stream then keeps the list live.
type meResponse struct {
	User         meUser           `json:"user"`
	Prefs        userPrefs        `json:"prefs"`
	Machines     []MachineSummary `json:"machines"`
	MachineLimit int              `json:"machine_limit"`
}

type meUser struct {
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// handleLogout delegates to the Auth handler and appends an audit event on
// success. It wraps s.Auth.Logout so the httpapi layer can reach userFromContext
// (the context key is private to this package).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(w, r)
	user, ok := userFromContext(r.Context())
	if !ok {
		return
	}
	uid := uuidString(user.ID)
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionAuthLogout,
		Target: uid,
	})
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionSessionRevoke,
		Target: uid,
	})
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
		Prefs:        prefsView(user),
		Machines:     []MachineSummary{},
		MachineLimit: s.Machines.MaxPerUser(),
	}
	ms, err := s.Machines.List(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if len(ms) > 0 {
		resp.Machines = s.summaries(r.Context(), ms)
	}
	writeJSON(w, http.StatusOK, resp)
}
