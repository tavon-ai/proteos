package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// createTokenRequest is the body of POST /api/tokens. ExpiresInDays == 0 mints a
// token that never expires.
type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

// createdTokenResponse is the 201 body of POST /api/tokens. The plaintext token
// appears here and nowhere else — it cannot be recovered later.
type createdTokenResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token"`
	Prefix    string `json:"prefix"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// tokenView is one PAT in the GET listing — never the secret or its hash.
type tokenView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

type tokensResponse struct {
	Tokens []tokenView `json:"tokens"`
}

// handleCreateToken mints a personal access token for the caller. The plaintext
// is returned exactly once.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.ExpiresInDays < 0 {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var expiresIn time.Duration
	if req.ExpiresInDays > 0 {
		expiresIn = time.Duration(req.ExpiresInDays) * 24 * time.Hour
	}

	created, err := s.PATs.Create(r.Context(), user.ID, req.Name, expiresIn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	id := machine.UUIDString(created.Row.ID)

	uid := uuidString(user.ID)
	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionTokenCreate,
		Target:   id,
		Metadata: map[string]any{"name": created.Row.Name, "prefix": created.Row.Prefix},
	})

	writeJSON(w, http.StatusCreated, createdTokenResponse{
		ID:        id,
		Name:      created.Row.Name,
		Token:     created.Plaintext,
		Prefix:    created.Row.Prefix,
		ExpiresAt: tsString(created.Row.ExpiresAt),
	})
}

// handleListTokens returns the caller's live tokens, newest first — never the
// secret or its hash.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rows, err := s.PATs.List(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed")
		return
	}
	views := make([]tokenView, 0, len(rows))
	for _, t := range rows {
		views = append(views, tokenView{
			ID:         machine.UUIDString(t.ID),
			Name:       t.Name,
			Prefix:     t.Prefix,
			CreatedAt:  tsString(t.CreatedAt),
			LastUsedAt: tsString(t.LastUsedAt),
			ExpiresAt:  tsString(t.ExpiresAt),
		})
	}
	writeJSON(w, http.StatusOK, tokensResponse{Tokens: views})
}

// handleRevokeToken revokes one of the caller's tokens. Revoking an unknown or
// not-owned token is a 404 (never reveals another user's tokens).
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tid, err := machine.ParseUUID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_token")
		return
	}
	revoked, err := s.PATs.Revoke(r.Context(), user.ID, tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !revoked {
		writeError(w, http.StatusNotFound, "no_token")
		return
	}

	uid := uuidString(user.ID)
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionTokenRevoke,
		Target: machine.UUIDString(tid),
	})
	w.WriteHeader(http.StatusNoContent)
}
