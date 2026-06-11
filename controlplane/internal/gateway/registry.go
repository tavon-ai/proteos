package gateway

import "sync"

// Registry tracks live terminal connections by the session id that authorized
// them, so a session revocation (logout, admin revoke) can immediately close
// every WebSocket it opened (Phase 3 decision #9). It implements
// session.RevocationListener via SessionRevoked.
//
// This is exact and immediate for a single control-plane instance. Multi-
// instance fan-out (revoke on instance A closing conns on instance B) is
// deferred to Phase 10/11, which will need periodic per-conn revalidation
// anyway.
type Registry struct {
	mu    sync.Mutex
	conns map[string]map[int]func() // sessionID -> connSeq -> close func
	seq   int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]map[int]func())}
}

// Register records closeFn (which closes one browser WebSocket with the
// session_revoked code) under sessionID and returns an unregister func to call
// when the connection ends normally. A blank sessionID is not tracked (the
// connection simply will not be force-closed on revoke) but still returns a
// usable no-op unregister.
func (r *Registry) Register(sessionID string, closeFn func()) func() {
	if sessionID == "" {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	id := r.seq
	m := r.conns[sessionID]
	if m == nil {
		m = make(map[int]func())
		r.conns[sessionID] = m
	}
	m[id] = closeFn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if m := r.conns[sessionID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(r.conns, sessionID)
			}
		}
	}
}

// SessionRevoked closes every live connection authorized by sessionID. The
// close funcs run outside the lock so a slow Close cannot stall registration.
func (r *Registry) SessionRevoked(sessionID string) {
	r.mu.Lock()
	m := r.conns[sessionID]
	closers := make([]func(), 0, len(m))
	for _, fn := range m {
		closers = append(closers, fn)
	}
	r.mu.Unlock()
	for _, fn := range closers {
		fn()
	}
}
