package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tavon/proteos/controlplane/internal/profile"
)

// profileItemView is one row of GET /api/profile/items. It exposes only
// non-secret metadata — never the stored value. `connected` is always true for a
// listed item (a row exists only because a value was set); it is surfaced
// explicitly so the UI does not infer connection state from presence alone.
type profileItemView struct {
	Key       string  `json:"key"`
	Kind      string  `json:"kind"`
	Target    string  `json:"target"`
	Connected bool    `json:"connected"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// handleListProfileItems returns the caller's portable-profile items as metadata
// only. The secret values live exclusively in OpenBao and are never read here.
func (s *Server) handleListProfileItems(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := s.Profile.List(r.Context(), uuidString(user.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]profileItemView, 0, len(items))
	for _, it := range items {
		v := profileItemView{
			Key:       it.Key,
			Kind:      string(it.Kind),
			Target:    it.Target,
			Connected: true,
			CreatedAt: it.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: it.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if it.ExpiresAt != nil {
			s := it.ExpiresAt.UTC().Format(time.RFC3339)
			v.ExpiresAt = &s
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// setProfileItemRequest is the write-only body of PUT /api/profile/items/{key}.
// Only the value is client-supplied; the item's kind/target come from the
// server-side Def registry keyed by the path. The value is stored in OpenBao and
// never echoed.
type setProfileItemRequest struct {
	Value string `json:"value"`
}

// handleSetProfileItem stores (never echoes) a profile item's value. The path
// key must be a registered item type (404 otherwise), which fixes the item's
// kind/target so a client cannot target an arbitrary destination.
func (s *Server) handleSetProfileItem(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	def, ok := profile.Lookup(r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_item")
		return
	}

	var req setProfileItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	value := strings.TrimSpace(req.Value)
	if value == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_value")
		return
	}
	if len(value) > profile.MaxValueLen {
		writeError(w, http.StatusUnprocessableEntity, "value_too_long")
		return
	}

	if err := s.Profile.Set(r.Context(), uuidString(user.ID), def, value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteProfileItem removes a profile item (value + metadata), stopping
// propagation to the user's machines on their next injection.
func (s *Server) handleDeleteProfileItem(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, ok := profile.Lookup(r.PathValue("key")); !ok {
		writeError(w, http.StatusNotFound, "unknown_item")
		return
	}
	if err := s.Profile.Delete(r.Context(), uuidString(user.ID), r.PathValue("key")); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
