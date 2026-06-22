package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// userPrefs is the JSON shape of the user's account-level preferences. It is
// embedded in GET /api/me (seeding the Settings UI on load) and returned by
// PATCH /api/user/preferences. Today the sole preference is the project-download
// mode: download_as_is true ⇒ archive the full tree (including .git and ignored
// files); false (default) ⇒ a clean export.
type userPrefs struct {
	DownloadAsIs bool `json:"download_as_is"`
}

// handleUpdateUserPrefs applies a partial update to the authenticated user's
// account preferences: only the fields present in the body change. It is a
// cookie-authenticated mutation, so the route is CSRF-guarded. The updated
// preference set is returned so the client can reconcile without a refetch.
func (s *Server) handleUpdateUserPrefs(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		DownloadAsIs *bool `json:"download_as_is"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}

	downloadAsIs := user.DownloadAsIs
	if body.DownloadAsIs != nil {
		downloadAsIs = *body.DownloadAsIs
	}

	updated, err := s.Queries.SetUserDownloadAsIs(r.Context(), store.SetUserDownloadAsIsParams{
		ID:           user.ID,
		DownloadAsIs: downloadAsIs,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, userPrefs{DownloadAsIs: updated.DownloadAsIs})
}
