// Package httpapi wires the control-plane HTTP routes, middleware, and handlers
// together. The router is built with the stdlib net/http 1.22 pattern syntax;
// no framework is involved.
package httpapi

import (
	"net/http"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Sessions *session.Manager
	Auth     *auth.Handler

	// Machines drives the machine lifecycle (Phase 2). Required by /api/me and
	// the /api/machine routes.
	Machines *machine.Service

	// Broker and Queries back the SSE endpoint (snapshot + replay + live
	// stream). Queries is the read-only side used for the snapshot/replay.
	Broker  *machine.Broker
	Queries *store.Queries
}

// Handler builds the fully-wired http.Handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness probe — unauthenticated, no logging noise needed but harmless.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Auth flow (public).
	if s.Auth != nil {
		mux.HandleFunc("GET /api/auth/github/login", s.Auth.Login)
		mux.HandleFunc("GET /api/auth/github/callback", s.Auth.Callback)
		// Logout mutates state, so it requires the CSRF header and a session.
		mux.Handle("POST /api/auth/logout", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.Auth.Logout))))
	}

	// Current user (authenticated).
	mux.Handle("GET /api/me", s.requireAuth(http.HandlerFunc(s.handleMe)))

	// Machine routes (Phase 2). Reads are auth-only; mutations also require the
	// CSRF header. The SSE stream is a GET (no CSRF) — EventSource cannot set
	// custom headers, and it is read-only.
	mux.Handle("GET /api/machine", s.requireAuth(http.HandlerFunc(s.handleGetMachine)))
	mux.Handle("POST /api/machine", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleCreateMachine))))
	mux.Handle("POST /api/machine/start", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleStartMachine))))
	mux.Handle("POST /api/machine/stop", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleStopMachine))))
	mux.Handle("GET /api/machine/events", s.requireAuth(http.HandlerFunc(s.handleMachineEvents)))

	// Wrap everything in request logging.
	return requestLogger(mux)
}
