package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/session"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// handleWebSession mints the one-shot, ≤60s web-session URL the SPA navigates the
// editor iframe to (Phase 8 decision #2). It runs on the MAIN origin behind
// requireAuth + the CSRF header; the minted token carries {machine_id, user_id,
// session_id} so the machine-web origin can set its subdomain cookie bound to
// this parent session. Resolution mirrors the terminal gateway: an empty
// ?machine= is the user's single machine; a provided id must be owned.
func (s *Server) handleWebSession(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID, _ := sessionIDFromContext(r.Context())

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}

	machineID := machine.UUIDString(m.ID)

	// PP1: an optional ?port= mints a preview origin (m-<uuid>-p<port>) instead of
	// the editor. The port must be an in-range preview port — reserved system
	// ports (1024 terminal / 1025 code-server) and out-of-range values are 400
	// before any URL is minted or guest dialled. Absent ⇒ the editor (port 0).
	port, ok := webSessionPort(w, r)
	if !ok {
		return
	}

	// Phase 9 decision #5: an optional {folder} opens code-server directly on a
	// project. It is validated against the machine's listable projects (the same
	// check as a session cwd); absent ⇒ the workspace root (Phase 8 behavior).
	// Folder applies to the editor only; a preview opens the app root.
	folder := ""
	if port == 0 {
		folder, ok = s.webSessionFolder(w, r, machineID)
		if !ok {
			return
		}
	}

	url := s.MachineWeb.MintWebSessionURL(externalScheme(r), machineID, sessionID, machine.UUIDString(user.ID), folder, port)
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// webSessionPort reads the optional ?port= and validates it as a preview port.
// It returns (0, true) when absent (the editor), (port, true) for a valid
// in-range preview port, and (0, false) — after writing 400 bad_request — for a
// malformed, reserved, or out-of-range value.
func webSessionPort(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	raw := r.URL.Query().Get(agentapi.GuestPortParam)
	if raw == "" {
		return 0, true
	}
	p, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || !agentapi.ValidPreviewPort(uint32(p), agentapi.DefaultPreviewPortMin, agentapi.DefaultPreviewPortMax) {
		writeError(w, http.StatusBadRequest, "bad_request")
		return 0, false
	}
	return uint32(p), true
}

// webSessionFolder decodes the optional {folder} from the request body and
// validates it against the machine's listable projects. It returns the validated
// folder ("" when absent) and ok=false (after writing 400 bad_folder) when a
// supplied folder is not a listable project. A missing/empty body is fine.
func (s *Server) webSessionFolder(w http.ResponseWriter, r *http.Request, machineID string) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(body) == 0 {
		return "", true
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return "", false
	}
	if req.Folder == "" {
		return "", true
	}
	folder, errCode := s.resolveSessionCwd(r.Context(), machineID, req.Folder)
	if errCode != "" {
		// Surface the editor-specific code; a guest_unreachable still reads as a
		// bad_folder from the SPA's perspective (it cannot open that folder now).
		writeError(w, http.StatusBadRequest, "bad_folder")
		return "", false
	}
	return folder, true
}

// externalScheme resolves the browser-facing scheme of the main origin, honoring
// the proxy layer's X-Forwarded-Proto (NPMplus / app-stack nginx terminate TLS).
func externalScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- gateway adapters -------------------------------------------------------
//
// The gateway machine-web handler depends on small string-typed interfaces so it
// stays decoupled from store/pgtype; the Server (which owns the session + machine
// services) supplies them.

// sessionOwnerAdapter satisfies gateway.SessionResolver via the session manager.
type sessionOwnerAdapter struct{ sessions *session.Manager }

func (a sessionOwnerAdapter) SessionOwner(ctx context.Context, sessionID string) (string, bool, error) {
	id, err := machine.ParseUUID(sessionID)
	if err != nil || !id.Valid {
		return "", false, nil
	}
	user, err := a.sessions.AliveByID(ctx, id)
	if err != nil {
		if errors.Is(err, session.ErrInvalidSession) {
			return "", false, nil
		}
		return "", false, err
	}
	return machine.UUIDString(user.ID), true, nil
}

// machineOwnerAdapter satisfies gateway.MachineResolver via the machine service.
type machineOwnerAdapter struct{ machines *machine.Service }

func (a machineOwnerAdapter) MachineOwner(ctx context.Context, machineID string) (string, bool, bool, error) {
	id, err := machine.ParseUUID(machineID)
	if err != nil || !id.Valid {
		return "", false, false, nil
	}
	m, err := a.machines.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, machine.ErrNoMachine) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	running := machine.State(m.State) == machine.StateRunning
	return machine.UUIDString(m.UserID), running, true, nil
}

// MachineWebResolvers builds the gateway resolver adapters from the session +
// machine services. Exported so main.go can wire NewMachineWeb without poking at
// the unexported adapter types.
func MachineWebResolvers(sessions *session.Manager, machines *machine.Service) (gateway.SessionResolver, gateway.MachineResolver) {
	return sessionOwnerAdapter{sessions: sessions}, machineOwnerAdapter{machines: machines}
}
