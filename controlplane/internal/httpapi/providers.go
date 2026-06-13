package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
)

// maxAPIKeyLen bounds an accepted provider key (defensive; real keys are ~100B).
const maxAPIKeyLen = 8192

// providerView is one row of GET /api/providers. It deliberately exposes no
// secret material — only whether the user has set a key (key_set).
type providerView struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	KeySet      bool   `json:"key_set"`
}

// handleListProviders returns the registry plus the caller's key_set status per
// provider. key_set is computed by reading the user's secret with their scoped
// token and discarding the value (never logged, never echoed).
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)

	list, err := s.Providers.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]providerView, 0, len(list))
	for _, p := range list {
		out = append(out, providerView{
			Key:         p.Key,
			DisplayName: p.DisplayName,
			Enabled:     p.Enabled,
			KeySet:      s.providerKeySet(uid, p.Key),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// providerKeySet reports whether the user has a stored key for the provider. A
// missing secret (ErrNotFound) is the common, non-error "not set" case.
func (s *Server) providerKeySet(uid, key string) bool {
	data, err := s.Secrets.Get(secrets.UserProviderPath(uid, key))
	if err != nil {
		return false
	}
	return len(data) > 0
}

// setProviderKeyRequest is the write-only body of PUT /api/secrets/providers/{key}.
type setProviderKeyRequest struct {
	APIKey string `json:"api_key"`
}

// handleSetProviderKey writes (never echoes) a provider API key to the user's
// secret path after validating the provider is registered + enabled.
func (s *Server) handleSetProviderKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	key := r.PathValue("key")
	prov, err := s.Providers.Get(r.Context(), key)
	if err != nil || !prov.Enabled {
		writeError(w, http.StatusNotFound, "unknown_provider")
		return
	}

	var req setProviderKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" || len(apiKey) > maxAPIKeyLen {
		writeError(w, http.StatusUnprocessableEntity, "invalid_key")
		return
	}

	uid := uuidString(user.ID)
	field := secretFieldFor(prov)
	if err := s.Secrets.Put(secrets.UserProviderPath(uid, key), map[string]string{field: apiKey}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionSecretPut,
		Target: secrets.UserProviderPath(uid, key),
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteProviderKey removes the user's stored key for a provider.
func (s *Server) handleDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	key := r.PathValue("key")
	if _, err := s.Providers.Get(r.Context(), key); err != nil {
		writeError(w, http.StatusNotFound, "unknown_provider")
		return
	}

	uid := uuidString(user.ID)
	if err := s.Secrets.Delete(secrets.UserProviderPath(uid, key)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionSecretDelete,
		Target: secrets.UserProviderPath(uid, key),
	})
	w.WriteHeader(http.StatusNoContent)
}

// secretFieldFor returns the secret field the provider's single API key is
// stored under: the one distinct field referenced by secret_env (claude →
// "api_key"). Falls back to "api_key" if the registry has no mapping.
func secretFieldFor(p providers.Provider) string {
	for _, field := range p.SecretEnv {
		return field
	}
	return "api_key"
}

// uuidString renders a pgtype.UUID as its canonical 36-char form (empty if the
// UUID is not valid).
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	const hexdig = "0123456789abcdef"
	buf := make([]byte, 36)
	pos := 0
	for i := range 16 {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[pos] = '-'
			pos++
		}
		b := u.Bytes[i]
		buf[pos] = hexdig[b>>4]
		buf[pos+1] = hexdig[b&0x0f]
		pos += 2
	}
	return string(buf)
}
