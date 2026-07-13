package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// GitChannel is the control-channel surface the git API needs: ask whether a
// machine has a live channel and dispatch a clone. *guestctl.Manager satisfies
// it; an interface keeps the handlers unit-testable.
type GitChannel interface {
	HasChannel(machineID string) bool
	Clone(ctx context.Context, machineID, url, dest, opID string) error
}

// fullNameRe constrains a repo full-name to owner/repo of path-safe characters,
// rejecting nested paths before it is used to build a clone dest.
var fullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validFullName reports whether s is an owner/repo full-name safe to build a
// clone dest from. fullNameRe alone admits all-dot segments ("owner/.." would
// make the dest /workspace/..), so those are rejected explicitly; no real
// forge allows them as owner or repo names.
func validFullName(s string) bool {
	if !fullNameRe.MatchString(s) {
		return false
	}
	for seg := range strings.SplitSeq(s, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

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

// cloneRequest is the body of POST /api/git/clone. Exactly one of FullName
// (the GitHub picker path — cloned from s.GitHost) or URL (clone-by-URL — the
// host must be s.GitHost or one of s.GitPublicHosts) must be set.
type cloneRequest struct {
	FullName string `json:"full_name"`
	URL      string `json:"url"`
}

// cloneResponse is the 202 body of POST /api/git/clone.
type cloneResponse struct {
	OpID string `json:"op_id"`
}

// handleGitClone dispatches an async clone into the machine's workspace.
// Completion arrives as a machine_events row (type git.clone) over the SSE
// stream. The target need not appear in the user's granted set: the URL is
// host-pinned to the operator-configured allowlist (s.GitHost plus
// s.GitPublicHosts, rebuilt from validated parts — never the raw user string)
// and fullNameRe rejects traversal, so the worst case is cloning a public
// repo. The credential helper only ever supplies the user's own token, and
// only for s.GitHost, so a private repo they have not granted simply fails at
// the git layer (reported as a failed git.clone event) rather than leaking
// access.
func (s *Server) handleGitClone(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	uid := uuidString(user.ID)

	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.FullName == "") == (req.URL == "") {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	host, fullName := s.GitHost, req.FullName
	if req.URL != "" {
		var err error
		host, fullName, err = parseCloneURL(req.URL, s.GitHost, s.GitPublicHosts)
		if errors.Is(err, errForbiddenHost) {
			writeError(w, http.StatusBadRequest, "forbidden_host")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_url")
			return
		}
	} else if !validFullName(fullName) {
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

	opID := newOpID()
	// The clone URL embeds no token — the credential helper supplies it at fetch
	// time (and only for s.GitHost), so .git/config keeps a clean URL (decision #7).
	cloneURL := fmt.Sprintf("https://%s/%s.git", host, fullName)
	dest := "/workspace/" + repoDir(fullName)

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
		Target:   fullName,
		Metadata: map[string]any{"op_id": opID, "host": host},
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

// errForbiddenHost marks a clone URL whose host is outside the allowlist, so
// the handler can distinguish it from a malformed URL.
var errForbiddenHost = errors.New("host not allowlisted")

// parseCloneURL validates a user-supplied clone URL and returns its host and
// owner/repo full-name. It accepts https://host[:port]/owner/repo[.git] (a
// trailing slash is tolerated) where host is gitHost or one of publicHosts
// (case-insensitive). The caller rebuilds the clone URL from the returned
// parts, so nothing else in the raw string — userinfo, query, fragment, extra
// path segments — can reach the guest; all are rejected here.
func parseCloneURL(raw, gitHost string, publicHosts []string) (host, fullName string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("unparseable url: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("clone url must be https://host/owner/repo with no credentials, query, or fragment")
	}
	fullName = strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !validFullName(fullName) {
		return "", "", errors.New("clone url path must be exactly owner/repo")
	}
	host = strings.ToLower(u.Host)
	if host != strings.ToLower(gitHost) && !slices.Contains(publicHosts, host) {
		return "", "", errForbiddenHost
	}
	return host, fullName, nil
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
