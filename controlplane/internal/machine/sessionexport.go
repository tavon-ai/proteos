package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// defaultSessionExportDir is used when Spec.SessionExportDir is unset. It
// mirrors config.Load's own default so a Service built by hand (tests, or any
// future caller that skips config.Load) still exports somewhere sane rather
// than silently landing in the process's working directory unexpectedly.
const defaultSessionExportDir = "./exports/sessions/"

// ErrSessionExportFailed indicates Destroy aborted before tearing the machine
// down because exporting its Claude coding-agent sessions (TAV-141) failed —
// or completed with some sessions failing — and the caller did not set
// force. Retrying with force=true bypasses the export outcome and deletes the
// machine anyway.
var ErrSessionExportFailed = errors.New("machine: session export failed")

// sessionExportRecord is the on-disk JSON shape for one exported session —
// the same fields the Sessions page's own API returns (httpapi.sessionView),
// so an exported file matches what the UI would have shown for that session
// while it still existed.
type sessionExportRecord struct {
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
	CreatedAt      string          `json:"created_at,omitempty"`
	StartedAt      string          `json:"started_at,omitempty"`
	EndedAt        string          `json:"ended_at,omitempty"`
}

// sessionExportResult reports the outcome of exporting one machine's sessions
// ahead of a destroy. Failed holds one "<session-id>: <reason>" entry per
// session that could not be written, so a caller blocking on export failure
// can tell the user exactly which sessions were not saved.
type sessionExportResult struct {
	Total    int
	Exported int
	Failed   []string
}

func (r sessionExportResult) ok() bool { return len(r.Failed) == 0 }

// summary renders a one-line human-readable outcome, used both for the error
// surfaced to the API/UI and for the warning logged when force bypasses it.
func (r sessionExportResult) summary() string {
	if r.ok() {
		return fmt.Sprintf("%d/%d sessions exported", r.Exported, r.Total)
	}
	return fmt.Sprintf("%d/%d sessions failed to export: %s", len(r.Failed), r.Total, strings.Join(r.Failed, "; "))
}

// exportSessions writes every one of m's coding-agent sessions to the
// configured export directory as "<machine-name>_<session-id>.json"
// (TAV-141), ahead of Destroy tearing the machine down. Each session is
// exported independently — one bad session (a marshal error or a failed
// write) does not stop the rest — so the aggregate result tells the caller
// exactly which ones, if any, failed. A machine with no sessions is a no-op
// (the export directory is not even created).
func (s *Service) exportSessions(ctx context.Context, m store.Machine) (sessionExportResult, error) {
	rows, err := s.q.ListAgentTasksByMachine(ctx, m.ID)
	if err != nil {
		return sessionExportResult{}, fmt.Errorf("list sessions: %w", err)
	}
	res := sessionExportResult{Total: len(rows)}
	if len(rows) == 0 {
		return res, nil
	}

	dir := s.spec.SessionExportDir
	if dir == "" {
		dir = defaultSessionExportDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("create export dir %q: %w", dir, err)
	}

	namePrefix := sanitizeFilenamePart(m.Name)
	for _, t := range rows {
		id := UUIDString(t.ID)
		rec := sessionExportRecord{
			ID:             id,
			MachineID:      UUIDString(t.MachineID),
			MachineName:    m.Name,
			Status:         t.Status,
			Provider:       t.Provider,
			Project:        t.Project,
			Prompt:         t.Prompt,
			AgentSessionID: t.AgentSessionID,
			ResultSummary:  t.ResultSummary,
			Error:          t.Error,
			CreatedAt:      formatTimestamp(t.CreatedAt),
			StartedAt:      formatTimestamp(t.StartedAt),
			EndedAt:        formatTimestamp(t.EndedAt),
		}
		if len(t.Usage) > 0 && string(t.Usage) != "{}" {
			rec.Usage = json.RawMessage(t.Usage)
		}
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("%s_%s.json", namePrefix, id))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Exported++
	}
	if len(res.Failed) > 0 {
		slog.Warn("session export: some sessions failed", "machine", UUIDString(m.ID), "failed", res.Failed)
	}
	return res, nil
}

// unsafeFilenameChars matches every rune that isn't safe to place unescaped in
// a file name.
var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFilenamePart collapses anything but [A-Za-z0-9._-] to "_" so a
// user-editable machine name (Rename has no character restrictions) can never
// escape the export directory (e.g. via ".." or "/") or collide with an
// unrelated path.
func sanitizeFilenamePart(s string) string {
	s = unsafeFilenameChars.ReplaceAllString(s, "_")
	if s == "" {
		return "machine"
	}
	return s
}

// formatTimestamp renders a nullable Postgres timestamp the same way the
// Sessions API does (httpapi.tsString): RFC 3339 in UTC, empty when unset.
func formatTimestamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
