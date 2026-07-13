package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/gitea"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// Gitea/Forgejo phase 2: per-user PATs for the additional git hosts on the
// PROTEOS_GIT_PUBLIC_HOSTS allowlist. Write-only like the provider-keys API —
// no route ever returns the token, only the non-sensitive login.

// gitHostView is one row of GET /api/git/hosts.
type gitHostView struct {
	Host   string `json:"host"`
	Linked bool   `json:"linked"`
	Login  string `json:"login,omitempty"`
}

// handleGitHosts lists the allowlisted hosts with the user's link state.
func (s *Server) handleGitHosts(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	links, err := s.Queries.ListGitHostLinks(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	logins := make(map[string]string, len(links))
	for _, l := range links {
		logins[l.Host] = loginFromMetadata(l.Metadata)
	}
	out := struct {
		Hosts []gitHostView `json:"hosts"`
	}{Hosts: make([]gitHostView, 0, len(s.GitPublicHosts))}
	for _, host := range s.GitPublicHosts {
		login, linked := logins[host]
		out.Hosts = append(out.Hosts, gitHostView{Host: host, Linked: linked, Login: login})
	}
	writeJSON(w, http.StatusOK, out)
}

// setGitHostTokenRequest is the body of PUT /api/git/hosts/{host}/token. The
// token is validated against the host, then stored — never echoed.
type setGitHostTokenRequest struct {
	Token string `json:"token"`
}

// handleSetGitHostToken validates a PAT against the host (which also captures
// the login git-over-https Basic auth needs as the username) and stores it.
func (s *Server) handleSetGitHostToken(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	host := strings.ToLower(r.PathValue("host"))
	if !slices.Contains(s.GitPublicHosts, host) {
		writeError(w, http.StatusNotFound, "unknown_host")
		return
	}

	var req setGitHostTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	token := strings.TrimSpace(req.Token)

	login, err := s.GiteaFor(host).GetUser(r.Context(), token)
	if errors.Is(err, gitea.ErrBadToken) {
		writeError(w, http.StatusBadRequest, "bad_token")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "githost_unavailable")
		return
	}

	uid := uuidString(user.ID)
	ref := secrets.UserGitHostPath(uid, host)
	if err := s.Secrets.Put(ref, map[string]string{
		secrets.GitHostFieldToken: token,
		secrets.GitHostFieldLogin: login,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	meta, _ := json.Marshal(map[string]any{"login": login})
	if _, err := s.Queries.UpsertGitHostLink(r.Context(), store.UpsertGitHostLinkParams{
		UserID:    user.ID,
		Host:      host,
		Metadata:  meta,
		SecretRef: ref,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionGitHostTokenSet,
		Target:   host,
		Metadata: map[string]any{"login": login},
	})
	writeJSON(w, http.StatusOK, gitHostView{Host: host, Linked: true, Login: login})
}

// handleDeleteGitHostToken removes the user's PAT for a host. Idempotent, and
// deliberately not gated on the allowlist so a link for a host the operator
// has since removed can still be cleaned up.
func (s *Server) handleDeleteGitHostToken(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	host := strings.ToLower(r.PathValue("host"))
	uid := uuidString(user.ID)

	if err := s.Secrets.Delete(secrets.UserGitHostPath(uid, host)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Queries.DeleteGitHostLink(r.Context(), store.DeleteGitHostLinkParams{
		UserID: user.ID,
		Host:   host,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID: uid,
		Actor:  audit.UserActor(uid),
		Action: audit.ActionGitHostTokenDelete,
		Target: host,
	})
	w.WriteHeader(http.StatusNoContent)
}

// loginFromMetadata reads the non-sensitive login hint from a link's metadata.
func loginFromMetadata(meta []byte) string {
	var m struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(meta, &m)
	return m.Login
}
