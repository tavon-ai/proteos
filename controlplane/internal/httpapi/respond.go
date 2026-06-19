package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorEnvelope is the consistent JSON error shape returned by every endpoint:
// {"error": "<machine_readable_code>"}. Detail is an optional human-readable
// elaboration (e.g. which resource was out of range), omitted when empty.
type errorEnvelope struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// writeJSON serializes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON encode failed", "err", err)
	}
}

// writeError emits the standard {"error": code} envelope with the given status.
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorEnvelope{Error: code})
}

// writeErrorDetail emits {"error": code, "detail": detail} with the given status,
// for errors that carry a human-readable elaboration.
func writeErrorDetail(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorEnvelope{Error: code, Detail: detail})
}
