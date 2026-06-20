package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/guestctl"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// GitWorktree is the control-channel surface the worktree-review API needs (GR1):
// confirm a live channel, resolve the machine's listable projects, and read a
// project's status/diff. *guestctl.Manager satisfies it; an interface keeps the
// handlers unit-testable against a fake. As the git-review work grows (commit,
// branch, push) the mutating verbs join this interface.
type GitWorktree interface {
	HasChannel(machineID string) bool
	ListProjects(ctx context.Context, machineID string) ([]guestwire.Project, error)
	GitStatus(ctx context.Context, machineID, repoPath string) (guestwire.GitStatusResponse, error)
	GitDiff(ctx context.Context, machineID, repoPath string, staged bool) (guestwire.GitDiffResponse, error)
	GitBranch(ctx context.Context, machineID, repoPath, name string, checkout bool, from string) (guestwire.GitBranchResponse, error)
	GitCommit(ctx context.Context, machineID, repoPath, message string, paths []string) (guestwire.GitCommitResponse, error)
	Push(ctx context.Context, machineID, repoPath, branch string, setUpstream bool, opID string) error
}

// handleGitStatus returns the working-tree change set for a project in the
// machine named by the {id} path segment (GR1). The project is named by
// ?project=<name> and validated against the machine's listable set.
func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	machineID, repoPath, ok := s.gitWorktreeContext(w, r, user)
	if !ok {
		return
	}
	st, err := s.GitWorktree.GitStatus(r.Context(), machineID, repoPath)
	if errors.Is(err, guestctl.ErrNoChannel) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	if st.Files == nil {
		st.Files = []guestwire.GitFileStatus{}
	}
	writeJSON(w, http.StatusOK, st)
}

// handleGitDiff returns a project's unified diff (GR1). ?staged=true selects the
// index diff over the worktree diff.
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	machineID, repoPath, ok := s.gitWorktreeContext(w, r, user)
	if !ok {
		return
	}
	staged := r.URL.Query().Get("staged") == "true" || r.URL.Query().Get("staged") == "1"
	d, err := s.GitWorktree.GitDiff(r.Context(), machineID, repoPath, staged)
	if errors.Is(err, guestctl.ErrNoChannel) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// branchRequest is the body of POST /api/machines/{id}/git/branch (GR2).
type branchRequest struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Checkout bool   `json:"checkout"`
	From     string `json:"from"`
}

// handleGitBranch creates (and optionally checks out) a branch in a project
// (GR2). The branch name is validated before dispatch; the guest re-validates
// and reports a duplicate as a distinct error.
func (s *Server) handleGitBranch(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req branchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !guestwire.ValidBranchName(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_branch_name")
		return
	}
	machineID, ok := s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return
	}
	repoPath, code := s.resolveProject(r.Context(), machineID, req.Project)
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}

	res, err := s.GitWorktree.GitBranch(r.Context(), machineID, repoPath, req.Name, req.Checkout, req.From)
	if err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		var ce *guestctl.ControlError
		if errors.As(err, &ce) {
			switch ce.Code {
			case guestwire.ErrCodeBranchExists:
				writeError(w, http.StatusConflict, "branch_exists")
				return
			case guestwire.ErrCodeInvalidBranch:
				writeError(w, http.StatusBadRequest, "invalid_branch_name")
				return
			case guestwire.ErrCodeGitFailed:
				writeError(w, http.StatusUnprocessableEntity, "branch_failed")
				return
			}
		}
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uuidString(user.ID),
		Actor:    audit.UserActor(uuidString(user.ID)),
		Action:   audit.ActionGitBranch,
		Target:   req.Project,
		Metadata: map[string]any{"name": req.Name, "checkout": req.Checkout},
	})
	writeJSON(w, http.StatusOK, res)
}

// commitRequest is the body of POST /api/machines/{id}/git/commit (GR3).
type commitRequest struct {
	Project string   `json:"project"`
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

// handleGitCommit stages the requested paths (or all changes) and commits them
// (GR3). This is the human review gate: there is no path that commits on the
// agent's behalf — it only fires from this explicit, CSRF-guarded request.
func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req commitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "empty_message")
		return
	}
	machineID, ok := s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return
	}
	repoPath, code := s.resolveProject(r.Context(), machineID, req.Project)
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}

	res, err := s.GitWorktree.GitCommit(r.Context(), machineID, repoPath, req.Message, req.Paths)
	if err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		var ce *guestctl.ControlError
		if errors.As(err, &ce) {
			switch ce.Code {
			case guestwire.ErrCodeEmptyMessage:
				writeError(w, http.StatusBadRequest, "empty_message")
				return
			case guestwire.ErrCodeNothingToCommit:
				writeError(w, http.StatusConflict, "nothing_to_commit")
				return
			case guestwire.ErrCodeGitFailed:
				writeError(w, http.StatusUnprocessableEntity, "commit_failed")
				return
			}
		}
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uuidString(user.ID),
		Actor:    audit.UserActor(uuidString(user.ID)),
		Action:   audit.ActionGitCommit,
		Target:   req.Project,
		Metadata: map[string]any{"sha": res.Sha, "paths": len(req.Paths)},
	})
	writeJSON(w, http.StatusOK, res)
}

