package httpapi

import (
	"context"
	"errors"
	"net/http"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"

	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// ProjectChannel is the control-channel surface the projects/desktop API needs:
// list the machine's cloned projects and read/write its SQLite kv (the desktop
// layout). *guestctl.Manager satisfies it; an interface keeps the handlers
// unit-testable against a fake.
type ProjectChannel interface {
	HasChannel(machineID string) bool
	ListProjects(ctx context.Context, machineID string) ([]guestwire.Project, error)
	KVGet(ctx context.Context, machineID, key string) (*string, error)
	KVSet(ctx context.Context, machineID, key, value string) error
}

// projectsResponse is the envelope of GET /api/projects.
type projectsResponse struct {
	Projects []guestwire.Project `json:"projects"`
}

// handleProjects lists the cloned repositories in the user's running machine's
// workspace (Phase 9 decision #4). The filesystem is the source of truth, so the
// list is fetched live over the control channel each call; the desktop refetches
// on the git.clone SSE event.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	machineID := machine.UUIDString(m.ID)
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	projects, err := s.Projects.ListProjects(r.Context(), machineID)
	if errors.Is(err, guestctl.ErrNoChannel) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	if projects == nil {
		projects = []guestwire.Project{}
	}
	writeJSON(w, http.StatusOK, projectsResponse{Projects: projects})
}

// resolveSessionCwd validates a requested session working directory against the
// machine's listable projects (Phase 9 decision #3, CP half of the double
// validation). It returns the cleaned cwd to forward to the guest. An empty raw
// cwd yields ("", "") — no cwd, existing $HOME behavior. A bad value yields an
// error code the caller maps to its HTTP/WS error:
//
//	"bad_cwd"          — not under /workspace, or not a listable project
//	"guest_unreachable"— the project set could not be fetched
//
// The cwd must equal one of the machine's project paths exactly: windows open at
// a repo root, and an exact match keeps the check honest without walking the tree.
func (s *Server) resolveSessionCwd(ctx context.Context, machineID, raw string) (cwd, errCode string) {
	if raw == "" {
		return "", ""
	}
	clean, ok := guestwire.CleanWorkdir(raw)
	if !ok {
		return "", "bad_cwd"
	}
	if s.Projects == nil {
		// No listable set to validate against ⇒ reject rather than trust the raw
		// path (the guest re-checks prefix+existence, but the CP must not be an
		// open "start a shell anywhere under /workspace" proxy).
		return "", "bad_cwd"
	}
	projects, err := s.Projects.ListProjects(ctx, machineID)
	if err != nil {
		return "", "guest_unreachable"
	}
	for _, p := range projects {
		if p.Path == clean {
			return clean, ""
		}
	}
	return "", "bad_cwd"
}

// cwdErrorStatus maps a resolveSessionCwd error code to its pre-upgrade HTTP
// status (the gateway error set: 400 bad_cwd, 502 guest_unreachable).
func cwdErrorStatus(code string) int {
	if code == "guest_unreachable" {
		return http.StatusBadGateway
	}
	return http.StatusBadRequest
}
