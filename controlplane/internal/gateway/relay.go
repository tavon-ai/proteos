package gateway

import (
	"context"
	"errors"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// side identifies which leg of the relay ended first.
type side int

const (
	sideBrowser side = iota
	sideGuest
)

// relayResult reports which leg ended and with what error (a *websocket.
// CloseError when the peer sent a close frame).
type relayResult struct {
	side side
	err  error
}

// relay pumps messages 1:1 in both directions between the browser and guest
// WebSockets, preserving message type (binary PTY bytes stay binary, JSON
// control frames stay text), until either side ends. It returns which side
// ended and its error so the caller can map an appropriate browser close code.
//
// The gateway deliberately relays *messages*, not a raw byte splice: that keeps
// the framing intact across the two independent WebSocket connections and lets
// the gateway own the browser-facing close codes.
func relay(ctx context.Context, browser, guest *websocket.Conn) relayResult {
	done := make(chan relayResult, 2)
	go func() { done <- relayResult{sideBrowser, copyMessages(ctx, browser, guest)} }()
	go func() { done <- relayResult{sideGuest, copyMessages(ctx, guest, browser)} }()
	return <-done
}

// copyMessages forwards every message from src to dst until a read or write
// error occurs, which it returns.
func copyMessages(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}

// browserCloseFor maps a relay outcome onto the close code the gateway sends to
// the browser. running reports whether the machine is still running, used to
// distinguish a clean shell exit / transient fault from the machine stopping
// out from under the session.
func browserCloseFor(res relayResult, running bool) (websocket.StatusCode, string) {
	if res.side == sideBrowser {
		// The browser closed first (navigated away / network dropped). Nothing
		// to tell it; close normally so we tear down the guest leg cleanly.
		return websocket.StatusNormalClosure, ""
	}
	// The guest leg ended. A normal closure means the shell exited (the guest
	// sent an exit frame, then closed 1000) — propagate it verbatim.
	if websocket.CloseStatus(res.err) == websocket.StatusNormalClosure {
		return websocket.StatusNormalClosure, ""
	}
	// A provider-unavailable close is the guest intentionally refusing an agent
	// session (provider not injected, or its setup_command failed/degraded). Its
	// reason (Phase 6: not-injected vs setup_failed) is actionable, so propagate
	// the code and reason verbatim instead of masking it as an internal fault.
	if websocket.CloseStatus(res.err) == guestwire.CloseProviderUnavailable {
		var ce websocket.CloseError
		if errors.As(res.err, &ce) {
			return ce.Code, ce.Reason
		}
		return guestwire.CloseProviderUnavailable, guestwire.CloseReasonNotInjected
	}
	// Otherwise the tunnel/guest dropped unexpectedly: if the machine is no
	// longer running it stopped under us; else it is an internal fault.
	if !running {
		return guestwire.CloseMachineStopped, "machine_stopped"
	}
	return guestwire.CloseInternal, "internal"
}
