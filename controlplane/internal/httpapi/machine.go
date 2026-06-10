package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// MachineSummary is the machine shape returned to the SPA. It is the value of
// the `machine` field in /api/me and the body of the /api/machine endpoints.
type MachineSummary struct {
	ID           string          `json:"id"`
	State        string          `json:"state"`
	GuestIP      *string         `json:"guest_ip"`
	KernelRef    string          `json:"kernel_ref"`
	RootfsRef    string          `json:"rootfs_ref"`
	ResourceSpec json.RawMessage `json:"resource_spec"`
	LastError    *string         `json:"last_error"`
	CreatedAt    string          `json:"created_at"`
}

// toSummary renders a store.Machine as the API summary.
func toSummary(m store.Machine) MachineSummary {
	s := MachineSummary{
		ID:           machine.UUIDString(m.ID),
		State:        m.State,
		KernelRef:    m.KernelRef,
		RootfsRef:    m.RootfsRef,
		ResourceSpec: json.RawMessage(m.ResourceSpec),
		LastError:    m.LastError,
	}
	if m.GuestIp != nil {
		ip := m.GuestIp.String()
		s.GuestIP = &ip
	}
	if m.CreatedAt.Valid {
		s.CreatedAt = m.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return s
}

// handleGetMachine returns the authenticated user's machine, or 404 no_machine.
func (s *Server) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	m, err := s.Machines.Get(r.Context(), user.ID)
	if errors.Is(err, machine.ErrNoMachine) {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, toSummary(m))
}

// handleCreateMachine provisions the user's machine. 202 with the (provisioning)
// summary, or 409 machine_exists if they already have one.
func (s *Server) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	m, err := s.Machines.Create(r.Context(), user.ID)
	if errors.Is(err, machine.ErrMachineExists) {
		writeError(w, http.StatusConflict, "machine_exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusAccepted, toSummary(m))
}

// handleStartMachine cold-boots a stopped/errored machine. 202 or 409 invalid_state.
func (s *Server) handleStartMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Start)
}

// handleStopMachine gracefully stops a running machine. 202 or 409 invalid_state.
func (s *Server) handleStopMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Stop)
}

// machineMutation factors the shared shape of start/stop: auth, run op, map
// ErrNoMachine→404, ErrInvalidState→409, else 202 with the summary.
func (s *Server) machineMutation(w http.ResponseWriter, r *http.Request, op func(context.Context, pgtype.UUID) (store.Machine, error)) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	m, err := op(r.Context(), user.ID)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case errors.Is(err, machine.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusAccepted, toSummary(m))
	}
}
