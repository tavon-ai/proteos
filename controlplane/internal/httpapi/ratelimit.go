package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a single per-key token-bucket cell. Tokens refill continuously
// at rate tokens/second up to capacity. Each Allow call consumes one token; a
// call when the bucket is empty returns false.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	last     time.Time
}

func newBucket(capacity, rate float64) *tokenBucket {
	return &tokenBucket{tokens: capacity, capacity: capacity, rate: rate, last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	if add := elapsed * b.rate; add > 0 {
		if b.tokens+add > b.capacity {
			b.tokens = b.capacity
		} else {
			b.tokens += add
		}
	}
	if b.tokens < 1.0 {
		return false
	}
	b.tokens--
	return true
}

// Limiter is a concurrent per-key token-bucket rate limiter. Each distinct key
// maintains its own bucket; keys are arbitrary strings (IP addresses, user IDs).
// A nil Limiter passes all requests through.
type Limiter struct {
	buckets  sync.Map
	capacity float64
	rate     float64
}

// NewLimiter returns a Limiter that allows up to capacity requests in a burst,
// then refills at ratePerSec tokens per second.
func NewLimiter(capacity, ratePerSec float64) *Limiter {
	return &Limiter{capacity: capacity, rate: ratePerSec}
}

// Allow reports whether key's bucket has a token. False means over-limit.
func (l *Limiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	v, _ := l.buckets.LoadOrStore(key, newBucket(l.capacity, l.rate))
	return v.(*tokenBucket).allow()
}

// clientIP extracts the best-effort client IP from a request, preferring
// X-Real-IP, then the first value of X-Forwarded-For, then RemoteAddr.
func clientIP(r *http.Request) string {
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// "client, proxy1, proxy2" — take the leftmost (original client).
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// ipLimit returns middleware that rate-limits by client IP using l. A nil l is
// a no-op. Exceeding the limit returns 429 rate_limited.
func (s *Server) ipLimit(l *Limiter, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// userLimit returns middleware that rate-limits by authenticated user ID using
// l. It must run after requireAuth (user must be in context). A nil l is a
// no-op. Exceeding the limit returns 429 rate_limited.
func (s *Server) userLimit(l *Limiter, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r) // requireAuth handles the 401 path
			return
		}
		if !l.Allow(uuidString(user.ID)) {
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}
