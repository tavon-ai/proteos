package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tavon-ai/proteos/controlplane/internal/profile"
)

// GitConfigurer re-pushes git.configure to a user's running machines so a
// portable git-identity change takes effect without recreating them (Phase 4).
// Satisfied by *guestctl.Manager; nil ⇒ identity changes apply only to new
// machines (and only when the git control channel is wired at all).
type GitConfigurer interface {
	ReconfigureUser(ctx context.Context, userID string)
}

// gitIdentityView is GET /api/profile/git. source is "profile" when the user has
// set a portable identity, else "github" (the fallback used by git.configure).
type gitIdentityView struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Source string `json:"source"`
}

// handleGetGitIdentity returns the effective git identity: the portable profile
// identity if set, else the GitHub-derived default that git.configure would use.
func (s *Server) handleGetGitIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)
	name, email, set, err := s.Profile.GitIdentity(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if set {
		writeJSON(w, http.StatusOK, gitIdentityView{Name: name, Email: email, Source: "profile"})
		return
	}
	// GitHub default: login + primary/noreply email (mirrors guestctl.configure).
	defEmail := user.Email
	if defEmail == "" {
		defEmail = fmt.Sprintf("%s@users.noreply.github.com", user.Login)
	}
	writeJSON(w, http.StatusOK, gitIdentityView{Name: user.Login, Email: defEmail, Source: "github"})
}

type setGitIdentityRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// handleSetGitIdentity sets the portable git identity and re-applies it to the
// user's running machines (best-effort).
func (s *Server) handleSetGitIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req setGitIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	name := strings.TrimSpace(req.Name)
	email := strings.TrimSpace(req.Email)
	if name == "" || email == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_field")
		return
	}
	if len(name) > 256 || len(email) > 256 || !looksLikeEmail(email) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_field")
		return
	}
	uid := uuidString(user.ID)
	if err := s.Profile.SetGitIdentity(r.Context(), uid, name, email); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	s.reconfigureGit(r.Context(), uid)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteGitIdentity clears the portable git identity (revert to GitHub
// default) and re-applies to running machines.
func (s *Server) handleDeleteGitIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)
	existed, err := s.Profile.ClearGitIdentity(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "not_set")
		return
	}
	s.reconfigureGit(r.Context(), uid)
	w.WriteHeader(http.StatusNoContent)
}

// looksLikeEmail does a minimal structural check: exactly one "@" with a
// non-empty local part and a non-empty domain. It is deliberately permissive (the
// value only lands in ~/.gitconfig, never a shell), just rejecting obvious junk.
func looksLikeEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	return at > 0 && at < len(email)-1 && strings.IndexByte(email[at+1:], '@') < 0
}

// reconfigureGit re-applies the git identity to the user's running machines, if
// the configurer is wired.
func (s *Server) reconfigureGit(ctx context.Context, userID string) {
	if s.GitConfigurer != nil {
		s.GitConfigurer.ReconfigureUser(ctx, userID)
	}
}

// sshKeyView is GET/POST /api/profile/ssh. The private key is NEVER part of this
// (or any) response — only the public key and its fingerprint.
type sshKeyView struct {
	PublicKey   string `json:"public_key,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Present     bool   `json:"present"`
}

// handleGetSSHKey returns whether a key is set and, if so, its public key +
// fingerprint (never the private key).
func (s *Server) handleGetSSHKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	pub, present, err := s.Profile.SSHPublicKey(r.Context(), uuidString(user.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	view := sshKeyView{Present: present}
	if present {
		view.PublicKey = pub
		if fp, err := profile.SSHFingerprint(pub); err == nil {
			view.Fingerprint = fp
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// handleGenerateSSHKey mints a fresh ed25519 keypair, stores the private key as a
// file-kind item (0600 under ~/.ssh) plus the SSH client config, re-injects to
// running machines, and returns the public key + fingerprint for the user to add
// to GitHub. Re-posting replaces the existing key.
func (s *Server) handleGenerateSSHKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	priv, pub, fp, err := profile.GenerateSSHKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	uid := uuidString(user.ID)
	if err := s.Profile.SetSSHKey(r.Context(), uid, priv, pub); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	s.reinjectRunningMachines(r.Context(), uid)
	writeJSON(w, http.StatusOK, sshKeyView{Present: true, PublicKey: pub, Fingerprint: fp})
}

// handleDeleteSSHKey removes the SSH key (and its config) and re-injects so it is
// dropped from running machines.
func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)
	existed, err := s.Profile.DeleteSSHKey(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "not_set")
		return
	}
	s.reinjectRunningMachines(r.Context(), uid)
	w.WriteHeader(http.StatusNoContent)
}
