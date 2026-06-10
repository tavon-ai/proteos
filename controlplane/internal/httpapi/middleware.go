package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// csrfHeaderName / csrfHeaderValue: state-changing requests must carry this
// header. Combined with SameSite=Lax this defeats cross-site form/POST CSRF
// without per-request token plumbing in the SPA.
const (
	csrfHeaderName  = "X-Requested-By"
	csrfHeaderValue = "proteos"
)

// ctxKey is the unexported type for context keys set by middleware.
type ctxKey int

const userCtxKey ctxKey = iota

// userFromContext returns the authenticated user attached by requireAuth.
func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// requireAuth rejects requests without a valid session cookie with 401 JSON,
// otherwise attaches the user to the request context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, err := s.Sessions.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
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
