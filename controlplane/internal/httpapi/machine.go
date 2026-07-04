package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// MachineSummary is the machine shape returned to the SPA. It is the value of
// the `machine` field in /api/me and the body of the /api/machine endpoints.
type MachineSummary struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	State        string          `json:"state"`
	GuestIP      *string         `json:"guest_ip"`
	KernelRef    string          `json:"kernel_ref"`
	RootfsRef    string          `json:"rootfs_ref"`
	TemplateID   *string         `json:"template_id"` // catalog template the machine was created from; null ⇒ legacy
	ResourceSpec json.RawMessage `json:"resource_spec"`
	LastError    *string         `json:"last_error"`
	CreatedAt    string          `json:"created_at"`
	LastActiveAt *string         `json:"last_active_at"` // null when machine has never been active

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
		Name:         m.Name,
		State:        m.State,
		KernelRef:    m.KernelRef,
		RootfsRef:    m.RootfsRef,
		TemplateID:   m.TemplateID,
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
	if m.LastActiveAt.Valid {
		t := m.LastActiveAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		s.LastActiveAt = &t
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

// handleListMachines returns all of the authenticated user's machines (possibly
// empty). This is the multi-machine collection read.
func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ms, err := s.Machines.List(r.Context(), user.ID)
	if err != nil {
		slog.Error("list machines failed", "err", err, "user", user.ID)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]MachineSummary, 0, len(ms))
	for _, m := range ms {
		out = append(out, s.summary(r.Context(), m))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetMachine returns one of the user's machines by id, or 404 no_machine
// (also for a machine the user does not own — existence is never leaked).
func (s *Server) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	m, errCode := s.ownedMachine(r.Context(), user, r.PathValue("id"))
	if errCode != "" {
		writeError(w, http.StatusNotFound, errCode)
		return
	}
	writeJSON(w, http.StatusOK, s.summary(r.Context(), m))
}

// handleCreateMachine provisions a new machine for the user. 202 with the
// (provisioning) summary, 409 machine_limit when the per-user cap is reached, or
// 400 unknown_template when the body names a template not in the catalog. The
// optional JSON body {"name","template_id"} sets the display name (empty ⇒
// auto-named) and the template (empty ⇒ catalog default).
func (s *Server) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	opts := decodeCreateOptions(r)
	m, err := s.Machines.Create(r.Context(), user.ID, opts)
	var invRes machine.InvalidResourcesError
	switch {
	case errors.Is(err, machine.ErrMachineLimit):
		writeError(w, http.StatusConflict, "machine_limit")
	case errors.Is(err, machine.ErrUnknownTemplate):
		writeError(w, http.StatusBadRequest, "unknown_template")
	case errors.As(err, &invRes):
		writeErrorDetail(w, http.StatusBadRequest, "invalid_resources", invRes.Detail)
	case err != nil:
		slog.Error("create machine failed", "err", err, "user", user.ID)
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusAccepted, s.summary(r.Context(), m))
	}
}

// handleStartMachine cold-boots a stopped/errored machine. 202 or 409 invalid_state.
func (s *Server) handleStartMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Start)
}

// handleStopMachine gracefully stops a running machine. 202 or 409 invalid_state.
func (s *Server) handleStopMachine(w http.ResponseWriter, r *http.Request) {
	s.machineMutation(w, r, s.Machines.Stop)
}

// handleRenameMachine sets a machine's display name from a JSON body
// {"name": "..."}. 200 with the updated summary, 404 no_machine, 400 bad_request.
func (s *Server) handleRenameMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	m, err := s.Machines.Rename(r.Context(), user.ID, id, strings.TrimSpace(body.Name))
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case err != nil:
		slog.Error("rename machine failed", "err", err, "user", user.ID)
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, s.summary(r.Context(), m))
	}
}

// handleDestroyMachine tears down and removes one of the user's machines by id.
// 204 on success, 404 no_machine. Irreversible: the persistent disk is wiped
// (unlike stop, which hibernates).
func (s *Server) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	err := s.Machines.Destroy(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case err != nil:
		slog.Error("destroy machine failed", "err", err, "user", user.ID)
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// machineMutation factors the shared shape of start/stop: auth, parse the {id}
// path value, run op (which ownership-checks), map ErrNoMachine→404,
// ErrInvalidState→409, else 202 with the summary.
func (s *Server) machineMutation(w http.ResponseWriter, r *http.Request, op func(context.Context, pgtype.UUID, pgtype.UUID) (store.Machine, error)) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	m, err := op(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case errors.Is(err, machine.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state")
	case err != nil:
		slog.Error("machine mutation failed", "err", err, "user", user.ID)
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusAccepted, s.summary(r.Context(), m))
	}
}

// ownedMachine resolves a machine by its path id and verifies ownership,
// returning a non-empty error code ("no_machine") the caller maps to 404.
func (s *Server) ownedMachine(ctx context.Context, user store.User, idParam string) (store.Machine, string) {
	id, ok := parseMachineID(idParam)
	if !ok {
		return store.Machine{}, "no_machine"
	}
	m, err := s.resolveTerminalMachine(ctx, user, machine.UUIDString(id))
	if err != nil {
		return store.Machine{}, "no_machine"
	}
	return m, ""
}

// parseMachineID parses a machine id path value into a UUID. A malformed value
// yields ok=false (callers map that to 404 to avoid leaking existence).
func parseMachineID(s string) (pgtype.UUID, bool) {
	id, err := machine.ParseUUID(s)
	if err != nil || !id.Valid {
		return pgtype.UUID{}, false
	}
	return id, true
}

// decodeCreateOptions reads the optional {"name","template_id"} create body,
// tolerating an empty/missing body (auto-naming + catalog default then apply). A
// malformed body yields zero options. Values are trimmed.
func decodeCreateOptions(r *http.Request) machine.CreateOptions {
	var body struct {
		Name       string `json:"name"`
		TemplateID string `json:"template_id"`
		Vcpus      *int   `json:"vcpus"`
		MemMiB     *int   `json:"mem_mib"`
		DiskMiB    *int   `json:"disk_mib"`
	}
	if r.Body == nil {
		return machine.CreateOptions{}
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return machine.CreateOptions{}
	}
	return machine.CreateOptions{
		Name:       strings.TrimSpace(body.Name),
		TemplateID: strings.TrimSpace(body.TemplateID),
		Vcpus:      body.Vcpus,
		MemMiB:     body.MemMiB,
		DiskMiB:    body.DiskMiB,
	}
}
