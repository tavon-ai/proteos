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
	// GitHubConnected gates the SPA (TAV-149): login is Zitadel, but git
	// operations need a linked GitHub account, so the UI blocks on the
	// Connect GitHub screen until this is true.
	GitHubConnected bool `json:"github_connected"`
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

// handleGitHubConnect starts the Connect GitHub OAuth flow (TAV-149). It sits
// behind requireAuth so only a logged-in user can begin linking.
func (s *Server) handleGitHubConnect(w http.ResponseWriter, r *http.Request) {
	s.Auth.GitHubConnect(w, r)
}

// handleGitHubCallback completes Connect GitHub for the authenticated user.
// requireAuth resolved the session cookie (the callback is a top-level GET, so
// SameSite=Lax sends it); the auth handler needs the user to record the link.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.Auth.GitHubCallback(w, r, user)
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
		Prefs:           prefsView(user),
		Machines:        []MachineSummary{},
		MachineLimit:    s.Machines.MaxPerUser(),
		GitHubConnected: user.GithubUserID != nil,
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
