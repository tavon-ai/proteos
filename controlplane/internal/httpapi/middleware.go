package httpapi

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
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
	tokenAuthCtxKey
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

// requireAuth authenticates a request by either an Authorization: Bearer
// personal access token (CLI / programmatic callers) or the browser session
// cookie, and attaches the user to the request context. A Bearer token, when
// present, takes precedence and is authoritative — an invalid one is rejected
// rather than silently falling back to the cookie. Token-authenticated requests
// are marked so csrfHeader can exempt them (a bearer token is not an ambient
// browser credential, so the CSRF header is unnecessary).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearerToken(r); tok != "" {
			if s.PATs == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			user, _, err := s.PATs.Authenticate(r.Context(), tok)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			ctx := context.WithValue(r.Context(), userCtxKey, user)
			ctx = context.WithValue(ctx, tokenAuthCtxKey, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

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

// csrfHeader rejects state-changing cookie-authenticated requests lacking the
// X-Requested-By header. Bearer-token requests are exempt: the token is not sent
// automatically by a browser, so cross-site forgery does not apply.
func (s *Server) csrfHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isTokenAuth(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(csrfHeaderName) != csrfHeaderValue {
			writeError(w, http.StatusForbidden, "csrf_required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the credential from an "Authorization: Bearer <token>"
// header, or "" if absent/malformed.
func bearerToken(r *http.Request) string {
	const scheme = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		return strings.TrimSpace(h[len(scheme):])
	}
	return ""
}

// isTokenAuth reports whether the request was authenticated by a bearer token.
func isTokenAuth(ctx context.Context) bool {
	v, _ := ctx.Value(tokenAuthCtxKey).(bool)
	return v
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

// recoverer catches panics in handlers, logs them with a stack trace, and
// returns 500 so the server stays up rather than crashing on an unhandled panic.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in http handler", "recover", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal")
			}
		}()
		next.ServeHTTP(w, r)
	})
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
