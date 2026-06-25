// Package ctlchan implements the guest side of the Phase 7 control channel: the
// GET /control WebSocket the control plane dials, plus the credential lookup the
// in-VM git credential helper relies on. The guest is a pure server here — it
// never opens the connection (Phase 7 decision #1), so there is no guest-side
// transport credential to manage. Frames are JSON request/response pairs
// (guestwire.ControlFrame), bidirectional: the CP sends git.configure/git.clone,
// the guest sends git.credential/git.clone.done.
package ctlchan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// reqHandler handles an inbound (CP → guest) request frame. It returns either a
// response payload or a typed error payload — never both. A nil response with a
// nil error payload is sent as an empty resp.
type reqHandler func(ctx context.Context, op string, payload json.RawMessage) (json.RawMessage, *guestwire.ControlErrorPayload)

// ControlError is the error returned by conn.request when the peer answers with
// an err frame. Code is one of the guestwire.ErrCode* values.
type ControlError struct {
	Code    string
	Message string
}

func (e *ControlError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// ErrorCode exposes the machine-readable code so the local helper socket can
// relay it verbatim to the git credential helper.
func (e *ControlError) ErrorCode() string { return e.Code }

// conn is one live control-channel RPC session over a single WebSocket. It
// multiplexes outbound requests (matched to responses by id) with inbound
// requests (dispatched to handler). It is safe for concurrent use.
type conn struct {
	ws      *websocket.Conn
	handler reqHandler

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	waiters map[int64]chan guestwire.ControlFrame
	closed  bool
}

func newConn(ws *websocket.Conn, handler reqHandler) *conn {
	return &conn{ws: ws, handler: handler, waiters: map[int64]chan guestwire.ControlFrame{}}
}

// run reads frames until the connection closes or ctx is cancelled. Inbound
// requests are dispatched on their own goroutines so a slow handler never stalls
// the read loop; responses are routed to the waiting requester.
func (c *conn) run(ctx context.Context) error {
	defer c.fail()
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue // control frames are JSON text; ignore anything else
		}
		var f guestwire.ControlFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue // ignore malformed frames
		}
		switch f.Kind {
		case guestwire.ControlReq:
			go c.dispatch(ctx, f)
		case guestwire.ControlResp, guestwire.ControlErr:
			c.deliver(f)
		}
	}
}

// dispatch runs the handler for an inbound request and writes the reply, unless
// the op is a notification (no reply expected, e.g. nothing on the guest side).
func (c *conn) dispatch(ctx context.Context, f guestwire.ControlFrame) {
	if c.handler == nil {
		c.writeFrame(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlErr,
			Payload: mustJSON(guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "no handler"})})
		return
	}
	resp, errp := c.handler(ctx, f.Op, f.Payload)
	if errp != nil {
		c.writeFrame(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlErr, Payload: mustJSON(errp)})
		return
	}
	c.writeFrame(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp, Payload: resp})
}

// deliver routes a response/error frame to its waiter, if any. Frames for
// unknown ids (e.g. a notification the peer never waits on) are dropped.
func (c *conn) deliver(f guestwire.ControlFrame) {
	c.mu.Lock()
	ch, ok := c.waiters[f.ID]
	if ok {
		delete(c.waiters, f.ID)
	}
	c.mu.Unlock()
	if ok {
		ch <- f
	}
}

// request sends an op with payload and blocks until the peer replies or ctx ends.
func (c *conn) request(ctx context.Context, op string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("control channel closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan guestwire.ControlFrame, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	if err := c.writeFrame(ctx, guestwire.ControlFrame{ID: id, Kind: guestwire.ControlReq, Op: op, Payload: body}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case f, ok := <-ch:
		if !ok {
			return nil, errors.New("control channel closed before reply")
		}
		if f.Kind == guestwire.ControlErr {
			var ep guestwire.ControlErrorPayload
			_ = json.Unmarshal(f.Payload, &ep)
			return nil, &ControlError{Code: ep.Code, Message: ep.Message}
		}
		return f.Payload, nil
	}
}

// notify sends a one-way request the peer does not reply to (git.clone.done).
func (c *conn) notify(ctx context.Context, op string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return c.writeFrame(ctx, guestwire.ControlFrame{ID: id, Kind: guestwire.ControlReq, Op: op, Payload: body})
}

func (c *conn) writeFrame(ctx context.Context, f guestwire.ControlFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// fail aborts all in-flight requests so callers unblock when the channel drops.
func (c *conn) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.waiters {
		close(ch)
		delete(c.waiters, id)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(fmt.Sprintf("%q", err.Error()))
	}
	return b
}
