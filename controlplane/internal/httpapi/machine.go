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

	// Phase 4: persistent disk + hibernate/resume.
	Boot     *string          `json:"boot"`     // "cold" | "resumed" | null
	DiskID   *string          `json:"disk_id"`  // null if not yet provisioned
	DiskMiB  *int             `json:"disk_mib"` // attached disk size
	Snapshot *SnapshotSummary `json:"snapshot"` // present only when hibernated
}

// SnapshotSummary is the current hibernation snapshot metadata in the API.
type SnapshotSummary struct {
	FCVersion string `json:"fc_version"`
	MemBytes  int64  `json:"mem_bytes"`
	CreatedAt string `json:"created_at"`
}

// toSummary renders a store.Machine (plus its optional disk + snapshot) as the
// API summary. disk/snap may be nil when absent.
func toSummary(m store.Machine, disk *store.Disk, snap *store.Snapshot) MachineSummary {
	s := MachineSummary{
		ID:           machine.UUIDString(m.ID),
		State:        m.State,
		KernelRef:    m.KernelRef,
		RootfsRef:    m.RootfsRef,
		ResourceSpec: json.RawMessage(m.ResourceSpec),
		LastError:    m.LastError,
		Boot:         m.Boot,
	}
	if m.GuestIp != nil {
		ip := m.GuestIp.String()
		s.GuestIP = &ip
	}
	if m.CreatedAt.Valid {
		s.CreatedAt = m.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if disk != nil {
		id := machine.UUIDString(disk.ID)
		size := int(disk.SizeMib)
		s.DiskID, s.DiskMiB = &id, &size
	}
	if snap != nil {
		created := ""
		if snap.CreatedAt.Valid {
			created = snap.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		s.Snapshot = &SnapshotSummary{FCVersion: snap.FcVersion, MemBytes: snap.MemBytes, CreatedAt: created}
	}
	return s
}

// summary enriches a machine with its disk + current snapshot and renders the
// API summary. Disk/snapshot lookups are best-effort: a lookup error degrades to
// a summary without that field rather than failing the whole response.
func (s *Server) summary(ctx context.Context, m store.Machine) MachineSummary {
	disk, err := s.Machines.DiskFor(ctx, m.ID)
	if err != nil {
		disk = nil
	}
	snap, err := s.Machines.SnapshotFor(ctx, m.ID)
	if err != nil {
		snap = nil
	}
	return toSummary(m, disk, snap)
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
	writeJSON(w, http.StatusOK, s.summary(r.Context(), m))
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
	writeJSON(w, http.StatusAccepted, s.summary(r.Context(), m))
}

// handleStartMachine cold-boots a stopped/errored machine. 202 or 409 invalid_state.
func (s *Server) handleStartMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Start)
}

// handleStopMachine gracefully stops a running machine. 202 or 409 invalid_state.
func (s *Server) handleStopMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Stop)
}

// handleDestroyMachine tears down and removes the user's machine. 204 on
// success, 404 no_machine if they have none. Irreversible: the persistent disk
// is wiped (unlike stop, which hibernates).
func (s *Server) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	err := s.Machines.Destroy(r.Context(), user.ID)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
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
		writeJSON(w, http.StatusAccepted, s.summary(r.Context(), m))
	}
}
