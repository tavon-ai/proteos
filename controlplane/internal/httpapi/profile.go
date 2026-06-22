package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/profile"
)

// profileItemView is one row of GET /api/profile/items. It exposes only
// non-secret metadata — never the stored value. `connected` is always true for a
// listed item (a row exists only because a value was set); it is surfaced
// explicitly so the UI does not infer connection state from presence alone.
// `needs_reconnect` is true when the item is known-expired from its metadata
// (e.g. a Claude setup-token past its ~1-year TTL) — the UI shows a reconnect
// prompt rather than silently failing on the machine.
type profileItemView struct {
	Key            string  `json:"key"`
	Kind           string  `json:"kind"`
	Target         string  `json:"target"`
	Connected      bool    `json:"connected"`
	NeedsReconnect bool    `json:"needs_reconnect"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
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
	now := time.Now()
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
			v.NeedsReconnect = it.ExpiresAt.Before(now)
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

	uid := uuidString(user.ID)
	if err := s.Profile.Set(r.Context(), uid, def, value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	// Re-inject to already-running machines so the new token takes effect without
	// recreating a machine (newly created ones pick it up on their boot injection).
	s.reinjectRunningMachines(r.Context(), uid)
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
	uid := uuidString(user.ID)
	if err := s.Profile.Delete(r.Context(), uid, r.PathValue("key")); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	// Re-inject (replace-all) to running machines so the now-removed item drops
	// from their env on the next push, not only on their next boot.
	s.reinjectRunningMachines(r.Context(), uid)
	w.WriteHeader(http.StatusNoContent)
}

// reinjectRunningMachines pushes the user's current composed secret/profile set
// to every running machine they own. Best-effort and async (InjectAsync has its
// own bounded retry): a profile change must not block on, or fail because of,
// guest availability. A machine only ever receives its owner's secrets, so this
// stays owner-scoped by construction. No-op when the injector or store is unwired.
func (s *Server) reinjectRunningMachines(ctx context.Context, userID string) {
	if s.Injector == nil || s.Queries == nil {
		return
	}
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		slog.Warn("profile: bad user id for re-injection", "err", err)
		return
	}
	ids, err := s.Queries.ListRunningMachineIDsByUserID(ctx, uid)
	if err != nil {
		slog.Warn("profile: list running machines for re-injection", "err", err)
		return
	}
	for _, id := range ids {
		s.Injector.InjectAsync(userID, uuidString(id))
	}
}
