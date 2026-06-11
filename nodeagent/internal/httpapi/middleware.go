package httpapi

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// errNotHijacker is returned when the underlying ResponseWriter cannot be
// hijacked (e.g. an HTTP/2 connection). The guest tunnel needs raw connection
// access, so it surfaces this as a 500.
var errNotHijacker = errors.New("response writer does not support hijacking")

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack lets the guest-tunnel handler take over the raw connection for its
// bidirectional byte splice. Without this passthrough the wrapped writer would
// not satisfy http.Hijacker and the upgrade would 500.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errNotHijacker
	}
	return hj.Hijack()
}

// requestLogger logs method, path, status, and duration. It never logs the
// Authorization header.
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
