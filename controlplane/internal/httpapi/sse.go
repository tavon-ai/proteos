package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// sseHeartbeat is how often we emit an SSE comment to keep proxies/clients from
// timing out an idle stream.
const sseHeartbeat = 25 * time.Second

// machineEventJSON is the wire shape of a machine_events row.
type machineEventJSON struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	FromState *string         `json:"from_state"`
	ToState   *string         `json:"to_state"`
	Actor     string          `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

func toEventJSON(e store.MachineEvent) machineEventJSON {
	j := machineEventJSON{
		ID:        e.ID,
		Type:      e.Type,
		FromState: e.FromState,
		ToState:   e.ToState,
		Actor:     e.Actor,
		Payload:   json.RawMessage(e.Payload),
	}
	if e.CreatedAt.Valid {
		j.CreatedAt = e.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	return j
}

// snapshotData is the body of the initial `snapshot` event: all of the user's
// machines plus the most-recent events across them.
type snapshotData struct {
	Machines []MachineSummary   `json:"machines"`
	Events   []machineEventJSON `json:"events"`
}

// machineData is the body of each live `machine` event.
type machineData struct {
	Machine MachineSummary   `json:"machine"`
	Event   machineEventJSON `json:"event"`
}

// handleMachineEvents streams the user's machine state over Server-Sent Events:
// a `snapshot` on connect (machine + last 50 events), then live `machine`
// events with id: set to the event row id. A reconnect carrying Last-Event-ID
// replays the rows it missed straight from the DB before resuming the live
// stream, so no transition is ever lost. A heartbeat comment fires every 25s.
func (s *Server) handleMachineEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
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

	ctx := r.Context()

	// Subscribe BEFORE the snapshot/replay read so no event committed between
	// the read and the live loop is dropped (the live loop dedups by event id).
	updates, cancel := s.Broker.Subscribe()
	defer cancel()

	machines, err := s.Machines.List(ctx, user.ID)
	if err != nil {
		// Can't even read the machines; close the stream.
		return
	}
	// Index by id so replay can render each event's machine summary.
	byID := make(map[pgtype.UUID]store.Machine, len(machines))
	for _, m := range machines {
		byID[m.ID] = m
	}

	var lastSent int64
	if lastID := lastEventID(r); lastID > 0 {
		// Reconnect: replay everything after the client's last seen id.
		lastSent = s.replayAfter(ctx, w, flusher, user.ID, byID, lastID)
	} else {
		// Fresh connection: send the snapshot (all machines + recent events).
		lastSent = s.writeSnapshot(ctx, w, flusher, user.ID, machines)
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case u, ok := <-updates:
			if !ok {
				return
			}
			if !sameUser(u.Machine.UserID, user.ID) {
				continue
			}
			// A destroy carries no event row; emit a terminal `destroyed` frame so
			// the client drops the machine (the row is already gone).
			if u.Deleted {
				if writeSSE(w, "destroyed", "", map[string]string{"machine_id": machine.UUIDString(u.Machine.ID)}) != nil {
					return
				}
				flusher.Flush()
				continue
			}
			// Skip rows already replayed/snapshotted.
			if u.Event.ID <= lastSent {
				continue
			}
			if err := s.writeMachineEvent(ctx, w, u.Machine, u.Event); err != nil {
				return
			}
			flusher.Flush()
			lastSent = u.Event.ID
		}
	}
}

// writeSnapshot emits the `snapshot` event (all the user's machines + the most
// recent events across them) and returns the highest event id it included (so
// the live loop can skip duplicates).
func (s *Server) writeSnapshot(ctx context.Context, w http.ResponseWriter, f http.Flusher, userID pgtype.UUID, machines []store.Machine) int64 {
	data := snapshotData{Machines: make([]MachineSummary, 0, len(machines)), Events: []machineEventJSON{}}
	for _, m := range machines {
		data.Machines = append(data.Machines, s.summary(ctx, m))
	}
	var maxID int64
	if len(machines) > 0 {
		evs, err := s.Queries.ListUserMachineEventsRecent(ctx, store.ListUserMachineEventsRecentParams{UserID: userID, Limit: 50})
		if err == nil {
			for _, e := range evs {
				data.Events = append(data.Events, toEventJSON(e))
				if e.ID > maxID {
					maxID = e.ID
				}
			}
		}
	}
	_ = writeSSE(w, "snapshot", "", data)
	f.Flush()
	return maxID
}

// replayAfter streams every event after lastID (across all the user's machines)
// as a `machine` event, returning the highest id sent. byID maps machine id to
// the row whose summary is sent with each event.
func (s *Server) replayAfter(ctx context.Context, w http.ResponseWriter, f http.Flusher, userID pgtype.UUID, byID map[pgtype.UUID]store.Machine, lastID int64) int64 {
	evs, err := s.Queries.ListUserMachineEventsAfter(ctx, store.ListUserMachineEventsAfterParams{UserID: userID, ID: lastID})
	if err != nil {
		return lastID
	}
	maxID := lastID
	for _, e := range evs {
		m, ok := byID[e.MachineID]
		if !ok {
			// Machine was destroyed (its events cascade-deleted) between list and
			// now; skip — the client already saw/​will see the destroyed event.
			continue
		}
		if s.writeMachineEvent(ctx, w, m, e) != nil {
			break
		}
		maxID = e.ID
	}
	f.Flush()
	return maxID
}

// writeMachineEvent emits one `machine` SSE event with id: set to the row id.
func (s *Server) writeMachineEvent(ctx context.Context, w http.ResponseWriter, m store.Machine, e store.MachineEvent) error {
	return writeSSE(w, "machine", strconv.FormatInt(e.ID, 10), machineData{
		Machine: s.summary(ctx, m),
		Event:   toEventJSON(e),
	})
}

// writeSSE writes one event frame: optional id:, event:, and a single-line JSON
// data:. The blank line terminates the frame.
func writeSSE(w http.ResponseWriter, event, id string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// lastEventID returns the Last-Event-ID header as an int (0 if absent/invalid).
func lastEventID(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func sameUser(a, b pgtype.UUID) bool {
	return a.Valid && b.Valid && a.Bytes == b.Bytes
}