// pushRequest is the body of POST /api/machines/{id}/git/push (GR4).
type pushRequest struct {
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	SetUpstream bool   `json:"set_upstream"`
}

// pushResponse is the 202 body of POST /api/machines/{id}/git/push: the op id the
// caller correlates to the later git.push SSE event.
type pushResponse struct {
	OpID string `json:"op_id"`
}

// handleGitPush dispatches an async push of a branch to origin (GR4). It returns
// 202 + op_id immediately; completion arrives as a machine_events row (type
// git.push) over the SSE stream. Auth to GitHub is supplied by the in-VM
// credential helper, so no token travels in this request.
func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" || req.Branch == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !guestwire.ValidBranchName(req.Branch) {
		writeError(w, http.StatusBadRequest, "invalid_branch_name")
		return
	}
	machineID, ok := s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return
	}
	repoPath, code := s.resolveProject(r.Context(), machineID, req.Project)
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}

	opID := newOpID()
	if err := s.GitWorktree.Push(r.Context(), machineID, repoPath, req.Branch, req.SetUpstream, opID); err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusInternalServerError, "push_dispatch_failed")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uuidString(user.ID),
		Actor:    audit.UserActor(uuidString(user.ID)),
		Action:   audit.ActionGitPush,
		Target:   req.Project,
		Metadata: map[string]any{"op_id": opID, "branch": req.Branch},
	})
	writeJSON(w, http.StatusAccepted, pushResponse{OpID: opID})
}

// prRequest is the body of POST /api/machines/{id}/git/pr (GR5). Head is the
// (already pushed) branch with the changes; Base defaults to the repo's default
// branch when empty.
type prRequest struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Head    string `json:"head"`
	Base    string `json:"base"`
}

// prResponse is the 200 body of POST .../git/pr: the opened PR's URL and number.
type prResponse struct {
	PRURL  string `json:"pr_url"`
	Number int    `json:"number"`
}

// handleGitPR opens a pull request from Head into Base (GR5) — the only git hop
// that is a CP→GitHub call, not a guest verb. owner/repo come from the project's
// origin remote; Base defaults to the repo's default branch. The user's OAuth
// token authorizes the call (same path as repo listing).
func (s *Server) handleGitPR(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req prRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" || req.Title == "" || req.Head == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !guestwire.ValidBranchName(req.Head) || (req.Base != "" && !guestwire.ValidBranchName(req.Base)) {
		writeError(w, http.StatusBadRequest, "invalid_branch_name")
		return
	}
	machineID, ok := s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return
	}
	remote, code := s.resolveProjectRemote(r.Context(), machineID, req.Project)
	if code == "no_remote" {
		writeError(w, http.StatusUnprocessableEntity, "no_remote")
		return
	}
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}
	owner, repo, ok := parseOwnerRepo(remote)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "bad_remote")
		return
	}

	uid := uuidString(user.ID)
	cred, err := s.Tokens.Token(r.Context(), uid)
	if errors.Is(err, github.ErrReconnectGitHub) {
		writeError(w, http.StatusConflict, "reconnect_github")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "github_unavailable")
		return
	}

	base := req.Base
	if base == "" {
		rp, err := s.GitHub.GetRepo(r.Context(), cred.AccessToken, owner, repo)
		if err != nil || rp.DefaultBranch == "" {
			writeError(w, http.StatusBadGateway, "github_unavailable")
			return
		}
		base = rp.DefaultBranch
	}

	pr, err := s.GitHub.CreatePR(r.Context(), cred.AccessToken, owner, repo, req.Head, base, req.Title, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, github.ErrNoPRCommits):
			writeError(w, http.StatusUnprocessableEntity, "no_commits")
		case errors.Is(err, github.ErrPRAlreadyExists):
			writeError(w, http.StatusConflict, "pr_exists")
		default:
			writeError(w, http.StatusBadGateway, "github_unavailable")
		}
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionGitPRCreate,
		Target:   req.Project,
		Metadata: map[string]any{"number": pr.Number, "head": req.Head, "base": base},
	})
	// Surface the PR over SSE too, so the desktop activity log and any other
	// session learn it landed (best-effort; the response is the primary carrier).
	s.emitMachineEvent(r.Context(), machineID, audit.ActionGitPRCreate, map[string]any{
		"number": pr.Number, "url": pr.HTMLURL, "project": req.Project,
	})
	writeJSON(w, http.StatusOK, prResponse{PRURL: pr.HTMLURL, Number: pr.Number})
}

