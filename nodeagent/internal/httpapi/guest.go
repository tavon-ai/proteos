package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
)

// handleGuest opens an opaque byte tunnel between the caller (the control-plane
// gateway) and the machine's in-guest agent. It is the node-agent's whole role
// in the terminal path: authenticate (bearer, via the auth wrapper), verify the
// machine is running, dial the guest, then hijack and splice bytes both ways.
// It never inspects what flows through — gateway and guest speak WebSocket to
// each other across this pipe (decision #4).
func (s *Server) handleGuest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// The caller must request our private upgrade protocol.
	if !headerHasToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), api.UpgradeGuestProto) {
		writeError(w, http.StatusBadRequest, api.ErrBadRequest)
		return
	}

	// Machine must exist (404) and be running (409) before we dial.
	st, err := s.drv.Status(r.Context(), id)
	if err != nil {
		s.writeDriverError(w, id, err)
		return
	}
	if st.State != api.StateRunning {
		writeError(w, http.StatusConflict, api.ErrNotRunning)
		return
	}

	dialer, ok := s.drv.(driver.GuestDialer)
	if !ok {
		writeError(w, http.StatusBadGateway, api.ErrGuestUnreachable)
		return
	}
	guestConn, err := dialer.DialGuest(r.Context(), id)
	if err != nil {
		if errors.Is(err, driver.ErrUnknownMachine) {
			writeError(w, http.StatusNotFound, api.ErrUnknownMachine)
			return
		}
		slog.Warn("guest dial failed", "machine", id, "err", err)
		writeError(w, http.StatusBadGateway, api.ErrGuestUnreachable)
		return
	}
	defer guestConn.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, api.ErrInternal)
		return
	}
	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		slog.Error("hijack failed", "machine", id, "err", err)
		return
	}
	defer clientConn.Close()

	// Switch protocols, then become a transparent pipe.
	if _, err := bufrw.WriteString(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: " + api.UpgradeGuestProto + "\r\n\r\n"); err != nil {
		return
	}
	if err := bufrw.Flush(); err != nil {
		return
	}

	bridge(clientConn, bufrw, guestConn)
}

// bridge copies bytes both ways between the hijacked client connection and the
// guest connection until either side closes, then tears the other down. The
// client side is read through bufrw (which may hold bytes buffered during the
// handshake) and written to via the raw conn.
func bridge(clientConn net.Conn, clientRead io.Reader, guestConn net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(guestConn, clientRead); done <- struct{}{} }()
	go func() { _, _ = io.Copy(clientConn, guestConn); done <- struct{}{} }()

	<-done
	// One direction ended; closing both unblocks the other copy.
	_ = clientConn.Close()
	_ = guestConn.Close()
	<-done
}

// headerHasToken reports whether a comma-separated header (e.g. Connection)
// contains token, case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
