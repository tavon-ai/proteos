package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
)

// secretFieldView is one declared input of a provider, rendered into
// GET /api/providers so the settings UI can build a form from data (Phase 6
// decision #5). Names/labels/env vars are not secret — only values are.
type secretFieldView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Env   string `json:"env"`
}

// providerView is one row of GET /api/providers. It deliberately exposes no
// secret material — only the field metadata and whether the user has set a key.
type providerView struct {
	Key          string            `json:"key"`
	DisplayName  string            `json:"display_name"`
	Enabled      bool              `json:"enabled"`
	KeySet       bool              `json:"key_set"`
	SecretFields []secretFieldView `json:"secret_fields"`
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
		fields := make([]secretFieldView, 0, len(p.SecretFields))
		for _, f := range p.SecretFields {
			fields = append(fields, secretFieldView{Name: f.Name, Label: f.Label, Env: f.Env})
		}
		out = append(out, providerView{
			Key:          p.Key,
			DisplayName:  p.DisplayName,
			Enabled:      p.Enabled,
			KeySet:       s.providerKeySet(uid, p.Key),
			SecretFields: fields,
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
// Fields maps each declared field name to its value; the values are stored under
// the user's provider secret path and never echoed.
type setProviderKeyRequest struct {
	Fields map[string]string `json:"fields"`
}

// handleSetProviderKey writes (never echoes) a provider's secret fields to the
// user's secret path after validating the provider is registered + enabled and
// the supplied fields match its declared schema.
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
	if err := prov.Validate(req.Fields); err != nil {
		writeError(w, http.StatusUnprocessableEntity, fieldErrorCode(err))
		return
	}

	// Store trimmed values keyed by field name (the injector reads them by name).
	stored := make(map[string]string, len(req.Fields))
	for _, f := range prov.SecretFields {
		stored[f.Name] = strings.TrimSpace(req.Fields[f.Name])
	}

	uid := uuidString(user.ID)
	if err := s.Secrets.Put(secrets.UserProviderPath(uid, key), stored); err != nil {
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

// fieldErrorCode maps a providers validation error to a stable API error code.
func fieldErrorCode(err error) string {
	switch {
	case errors.Is(err, providers.ErrUnknownField):
		return "unknown_field"
	case errors.Is(err, providers.ErrMissingField):
		return "missing_field"
	case errors.Is(err, providers.ErrFieldTooLong):
		return "field_too_long"
	default:
		return "invalid_fields"
	}
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
