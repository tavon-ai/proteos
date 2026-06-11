// Package httpapi serves the agentapi wire contract over HTTP/JSON, guarded by
// a constant-time bearer-token check. It is a thin shell over a driver.Driver:
// it parses requests, calls the driver, and maps driver results/errors onto the
// documented status codes.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
)

// Server wires the routes to a driver, holding the shared bearer token.
type Server struct {
	token []byte
	drv   driver.Driver
}

// New returns a Server authenticating with token against drv.
func New(token string, drv driver.Driver) *Server {
	return &Server{token: []byte(token), drv: drv}
}

// Handler builds the fully-wired http.Handler. /healthz is public; everything
// else is behind the bearer check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(api.RouteHealthz, s.handleHealthz)
	mux.Handle(api.RouteEnsure, s.auth(http.HandlerFunc(s.handleEnsure)))
	mux.Handle(api.RouteStop, s.auth(http.HandlerFunc(s.handleStop)))
	mux.Handle(api.RouteGetMachine, s.auth(http.HandlerFunc(s.handleGet)))
	mux.Handle(api.RouteListMachine, s.auth(http.HandlerFunc(s.handleList)))
	mux.Handle(api.RouteDestroy, s.auth(http.HandlerFunc(s.handleDestroy)))
	mux.Handle(api.RouteGuest, s.auth(http.HandlerFunc(s.handleGuest)))
	return requestLogger(mux)
}

// auth enforces the shared bearer token in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get(api.AuthHeader)
		if len(h) <= len(api.BearerPrefix) || h[:len(api.BearerPrefix)] != api.BearerPrefix {
			writeError(w, http.StatusUnauthorized, api.ErrUnauthorized)
			return
		}
		presented := []byte(h[len(api.BearerPrefix):])
		if subtle.ConstantTimeCompare(presented, s.token) != 1 {
			writeError(w, http.StatusUnauthorized, api.ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.HealthResponse{Status: "ok"})
}

func (s *Server) handleEnsure(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.EnsureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrBadRequest)
		return
	}
	handle, err := s.drv.EnsureRunning(r.Context(), driver.VMSpec{
		MachineID: id,
		Vcpus:     req.Vcpus,
		MemMiB:    req.MemMiB,
		KernelRef: req.KernelRef,
		RootfsRef: req.RootfsRef,
	})
	if err != nil {
		slog.Error("ensure failed", "machine", id, "err", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal)
		return
	}
	writeJSON(w, http.StatusAccepted, api.EnsureResponse{Handle: handle})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.drv.Stop(r.Context(), id); err != nil {
		s.writeDriverError(w, id, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, err := s.drv.Status(r.Context(), id)
	if err != nil {
		s.writeDriverError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, toWire(st))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	sts, err := s.drv.List(r.Context())
	if err != nil {
		slog.Error("list failed", "err", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal)
		return
	}
	out := api.ListResponse{Machines: make([]api.MachineStatus, 0, len(sts))}
	for _, st := range sts {
		out.Machines = append(out.Machines, toWire(st))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.drv.Destroy(r.Context(), id); err != nil {
		slog.Error("destroy failed", "machine", id, "err", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeDriverError maps ErrUnknownMachine to 404 and anything else to 500.
func (s *Server) writeDriverError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, driver.ErrUnknownMachine) {
		writeError(w, http.StatusNotFound, api.ErrUnknownMachine)
		return
	}
	slog.Error("driver error", "machine", id, "err", err)
	writeError(w, http.StatusInternalServerError, api.ErrInternal)
}

func toWire(st driver.Status) api.MachineStatus {
	return api.MachineStatus{
		MachineID: st.MachineID,
		State:     st.State,
		Reason:    st.Reason,
		Handle:    st.Handle,
		GuestIP:   st.GuestIP,
	}
}

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

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, api.ErrorResponse{Error: code})
}
