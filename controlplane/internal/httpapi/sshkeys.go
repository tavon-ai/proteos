package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/sshkeys"
)

// sshLoginKeyView is one row of GET/POST /api/ssh-keys: metadata only — the
// public key is not secret, but the private half never appears here (it never
// even reaches the server; see sshkeys.Store's doc).
type sshLoginKeyView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

func sshLoginKeyViewFrom(k sshkeys.Key) sshLoginKeyView {
	return sshLoginKeyView{
		ID:          k.ID,
		Label:       k.Label,
		PublicKey:   k.PublicKey,
		Fingerprint: k.Fingerprint,
		CreatedAt:   k.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// handleListSSHKeys returns the caller's active login keys, metadata only.
func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	keys, err := s.SSHKeys.List(r.Context(), uuidString(user.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]sshLoginKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, sshLoginKeyViewFrom(k))
	}
	writeJSON(w, http.StatusOK, out)
}

type addSSHKeyRequest struct {
	Label     string `json:"label"`
	PublicKey string `json:"public_key"`
}

// handleAddSSHKey validates + stores a new login key (rejecting an unparsable
// key or a duplicate active fingerprint), re-injects to the user's running
// machines so it is live without recreating a machine, and audits the add.
// There is deliberately no field for a private key anywhere in this request —
// a login key's private half must never leave the user's own machine.
func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req addSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	uid := uuidString(user.ID)
	k, err := s.SSHKeys.Add(r.Context(), uid, req.Label, req.PublicKey)
	switch err {
	case nil:
	case sshkeys.ErrInvalidKey:
		writeError(w, http.StatusUnprocessableEntity, "invalid_key")
		return
	case sshkeys.ErrDuplicateKey:
		writeError(w, http.StatusConflict, "duplicate_key")
		return
	default:
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionSSHKeyAdd,
		Target:   k.ID,
		Metadata: map[string]any{"fingerprint": k.Fingerprint},
	})
	s.reinjectRunningMachines(r.Context(), uid)
	writeJSON(w, http.StatusOK, sshLoginKeyViewFrom(k))
}

// handleRevokeSSHKey revokes (soft-deletes) a login key owned by the caller and
// re-injects to running machines so it is dropped from their
// ~/.ssh/authorized_keys on the very next push — no sshd reload needed, since
// OpenSSH re-reads authorized_keys on every connection attempt.
func (s *Server) handleRevokeSSHKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)
	id := r.PathValue("id")
	existed, err := s.SSHKeys.Revoke(r.Context(), uid, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "unknown_key")
		return
	}
	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionSSHKeyRevoke,
		Target: id,
	})
	s.reinjectRunningMachines(r.Context(), uid)
	w.WriteHeader(http.StatusNoContent)
}
