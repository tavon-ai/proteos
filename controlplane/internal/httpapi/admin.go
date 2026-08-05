package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// The admin console: a fleet-wide, READ-ONLY view of every machine and who owns
// it. Every route here sits behind requireAdmin.
//
// Read-only is the whole design, not a first cut waiting to grow buttons. These
// handlers are the one place in the API where a user sees rows that are not
// theirs, so keeping them incapable of mutation means the blast radius of a
// mistakenly-granted proteos.admin role is "saw a machine list", not "destroyed
// someone's work". Admin actions, if they are ever wanted, should be added
// deliberately and with their own audit trail — not by widening these.

// adminMachineListLimit caps the machine table.
//
// The cap exists so one page render cannot be made to serialize an unbounded
// fleet. It applies only to the LIST: the totals come from their own aggregate
// query, so the headline numbers stay exact even when the table is truncated,
// and the response says so explicitly rather than quietly showing a prefix.
const adminMachineListLimit = 500

// adminOverviewResponse is the body of GET /api/admin/overview.
type adminOverviewResponse struct {
	Totals   adminTotals    `json:"totals"`
	Machines []adminMachine `json:"machines"`
	// Truncated is true when the fleet holds more machines than the table
	// returned. The UI must say so — a silently-cut list reads as a complete one.
	Truncated bool `json:"truncated"`
}

// adminTotals are the fleet headline numbers, counted across ALL machines
// regardless of the list cap.
type adminTotals struct {
	// Machines is every machine row that exists.
	Machines int `json:"machines"`
	// Running is machines currently in the 'running' state — the "in use"
	// number, and the one that maps to consumed host CPU and memory.
	Running int `json:"running"`
	// ByState is the full state histogram, so the console can show where the
	// rest of the fleet sits (stopped, error, mid-transition) without this
	// struct needing a field per state in the machines CHECK constraint.
	ByState map[string]int `json:"by_state"`
	// Users is every account; UsersWithMachines is how many own at least one.
	Users             int `json:"users"`
	UsersWithMachines int `json:"users_with_machines"`
}

// adminOwner is the owning user, trimmed to what the console renders. It is
// deliberately not the whole users row: an admin view has no business shipping
// OIDC subjects or GitHub link state to the browser.
type adminOwner struct {
	ID        string `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// adminMachine is one row of the fleet table.
type adminMachine struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Owner        adminOwner      `json:"owner"`
	TemplateID   *string         `json:"template_id"`
	ResourceSpec json.RawMessage `json:"resource_spec"`
	LastError    *string         `json:"last_error"`
	Host         *string         `json:"host"`          // node the machine is placed on; null before scheduling
	CreatedAt    string          `json:"created_at"`
	LastActiveAt *string         `json:"last_active_at"` // null when never active
}

// handleAdminOverview answers the console's single read: fleet totals plus the
// machine table with owners.
//
// One endpoint rather than three (counts / machines / users) because the page
// renders them together and split reads could disagree — a machine appearing in
// the table but not the count is the kind of inconsistency an admin reasonably
// reads as a bug in the fleet rather than in the console.
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if s.Queries == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	ctx := r.Context()

	states, err := s.Queries.AdminCountMachinesByState(ctx)
	if err != nil {
		slog.Error("admin: count machines by state failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	users, err := s.Queries.AdminCountUsers(ctx)
	if err != nil {
		slog.Error("admin: count users failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	// One over the cap: if the extra row comes back the fleet is larger than the
	// table, which is what Truncated reports. Cheaper and race-free next to
	// comparing the list length against a separately-counted total.
	rows, err := s.Queries.AdminListMachines(ctx, adminMachineListLimit+1)
	if err != nil {
		slog.Error("admin: list machines failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	truncated := len(rows) > adminMachineListLimit
	if truncated {
		rows = rows[:adminMachineListLimit]
	}

	totals := adminTotals{
		ByState:           make(map[string]int, len(states)),
		Users:             int(users.Total),
		UsersWithMachines: int(users.WithMachines),
	}
	for _, s := range states {
		n := int(s.Count)
		totals.ByState[s.State] = n
		totals.Machines += n
		if s.State == machineStateRunning {
			totals.Running = n
		}
	}

	resp := adminOverviewResponse{
		Totals:    totals,
		Machines:  make([]adminMachine, 0, len(rows)),
		Truncated: truncated,
	}
	for _, row := range rows {
		resp.Machines = append(resp.Machines, toAdminMachine(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// machineStateRunning is the machines.state value that counts as "in use".
const machineStateRunning = "running"

// toAdminMachine renders one joined machine+owner row for the API.
func toAdminMachine(row store.AdminListMachinesRow) adminMachine {
	m := adminMachine{
		ID:           machine.UUIDString(row.ID),
		Name:         row.Name,
		State:        row.State,
		TemplateID:   row.TemplateID,
		ResourceSpec: json.RawMessage(row.ResourceSpec),
		LastError:    row.LastError,
		Host:         row.HostName,
		Owner: adminOwner{
			ID:        machine.UUIDString(row.UserID),
			Login:     row.OwnerLogin,
			Email:     row.OwnerEmail,
			AvatarURL: row.OwnerAvatarUrl,
		},
	}
	if row.CreatedAt.Valid {
		m.CreatedAt = row.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if row.LastActiveAt.Valid {
		t := row.LastActiveAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		m.LastActiveAt = &t
	}
	return m
}
