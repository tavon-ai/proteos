// Package applog captures recent Proteos application log lines — the control
// plane's own slog output ("api") and browser-reported UI errors/warnings
// ("ui") — in a bounded in-memory ring buffer that GET /api/logs reads (TAV-108).
// This is deliberately NOT a durable log store: it holds only the last
// `capacity` entries, reset on every restart. Durable API logs remain whatever
// they always were (stdout, captured by the container/systemd log driver); this
// package exists purely to give the desktop UI's Logs page something to read
// without depending on infrastructure (journald, docker logs) that varies by
// deployment. It never touches Firecracker/guest logs, which are a separate,
// per-machine concern.
package applog

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry is one captured log line.
type Entry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Source  string            `json:"source"` // "api" | "ui"
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Store is a fixed-capacity ring buffer of recent log entries, safe for
// concurrent use by the HTTP handlers and the slog Handler below.
type Store struct {
	mu       sync.Mutex
	entries  []Entry
	capacity int
	next     int // write index once full
	full     bool
}

// NewStore returns a Store holding at most capacity entries (oldest dropped
// first). capacity <= 0 falls back to a sane default.
func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = 1000
	}
	return &Store{capacity: capacity}
}

// Add appends an entry, evicting the oldest once capacity is reached.
func (s *Store) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) < s.capacity {
		s.entries = append(s.entries, e)
		return
	}
	s.entries[s.next] = e
	s.next = (s.next + 1) % s.capacity
	s.full = true
}

// List returns entries oldest-first, optionally filtered to one source ("" ⇒
// all) and capped to the most recent `limit` (limit <= 0 ⇒ unbounded).
func (s *Store) List(source string, limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := make([]Entry, 0, len(s.entries))
	if s.full {
		ordered = append(ordered, s.entries[s.next:]...)
		ordered = append(ordered, s.entries[:s.next]...)
	} else {
		ordered = append(ordered, s.entries...)
	}

	if source != "" {
		filtered := ordered[:0]
		for _, e := range ordered {
			if e.Source == source {
				filtered = append(filtered, e)
			}
		}
		ordered = filtered
	}
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[len(ordered)-limit:]
	}
	return ordered
}

// Handler is a slog.Handler that mirrors every record it sees into a Store
// (tagged with a fixed source) before delegating to the wrapped handler — so
// wrapping the process's real handler (the JSON stdout handler) with this adds
// in-memory capture without changing what actually gets written to stdout.
type Handler struct {
	next   slog.Handler
	store  *Store
	source string
}

// NewHandler wraps next, capturing every record it handles into store under
// source.
func NewHandler(next slog.Handler, store *Store, source string) *Handler {
	return &Handler{next: next, store: store, source: source}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	fields := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	h.store.Add(Entry{
		Time:    r.Time,
		Level:   r.Level.String(),
		Source:  h.source,
		Message: r.Message,
		Fields:  fields,
	})
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), store: h.store, source: h.source}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), store: h.store, source: h.source}
}
