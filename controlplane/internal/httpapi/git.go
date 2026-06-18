package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/guestctl"
	"github.com/tavon/proteos/controlplane/internal/machine"
)

// GitChannel is the control-channel surface the git API needs: ask whether a
// machine has a live channel and dispatch a clone. *guestctl.Manager satisfies
// it; an interface keeps the handlers unit-testable.
type GitChannel interface {
	HasChannel(machineID string) bool
	Clone(ctx context.Context, machineID, url, dest, opID string) error
}

// fullNameRe constrains a repo full-name to owner/repo of path-safe characters,
// rejecting traversal and nested paths before it is used to build a clone dest.
var fullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// repoView is one repository in GET /api/git/repos.
type repoView struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
}

// reposResponse is the envelope of GET /api/git/repos. grants_url links to the
// App's installation-settings page so the user can choose which repos ProteOS
// may access (Phase 7 decision #7).
type reposResponse struct {
	Repos     []repoView `json:"repos"`
	GrantsURL string     `json:"grants_url,omitempty"`
}

// handleGitRepos lists the repos the user has granted the App access to.
func (s *Server) handleGitRepos(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)

	repos, err := s.listRepos(r.Context(), uid)
	if errors.Is(err, github.ErrReconnectGitHub) {
		writeError(w, http.StatusConflict, "reconnect_github")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "github_unavailable")
		return
	}

	out := reposResponse{Repos: make([]repoView, 0, len(repos)), GrantsURL: s.grantsURL()}
	for _, repo := range repos {
		out.Repos = append(out.Repos, repoView{
			FullName:      repo.FullName,
			Private:       repo.Private,
			DefaultBranch: repo.DefaultBranch,
			PushedAt:      repo.PushedAt.UTC().Format(time.RFC3339),
		})
	}
	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionGitRepos,
		Metadata: map[string]any{"count": len(out.Repos)},
	})
	writeJSON(w, http.StatusOK, out)
}

// cloneRequest is the body of POST /api/git/clone.
type cloneRequest struct {
	FullName string `json:"full_name"`
}

// cloneResponse is the 202 body of POST /api/git/clone.
type cloneResponse struct {
	OpID string `json:"op_id"`
}

// handleGitClone validates the target against the user's listable repo set and
// dispatches an async clone into the machine's workspace. Completion arrives as a
// machine_events row (type git.clone) over the SSE stream.
func (s *Server) handleGitClone(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)

	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FullName == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !fullNameRe.MatchString(req.FullName) {
		writeError(w, http.StatusBadRequest, "bad_full_name")
		return
	}

	// The target machine (?machine=<id>) must exist, be owned by the user, and be
	// running with a live channel.
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	machineID := machine.UUIDString(mc.ID)
	if machine.State(mc.State) != machine.StateRunning || !s.GitChannel.HasChannel(machineID) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	// Validate the target appears in the user's listable (granted) set — the
	// control plane is not a clone-anything proxy.
	repos, err := s.listRepos(r.Context(), uid)
	if errors.Is(err, github.ErrReconnectGitHub) {
		writeError(w, http.StatusConflict, "reconnect_github")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "github_unavailable")
		return
	}
	if !containsRepo(repos, req.FullName) {
		writeError(w, http.StatusNotFound, "repo_not_listable")
		return
	}

	opID := newOpID()
	// The clone URL embeds no token — the credential helper supplies it at fetch
	// time, so .git/config keeps a clean URL (decision #7).
	cloneURL := fmt.Sprintf("https://%s/%s.git", s.GitHost, req.FullName)
	dest := "/workspace/" + repoDir(req.FullName)

	if err := s.GitChannel.Clone(r.Context(), machineID, cloneURL, dest, opID); err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusInternalServerError, "clone_dispatch_failed")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionGitClone,
		Target:   req.FullName,
		Metadata: map[string]any{"op_id": opID},
	})
	writeJSON(w, http.StatusAccepted, cloneResponse{OpID: opID})
}

// listRepos resolves a valid token and lists the user's accessible repos,
// translating a revoked grant to github.ErrReconnectGitHub.
func (s *Server) listRepos(ctx context.Context, uid string) ([]github.Repo, error) {
	cred, err := s.Tokens.Token(ctx, uid)
	if err != nil {
		return nil, err
	}
	return s.GitHub.ListUserRepos(ctx, cred.AccessToken)
}

// grantsURL builds the App installation-management URL, or "" if the slug is
// unset (the UI then omits the "choose repos" links).
func (s *Server) grantsURL() string {
	if s.GitHubAppSlug == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/apps/%s/installations/new", s.GitHubAppSlug)
}

func containsRepo(repos []github.Repo, fullName string) bool {
	for _, r := range repos {
		if r.FullName == fullName {
			return true
		}
	}
	return false
}

// repoDir is the workspace directory name for a repo full-name (the part after
// the slash). fullNameRe has already rejected traversal characters.
func repoDir(fullName string) string {
	for i := len(fullName) - 1; i >= 0; i-- {
		if fullName[i] == '/' {
			return fullName[i+1:]
		}
	}
	return fullName
}

func newOpID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
