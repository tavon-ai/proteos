// Package guestctl maintains the control plane's persistent control channel to
// each running guest (Phase 7 decision #1). It dials the guest's GET /control
// WebSocket over the existing node-agent byte tunnel (CP-initiated, so no
// guest-side transport credential is needed — the channel inherits the vsock
// topology-attested identity), maintains exactly one channel per running machine
// with backoff reconnect and teardown on stop, and is the single authorization
// choke point (decision #3) for guest → CP requests: every request is authorized
// by machine id (from the dial, never the payload) → owner user → allowed op.
package guestctl

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// reqHandler handles an inbound (guest → CP) request. It returns a response
// payload or a typed error payload, never both.
type reqHandler func(ctx context.Context, op string, payload json.RawMessage) (json.RawMessage, *guestwire.ControlErrorPayload)

// conn is one live control-channel RPC session over a single WebSocket. It
// multiplexes outbound requests (CP → guest, matched to responses by id) with
// inbound requests (guest → CP, dispatched to handler). Safe for concurrent use.
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

// run reads frames until the connection closes or ctx is cancelled.
func (c *conn) run(ctx context.Context) error {
	defer c.fail()
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var f guestwire.ControlFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Kind {
		case guestwire.ControlReq:
			go c.dispatch(ctx, f)
		case guestwire.ControlResp, guestwire.ControlErr:
			c.deliver(f)
		}
	}
}

func (c *conn) dispatch(ctx context.Context, f guestwire.ControlFrame) {
	if c.handler == nil {
		return
	}
	resp, errp := c.handler(ctx, f.Op, f.Payload)
	if errp != nil {
		c.writeFrame(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlErr, Payload: mustJSON(errp)})
		return
	}
	c.writeFrame(ctx, guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp, Payload: resp})
}

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

// request sends an op and blocks until the guest replies or ctx ends.
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

func (c *conn) writeFrame(ctx context.Context, f guestwire.ControlFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

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

// ControlError is returned by request when the guest answers with an err frame.
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

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"code":"unavailable"}`)
	}
	return b
}