// resolveProjectRemote returns the origin remote URL of a listable project, with
// an error code the caller maps (bad_project, no_remote, machine_not_running,
// guest_unreachable).
func (s *Server) resolveProjectRemote(ctx context.Context, machineID, name string) (remote, errCode string) {
	if name == "" {
		return "", "bad_project"
	}
	projects, err := s.GitWorktree.ListProjects(ctx, machineID)
	if errors.Is(err, guestctl.ErrNoChannel) {
		return "", "machine_not_running"
	}
	if err != nil {
		return "", "guest_unreachable"
	}
	for _, p := range projects {
		if p.Name == name {
			if p.Remote == "" {
				return "", "no_remote"
			}
			return p.Remote, ""
		}
	}
	return "", "bad_project"
}

// parseOwnerRepo extracts owner/repo from a git remote URL (https or scp-like),
// validating the result against the same owner/repo shape clone enforces.
func parseOwnerRepo(remote string) (owner, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:] // host/owner/repo...
		if k := strings.Index(rest, "/"); k >= 0 {
			s = rest[k+1:]
		}
	} else if at := strings.Index(s, "@"); at >= 0 {
		if colon := strings.Index(s, ":"); colon > at { // git@host:owner/repo
			s = s[colon+1:]
		}
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	owner, repo = parts[0], parts[1]
	if !fullNameRe.MatchString(owner + "/" + repo) {
		return "", "", false
	}
	return owner, repo, true
}

// emitMachineEvent records a CP-side machine event and publishes it to the SSE
// stream. Best-effort: a nil broker/queries or any DB error is silently skipped
// so it can never break the operation it annotates.
func (s *Server) emitMachineEvent(ctx context.Context, machineID, typ string, payload map[string]any) {
	if s.Broker == nil || s.Queries == nil {
		return
	}
	mid, err := machine.ParseUUID(machineID)
	if err != nil {
		return
	}
	body, _ := json.Marshal(payload)
	ev, err := s.Queries.InsertMachineEvent(ctx, store.InsertMachineEventParams{
		MachineID: mid, Type: typ, Actor: "system:httpapi", Payload: body,
	})
	if err != nil {
		return
	}
	mc, err := s.Queries.GetMachineByID(ctx, mid)
	if err != nil {
		return
	}
	s.Broker.Publish(machine.Update{Machine: mc, Event: ev})
}

// gitWorktreeContext resolves the common prelude for a worktree-review request:
// the machine from {id} (owned by the user, running, with a live channel) and
// the absolute repo path for ?project=. It writes the error response itself and
// returns ok=false when the request cannot proceed.
func (s *Server) gitWorktreeContext(w http.ResponseWriter, r *http.Request, user store.User) (machineID, repoPath string, ok bool) {
	machineID, ok = s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return "", "", false
	}
	path, code := s.resolveProject(r.Context(), machineID, r.URL.Query().Get("project"))
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return "", "", false
	}
	return machineID, path, true
}

// resolveWorktreeMachine resolves the {id} machine for a worktree request: owned
// by the user, running, with a live channel. It writes the error itself.
func (s *Server) resolveWorktreeMachine(w http.ResponseWriter, r *http.Request, user store.User) (machineID string, ok bool) {
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		// Foreign/unknown/malformed id all map to 404 to avoid leaking existence.
		writeError(w, http.StatusNotFound, "no_machine")
		return "", false
	}
	machineID = machine.UUIDString(mc.ID)
	if machine.State(mc.State) != machine.StateRunning || !s.GitWorktree.HasChannel(machineID) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return "", false
	}
	return machineID, true
}

// resolveProject matches a project name to its absolute path against the
// machine's listable set (the CP half of the double validation; the guest
// re-checks the path). It returns an error code the caller maps to HTTP:
//
//	"bad_project"        — empty name, or not a listable project
//	"machine_not_running"— the channel dropped between checks
//	"guest_unreachable"  — the project set could not be fetched
func (s *Server) resolveProject(ctx context.Context, machineID, name string) (path, errCode string) {
	if name == "" {
		return "", "bad_project"
	}
	projects, err := s.GitWorktree.ListProjects(ctx, machineID)
	if errors.Is(err, guestctl.ErrNoChannel) {
		return "", "machine_not_running"
	}
	if err != nil {
		return "", "guest_unreachable"
	}
	for _, p := range projects {
		if p.Name == name {
			return p.Path, ""
		}
	}
	return "", "bad_project"
}

// projectErrorStatus maps a resolveProject error code to its HTTP status.
func projectErrorStatus(code string) int {
	switch code {
	case "guest_unreachable":
		return http.StatusBadGateway
	case "machine_not_running":
		return http.StatusConflict
	default: // bad_project
		return http.StatusBadRequest
	}
}
