package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Event is one normalized agent-task event from the task SSE stream. Different
// kinds populate different fields:
//   - assistant_text: Text
//   - tool_use:       Tool, ToolID, Input
//   - tool_result:    ToolID, Output, IsError
//   - result:         Status, CostUSD, NumTurns, DurationMS, Error (terminal)
type Event struct {
	Kind       string          `json:"kind"`
	Text       string          `json:"text,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	ToolID     string          `json:"tool_id,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     string          `json:"output,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Status     string          `json:"status,omitempty"`
	CostUSD    float64         `json:"cost_usd,omitempty"`
	NumTurns   int             `json:"num_turns,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// EventHandler is invoked for each event with its SSE id. Returning an error
// stops streaming and is propagated by StreamEvents.
type EventHandler func(id string, ev Event) error

// stopError wraps a handler error so StreamEvents can distinguish "the caller
// asked to stop" from a transient connection failure (which is retried).
type stopError struct{ err error }

func (e *stopError) Error() string { return e.err.Error() }
func (e *stopError) Unwrap() error { return e.err }

// StreamEvents streams a task's events, reconnecting with Last-Event-ID on
// transient drops, until the terminal `result` event arrives, the handler
// returns an error, or ctx is done. startID is the initial Last-Event-ID ("" to
// begin from the server's snapshot).
func (c *Client) StreamEvents(ctx context.Context, machineID, taskID, startID string, h EventHandler) error {
	lastID := startID
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 10 * time.Second
	backoff := baseBackoff

	for {
		terminal, id, err := c.streamOnce(ctx, machineID, taskID, lastID, h)
		if id != "" {
			lastID = id
		}
		if terminal {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if se, ok := errors.AsType[*stopError](err); ok {
			return se.err
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Clean EOF with no terminal frame: reconnect promptly.
		backoff = baseBackoff
	}
}

// streamOnce holds a single SSE connection. It returns terminal=true when a
// `result` event is delivered, the last-seen event id (for reconnect), and a
// connection error (or a *stopError wrapping a handler error).
func (c *Client) streamOnce(ctx context.Context, machineID, taskID, lastID string, h EventHandler) (bool, string, error) {
	req, err := c.NewRequest(ctx, "GET", "/api/machines/"+machineID+"/tasks/"+taskID+"/events", nil)
	if err != nil {
		return false, lastID, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	// SSE connections must not be subject to the client's global Timeout, which
	// applies to the entire response-body read and would kill any stream longer
	// than 30 s. Context cancellation on the request already owns the deadline.
	sseHTTP := *c.HTTP
	sseHTTP.Timeout = 0
	resp, err := sseHTTP.Do(req)
	if err != nil {
		return false, lastID, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// An auth/not-found error is terminal — do not retry it.
		ae := decodeAPIError(resp)
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound {
			return false, lastID, &stopError{err: ae}
		}
		return false, lastID, ae
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var data strings.Builder
	curID := lastID

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// Dispatch the accumulated event.
			if data.Len() == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err == nil {
				if herr := h(curID, ev); herr != nil {
					return false, curID, &stopError{err: herr}
				}
				if ev.Kind == "result" {
					return true, curID, nil
				}
			}
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// Comment / heartbeat — ignore.
		case strings.HasPrefix(line, "id:"):
			curID = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[len("data:"):], " "))
		default:
			// event:/retry:/unknown fields — ignore.
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// An oversized frame can never be read on reconnect either — the server
			// would replay the same frame with Last-Event-ID, looping forever. Treat
			// it as a terminal stop instead of a transient reconnectable error.
			return false, curID, &stopError{err: fmt.Errorf("sse frame too large: %w", err)}
		}
		return false, curID, fmt.Errorf("read stream: %w", err)
	}
	return false, curID, nil
}
