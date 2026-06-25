package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
)

// handleTaskEvents streams one headless task's normalized agent events over SSE
// (AT2). On connect it replays the in-memory ring (snapshot, or just the tail
// past Last-Event-ID on reconnect), then tails live frames until the run's
// terminal `result` frame or the client disconnects. A heartbeat comment fires
// every 25s. Each frame is an `agent` event with id: set to the per-task seq, so
// the browser EventSource resumes cleanly via Last-Event-ID.
//
// If the run already finished before this CP process built the stream (e.g. a CP
// restart, or a late reconnect past the retention window), the live channel will
// never deliver a terminal frame, so the handler synthesizes the final `result`
// from the persisted task row and closes.
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	tid, err := machine.ParseUUID(r.PathValue("tid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	task, err := s.Queries.GetAgentTask(r.Context(), tid)
	if err != nil || machine.UUIDString(task.MachineID) != machine.UUIDString(mc.ID) {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	taskID := machine.UUIDString(task.ID)
	lastSeq := lastEventID(r)

	backlog, ch, cancel, terminal := s.TaskEvents.Subscribe(taskID, lastSeq)
	defer cancel()

	var sentSeq int64 = lastSeq
	for _, f := range backlog {
		if writeTaskFrame(w, f) != nil {
			return
		}
		sentSeq = f.Seq
	}
	flusher.Flush()

	// The backlog already carried the terminal frame — nothing more will come.
	if terminal {
		return
	}
	// The run ended before this process streamed its events: the live channel is
	// silent, so emit the authoritative result from the row and close.
	if isTerminalTaskStatus(task.Status) {
		if writeSynthTaskResult(w, task, sentSeq+1) == nil {
			flusher.Flush()
		}
		return
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case f, ok := <-ch:
			if !ok {
				return
			}
			if f.Seq <= sentSeq { // already replayed from the backlog
				continue
			}
			if writeTaskFrame(w, f) != nil {
				return
			}
			flusher.Flush()
			sentSeq = f.Seq
			if f.Terminal {
				return
			}
		}
	}
}

// isTerminalTaskStatus reports whether a task status is a final state.
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "done", "failed", "canceled":
		return true
	default:
		return false
	}
}

// writeTaskFrame emits one `agent` SSE event with id: set to the frame seq and
// the normalized event JSON as the data payload.
func writeTaskFrame(w http.ResponseWriter, f taskevents.Frame) error {
	return writeSSE(w, "agent", strconv.FormatInt(f.Seq, 10), f.Data)
}

// writeSynthTaskResult emits a terminal `result` frame reconstructed from the
// persisted task row, for clients that connect after the live stream is gone.
func writeSynthTaskResult(w http.ResponseWriter, t store.AgentTask, seq int64) error {
	data := map[string]any{
		"kind":     "result",
		"status":   t.Status,
		"is_error": t.Status == "failed",
		"text":     t.ResultSummary,
	}
	if t.Error != "" {
		data["error"] = t.Error
	}
	if len(t.Usage) > 0 && string(t.Usage) != "{}" {
		var usage map[string]any
		if json.Unmarshal(t.Usage, &usage) == nil {
			for k, v := range usage {
				data[k] = v
			}
		}
	}
	return writeSSE(w, "agent", strconv.FormatInt(seq, 10), data)
}
