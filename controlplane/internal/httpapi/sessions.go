package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// Limits for the sessions API (TAV-107), mirroring the logs API's shape:
// maxSessionsLimit/defaultSessionsLimit bound how many of a user's coding
// agent sessions a single GET returns.
const (
	maxSessionsLimit     = 2000
	defaultSessionsLimit = 500
)

// sessionView is one coding agent session (an agent_tasks row) in the GET
// responses, across every machine the user owns. It is deliberately close to
// taskView (httpapi/tasks.go) but adds MachineID/MachineName, since the
// Sessions page — unlike the per-machine Tasks window — needs to say where
// each session ran.
type sessionView struct {
	ID             string          `json:"id"`
	MachineID      string          `json:"machine_id"`
	MachineName    string          `json:"machine_name"`
	Status         string          `json:"status"`
	Provider       string          `json:"provider"`
	Project        string          `json:"project"`
	Prompt         string          `json:"prompt"`
	AgentSessionID string          `json:"agent_session_id,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	ResultSummary  string          `json:"result_summary,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      string          `json:"created_at"`
	StartedAt      string          `json:"started_at,omitempty"`
	EndedAt        string          `json:"ended_at,omitempty"`
}

type sessionsResponse struct {
	Sessions []sessionView `json:"sessions"`
}

// handleListSessions returns the caller's coding agent sessions — headless
// agent runs (AT1), past and in-progress — across every machine they own,
// newest first. ?status=active|finished filters to in-progress
// (queued/running) or terminal (done/failed/canceled) sessions (default:
// both); ?limit caps the count (default 500, max 2000).
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	status, ok := parseSessionStatus(r.URL.Query().Get("status"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status")
		return
	}
	limit := parseSessionsLimit(r.URL.Query().Get("limit"))

	rows, err := s.Queries.ListAgentTasksByUser(r.Context(), store.ListAgentTasksByUserParams{
		UserID: user.ID, Limit: int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed")
		return
	}
	views := make([]sessionView, 0, len(rows))
	for _, row := range rows {
		v := toSessionView(row)
		if status != "" && sessionStatusGroup(v.Status) != status {
			continue
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: views})
}

// handleExportSessions streams the same sessions handleListSessions would
// return as a downloadable CSV attachment — the Sessions page's "Export"
// button. A read-only GET: the response is Content-Disposition: attachment, so
// no CSRF header is required (same pattern as handleExportLogs). Unlike
// handleExportLogs (an unbounded read of the fixed-size in-memory ring
// buffer), this reads from the database, so the export is capped at
// maxSessionsLimit rather than unbounded — a runaway session history should
// not turn one export click into an unbounded query.
func (s *Server) handleExportSessions(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	status, ok := parseSessionStatus(r.URL.Query().Get("status"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status")
		return
	}

	rows, err := s.Queries.ListAgentTasksByUser(r.Context(), store.ListAgentTasksByUserParams{
		UserID: user.ID, Limit: maxSessionsLimit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed")
		return
	}

	name := fmt.Sprintf("proteos-sessions-%s.csv", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "machine", "status", "provider", "project", "prompt",
		"result_summary", "error", "created_at", "started_at", "ended_at",
	})
	for _, row := range rows {
		v := toSessionView(row)
		if status != "" && sessionStatusGroup(v.Status) != status {
			continue
		}
		_ = cw.Write([]string{
			csvSafe(v.ID), csvSafe(v.MachineName), csvSafe(v.Status), csvSafe(v.Provider),
			csvSafe(v.Project), csvSafe(v.Prompt), csvSafe(v.ResultSummary), csvSafe(v.Error),
			v.CreatedAt, v.StartedAt, v.EndedAt,
		})
	}
	cw.Flush()
}

// csvSafe guards against CSV/formula injection: a cell starting with
// =, +, -, or @ is interpreted as a formula by Excel/Sheets when the file is
// opened, so a session's free-text prompt/result/error (fully user-controlled)
// could otherwise execute attacker-chosen formulas on export. Prefixing with a
// tab neutralizes the leading character without changing the visible text (a
// leading single-quote is stripped by some spreadsheet apps on import, so a
// zero-width-safe prefix is used instead).
func csvSafe(field string) string {
	if field == "" {
		return field
	}
	switch field[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "\t" + field
	default:
		return field
	}
}

// parseSessionStatus validates the ?status query param: "" / "all" ⇒ no
// filter; "active" ⇒ queued/running; "finished" ⇒ done/failed/canceled;
// anything else is rejected.
func parseSessionStatus(raw string) (status string, ok bool) {
	switch raw {
	case "", "all":
		return "", true
	case "active", "finished":
		return raw, true
	default:
		return "", false
	}
}

// sessionStatusGroup maps a session's raw status onto the "active"/"finished"
// vocabulary ?status= filters on.
func sessionStatusGroup(status string) string {
	if isTerminalTaskStatus(status) {
		return "finished"
	}
	return "active"
}

func parseSessionsLimit(raw string) int {
	if raw == "" {
		return defaultSessionsLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultSessionsLimit
	}
	if n > maxSessionsLimit {
		return maxSessionsLimit
	}
	return n
}

func toSessionView(row store.ListAgentTasksByUserRow) sessionView {
	t := row.AgentTask
	v := sessionView{
		ID:             machine.UUIDString(t.ID),
		MachineID:      machine.UUIDString(t.MachineID),
		MachineName:    row.MachineName,
		Status:         t.Status,
		Provider:       t.Provider,
		Project:        t.Project,
		Prompt:         t.Prompt,
		AgentSessionID: t.AgentSessionID,
		ResultSummary:  t.ResultSummary,
		Error:          t.Error,
		CreatedAt:      tsString(t.CreatedAt),
		StartedAt:      tsString(t.StartedAt),
		EndedAt:        tsString(t.EndedAt),
	}
	if len(t.Usage) > 0 && string(t.Usage) != "{}" {
		v.Usage = json.RawMessage(t.Usage)
	}
	return v
}
