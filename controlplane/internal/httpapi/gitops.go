package httpapi

import (
	"context"
	"errors"
	"net/http"

	guestwire "github.com/tavon/proteos/guestagent/api"

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

// gitWorktreeContext resolves the common prelude for a worktree-review request:
// the machine from {id} (owned by the user, running, with a live channel) and
// the absolute repo path for ?project=. It writes the error response itself and
// returns ok=false when the request cannot proceed.
func (s *Server) gitWorktreeContext(w http.ResponseWriter, r *http.Request, user store.User) (machineID, repoPath string, ok bool) {
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		// Foreign/unknown/malformed id all map to 404 to avoid leaking existence.
		writeError(w, http.StatusNotFound, "no_machine")
		return "", "", false
	}
	machineID = machine.UUIDString(mc.ID)
	if machine.State(mc.State) != machine.StateRunning || !s.GitWorktree.HasChannel(machineID) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return "", "", false
	}
	path, code := s.resolveProject(r.Context(), machineID, r.URL.Query().Get("project"))
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return "", "", false
	}
	return machineID, path, true
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
