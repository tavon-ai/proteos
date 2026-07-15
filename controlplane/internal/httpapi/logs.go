package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/applog"
)

// Limits for the logs API (TAV-108). maxLogsLimit/defaultLogsLimit bound how
// much of the ring buffer a single GET returns; maxUILogMessageLen/
// maxUILogFields bound what a browser session may report, so a runaway client
// can't blow up the shared in-memory store.
const (
	maxLogsLimit        = 2000
	defaultLogsLimit    = 500
	maxUILogMessageLen  = 2000
	maxUILogFieldCount  = 20
	maxUILogFieldLength = 2000
)

// handleListLogs returns the most recent captured Proteos application log
// entries: the control plane's own request/lifecycle logs (source "api") plus
// warn/error lines reported by browser sessions (source "ui", see
// handleReportUILog). Firecracker/guest logs are a separate per-machine concern
// and never appear here. ?source=api|ui filters to one source (default: both);
// ?limit caps the count (default 500, max 2000).
func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	source, ok := parseLogSource(r.URL.Query().Get("source"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_source")
		return
	}
	limit := parseLogLimit(r.URL.Query().Get("limit"))
	entries := s.Logs.List(source, limit)
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleExportLogs streams the same entries handleListLogs would return as a
// downloadable plain-text file (one line per entry, chronological) — the
// Logs page's "Export" button, for attaching a session's logs to a bug report.
// A read-only GET: the response is Content-Disposition: attachment, so no CSRF
// header is required (same pattern as handleProjectDownload).
func (s *Server) handleExportLogs(w http.ResponseWriter, r *http.Request) {
	source, ok := parseLogSource(r.URL.Query().Get("source"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_source")
		return
	}
	entries := s.Logs.List(source, 0)

	name := fmt.Sprintf("proteos-logs-%s.log", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	for _, e := range entries {
		fmt.Fprintln(w, formatLogLine(e))
	}
}

// formatLogLine renders one entry as a single greppable text line:
// "<RFC3339 time> [<source>] <LEVEL> <message> key=value ...".
func formatLogLine(e applog.Entry) string {
	line := fmt.Sprintf("%s [%s] %-5s %s", e.Time.UTC().Format(time.RFC3339Nano), e.Source, e.Level, e.Message)
	for k, v := range e.Fields {
		line += fmt.Sprintf(" %s=%s", k, v)
	}
	return line
}

// parseLogSource validates the ?source query param: "" / "all" ⇒ both sources
// (the empty string List() treats as unfiltered); "api" / "ui" ⇒ that source
// only; anything else is rejected.
func parseLogSource(raw string) (source string, ok bool) {
	switch raw {
	case "", "all":
		return "", true
	case "api", "ui":
		return raw, true
	default:
		return "", false
	}
}

func parseLogLimit(raw string) int {
	if raw == "" {
		return defaultLogsLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLogsLimit
	}
	if n > maxLogsLimit {
		return maxLogsLimit
	}
	return n
}

// uiLogInput is the body of POST /api/logs/ui: one structured record from the
// browser's logger (web/src/lib/logger.ts), the client-side counterpart to the
// server's own slog lines.
type uiLogInput struct {
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields"`
}

// handleReportUILog captures one browser-side log record into the same store
// GET /api/logs reads, tagged source "ui". The caller's GitHub login is
// attached as a field so a report can be traced back to a user without trusting
// the browser to self-identify. Best-effort: the client fires this and does not
// treat a failure as user-visible (a dropped log report must never itself
// surface as an error).
func (s *Server) handleReportUILog(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var in uiLogInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json")
		return
	}
	msg := in.Message
	if msg == "" {
		writeError(w, http.StatusBadRequest, "empty_message")
		return
	}
	if len(msg) > maxUILogMessageLen {
		msg = msg[:maxUILogMessageLen]
	}

	fields := make(map[string]string, len(in.Fields)+1)
	fields["user"] = user.Login
	i := 0
	for k, v := range in.Fields {
		if i >= maxUILogFieldCount {
			break
		}
		if len(v) > maxUILogFieldLength {
			v = v[:maxUILogFieldLength]
		}
		fields[k] = v
		i++
	}

	s.Logs.Add(applog.Entry{
		Time:    time.Now(),
		Level:   normalizeUILogLevel(in.Level),
		Source:  "ui",
		Message: msg,
		Fields:  fields,
	})
	writeJSON(w, http.StatusNoContent, nil)
}

// normalizeUILogLevel maps the browser logger's lowercase levels onto the same
// uppercase vocabulary slog uses server-side, so the two sources read
// consistently in the merged view. Anything unrecognized defaults to INFO.
func normalizeUILogLevel(raw string) string {
	switch raw {
	case "debug":
		return "DEBUG"
	case "info":
		return "INFO"
	case "warn":
		return "WARN"
	case "error":
		return "ERROR"
	default:
		return "INFO"
	}
}
