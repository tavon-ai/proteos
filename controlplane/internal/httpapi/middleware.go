package httpapi

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// errNotHijacker is returned when the underlying ResponseWriter cannot be
// hijacked (e.g. HTTP/2). The terminal gateway needs raw connection access for
// the WebSocket upgrade, so it surfaces this as an internal error.
var errNotHijacker = errors.New("response writer does not support hijacking")

// csrfHeaderName / csrfHeaderValue: state-changing requests must carry this
// header. Combined with SameSite=Lax this defeats cross-site form/POST CSRF
// without per-request token plumbing in the SPA.
const (
	csrfHeaderName  = "X-Requested-By"
	csrfHeaderValue = "proteos"
)

// ctxKey is the unexported type for context keys set by middleware.
type ctxKey int

const (
	userCtxKey ctxKey = iota
	sessionIDCtxKey
)

// userFromContext returns the authenticated user attached by requireAuth.
func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// sessionIDFromContext returns the authenticated session id (canonical UUID
// string) attached by requireAuth. The gateway uses it to register live
// terminal connections for revocation.
func sessionIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(sessionIDCtxKey).(string)
	return id, ok
}

// requireAuth rejects requests without a valid session cookie with 401 JSON,
// otherwise attaches the user and session id to the request context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, sess, err := s.Sessions.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		ctx = context.WithValue(ctx, sessionIDCtxKey, session.SessionIDString(sess.ID))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrfHeader rejects state-changing requests lacking the X-Requested-By header.
func (s *Server) csrfHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeaderName) != csrfHeaderValue {
			writeError(w, http.StatusForbidden, "csrf_required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack delegates to the underlying ResponseWriter so the request logger does
// not hide http.Hijacker from the terminal gateway, which needs to take over
// the connection for the WebSocket upgrade.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errNotHijacker
	}
	return hj.Hijack()
}

// Flush delegates to the underlying ResponseWriter's flusher so that wrapping
// in the request logger does not break Server-Sent Events (the SSE handler
// type-asserts http.Flusher).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLogger logs method, path, status, and duration. It deliberately never
// logs cookies, the Authorization header, or query strings that may carry the
// OAuth code/state.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}
