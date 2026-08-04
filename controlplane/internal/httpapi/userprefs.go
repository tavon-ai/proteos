package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// userPrefs is the JSON shape of the user's account-level preferences. It is
// embedded in GET /api/me (seeding the Settings UI on load) and returned by
// PATCH /api/user/preferences. download_as_is selects the project-download
// mode: true ⇒ archive the full tree (including .git and ignored files); false
// (default) ⇒ a clean export. claude_attribution selects whether Claude Code
// stamps its attribution on commits/PRs: true (default) keeps Claude Code's own
// defaults; false blanks them on the user's machines (some organizations
// disallow co-authored commits). ssh_enabled is the account-wide inbound-SSH
// master switch: false (default) ⇒ registered login keys are not injected
// anywhere and /gw/ssh refuses; true ⇒ both are allowed.
type userPrefs struct {
	DownloadAsIs      bool `json:"download_as_is"`
	ClaudeAttribution bool `json:"claude_attribution"`
	SSHEnabled        bool `json:"ssh_enabled"`
}

// prefsView builds the wire shape from a user row.
func prefsView(u store.User) userPrefs {
	return userPrefs{
		DownloadAsIs:      u.DownloadAsIs,
		ClaudeAttribution: u.ClaudeAttribution,
		SSHEnabled:        u.SshEnabled,
	}
}

// handleUpdateUserPrefs applies a partial update to the authenticated user's
// account preferences: only the fields present in the body change. It is a
// cookie-authenticated mutation, so the route is CSRF-guarded. The updated
// preference set is returned so the client can reconcile without a refetch.
// A claude_attribution change is re-applied to the user's running machines
// (best-effort, like a git-identity change); stopped machines pick it up on
// their next connect.
func (s *Server) handleUpdateUserPrefs(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		DownloadAsIs      *bool `json:"download_as_is"`
		ClaudeAttribution *bool `json:"claude_attribution"`
		SSHEnabled        *bool `json:"ssh_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}

	updated := user
	if body.DownloadAsIs != nil && *body.DownloadAsIs != user.DownloadAsIs {
		u, err := s.Queries.SetUserDownloadAsIs(r.Context(), store.SetUserDownloadAsIsParams{
			ID:           user.ID,
			DownloadAsIs: *body.DownloadAsIs,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		updated.DownloadAsIs = u.DownloadAsIs
	}
	if body.ClaudeAttribution != nil && *body.ClaudeAttribution != user.ClaudeAttribution {
		u, err := s.Queries.SetUserClaudeAttribution(r.Context(), store.SetUserClaudeAttributionParams{
			ID:                user.ID,
			ClaudeAttribution: *body.ClaudeAttribution,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		updated.ClaudeAttribution = u.ClaudeAttribution
		s.reconfigureGit(r.Context(), uuidString(user.ID))
	}
	if body.SSHEnabled != nil && *body.SSHEnabled != user.SshEnabled {
		u, err := s.Queries.SetUserSSHEnabled(r.Context(), store.SetUserSSHEnabledParams{
			ID:         user.ID,
			SshEnabled: *body.SSHEnabled,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		updated.SshEnabled = u.SshEnabled
		uid := uuidString(user.ID)
		s.Audit.Record(r.Context(), audit.Entry{
			UserID:   uid,
			Actor:    audit.UserActor(uid),
			Action:   audit.ActionSSHAccessSet,
			Target:   uid,
			Metadata: map[string]any{"enabled": u.SshEnabled},
		})
		// Re-inject so the switch takes effect on machines that are already
		// running: turning it on writes ~/.ssh/authorized_keys, turning it off
		// drops the file (sshkeys.ActiveAuthorizedKeys returns empty while off, and
		// an omitted file is removed guest-side). sshd re-reads authorized_keys per
		// connection, so a disable is live with no restart. Machines that are
		// stopped pick the new state up when they next boot.
		s.reinjectRunningMachines(r.Context(), uid)
	}
	writeJSON(w, http.StatusOK, prefsView(updated))
}
