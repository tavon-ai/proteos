package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"

	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// desktopResponse is the body of GET /api/machine/desktop. Layout is the raw
// serialized window layout the desktop stored, or null when unset (Phase 9
// decision #6). It is opaque to the control plane — the React desktop owns its
// shape; the CP only relays it to/from machine SQLite.
type desktopResponse struct {
	Layout json.RawMessage `json:"layout"`
}

// desktopRequest is the body of PUT /api/machine/desktop.
type desktopRequest struct {
	Layout json.RawMessage `json:"layout"`
}

// handleGetDesktop reads the stored desktop layout from the machine's SQLite kv
// over the control channel.
func (s *Server) handleGetDesktop(w http.ResponseWriter, r *http.Request) {
	machineID, ok := s.runningMachineID(w, r)
	if !ok {
		return
	}
	value, err := s.Projects.KVGet(r.Context(), machineID, guestwire.KeyDesktopLayout)
	if errors.Is(err, guestctl.ErrNoChannel) {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	out := desktopResponse{Layout: json.RawMessage("null")}
	if value != nil {
		// The stored value is itself a JSON document; relay it verbatim.
		out.Layout = json.RawMessage(*value)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutDesktop writes the desktop layout into the machine's SQLite kv. On a
// diskless stack the guest acks without persisting (decision #6); the response is
// still 204 so the debounced client save never surfaces an error.
func (s *Server) handlePutDesktop(w http.ResponseWriter, r *http.Request) {
	machineID, ok := s.runningMachineID(w, r)
	if !ok {
		return
	}
	var req desktopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Layout) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !json.Valid(req.Layout) {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if err := s.Projects.KVSet(r.Context(), machineID, guestwire.KeyDesktopLayout, string(req.Layout)); err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runningMachineID resolves the caller's machine and confirms it is running,
// writing the appropriate error and returning ok=false otherwise.
func (s *Server) runningMachineID(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		writeError(w, http.StatusConflict, "machine_not_running")
		return "", false
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return "", false
	}
	return machine.UUIDString(m.ID), true
}
