// Package taskevents is the in-process fan-out for headless agent-task event
// streams (AT2). As the guest relays normalized agent.event frames over the
// control channel, the CP publishes them here; the task SSE endpoint subscribes
// per task to deliver a snapshot + live tail with Last-Event-ID replay.
//
// It is the task-scoped sibling of machine.Broker. Unlike the machine event
// stream (DB-backed, audit-grade), agent events are deliberately ephemeral and
// bounded: each task keeps only the most recent BufferSize events in a ring, and
// a finished task's stream is reaped after a short retention window. A client
// that reconnects past the window still gets the authoritative outcome from
// GET .../tasks/{tid} (DB), so nothing important is lost.
package taskevents

import (
	"encoding/json"
	"sync"
	"time"
)

// Defaults. BufferSize bounds the per-task ring; Retention is how long a
// terminal task's stream lingers (for a late reconnect) before it is reaped.
const (
	DefaultBufferSize = 500
	DefaultRetention  = 5 * time.Minute
)

// Frame is one published event: a per-task monotonic Seq (the SSE id) and the
// already-normalized JSON payload. Terminal marks the run's final frame, after
// which subscribers close.
type Frame struct {
	Seq      int64
	Data     json.RawMessage
	Terminal bool
}

// Hub fans out Frames per task id.
type Hub struct {
	bufSize   int
	retention time.Duration

	mu    sync.Mutex
	tasks map[string]*stream
}

// stream is one task's ring buffer plus its live subscribers.
type stream struct {
	seq       int64
	buf       []Frame // ring of the most recent bufSize frames (ascending Seq)
	truncated bool    // a frame has been dropped from the front (logged once)
	terminal  bool
	subs      map[int]chan Frame
	nextSub   int
	reaper    *time.Timer
}

// New builds a Hub. A non-positive bufSize/retention falls back to the defaults.
func New(bufSize int, retention time.Duration) *Hub {
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Hub{bufSize: bufSize, retention: retention, tasks: map[string]*stream{}}
}

// Publish appends one normalized event to a task's stream, assigns its Seq, and
// fans it out to live subscribers. terminal marks the run's last frame. It
// returns whether the ring dropped an older frame (truncation) for the first
// time, so the caller can log it once per task.
func (h *Hub) Publish(taskID string, data json.RawMessage, terminal bool) (firstTruncation bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.tasks[taskID]
	if st == nil {
		st = &stream{subs: map[int]chan Frame{}}
		h.tasks[taskID] = st
	}
	st.seq++
	f := Frame{Seq: st.seq, Data: data, Terminal: terminal}

	st.buf = append(st.buf, f)
	if len(st.buf) > h.bufSize {
		st.buf = st.buf[len(st.buf)-h.bufSize:]
		if !st.truncated {
			st.truncated = true
			firstTruncation = true
		}
	}

	for _, ch := range st.subs {
		select {
		case ch <- f:
		default: // slow subscriber: it recovers the gap via Last-Event-ID on reconnect
		}
	}

	if terminal {
		st.terminal = true
		h.scheduleReap(taskID, st)
	}
	return firstTruncation
}

// Subscribe registers a live subscriber for a task and returns the backlog of
// frames with Seq > afterSeq (the snapshot, or the missed tail on reconnect),
// the live channel, a cancel func, and whether the run has already ended
// (terminal). When terminal is true the backlog already includes the final
// frame and the caller should drain the backlog and close without waiting.
func (h *Hub) Subscribe(taskID string, afterSeq int64) (backlog []Frame, ch <-chan Frame, cancel func(), terminal bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.tasks[taskID]
	if st == nil {
		// No stream yet (run not started, or never produced an event in this CP
		// process). Create one so live events still flow to this subscriber.
		st = &stream{subs: map[int]chan Frame{}}
		h.tasks[taskID] = st
	}
	// A terminal stream pending reap is being read again: keep it alive.
	if st.reaper != nil {
		st.reaper.Stop()
		st.reaper = nil
	}

	for _, f := range st.buf {
		if f.Seq > afterSeq {
			backlog = append(backlog, f)
		}
	}

	live := make(chan Frame, 64)
	id := st.nextSub
	st.nextSub++
	st.subs[id] = live

	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur := h.tasks[taskID]; cur == st {
			delete(st.subs, id)
			close(live)
			if st.terminal && len(st.subs) == 0 {
				h.scheduleReap(taskID, st)
			}
		}
	}
	return backlog, live, cancel, st.terminal
}

// scheduleReap deletes a terminal stream after the retention window unless a new
// subscriber arrives first. Caller holds h.mu.
func (h *Hub) scheduleReap(taskID string, st *stream) {
	if st.reaper != nil {
		st.reaper.Stop()
	}
	st.reaper = time.AfterFunc(h.retention, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur := h.tasks[taskID]; cur == st && len(st.subs) == 0 {
			delete(h.tasks, taskID)
		}
	})
}
