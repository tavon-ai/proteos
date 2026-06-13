package term

import "sync"

// Manager is the named registry of live Sessions. It creates a session on first
// attach and removes it when its shell exits, so the next attach to that name
// spawns a fresh shell — matching the protocol's "shell exit ⇒ next attach is
// new" rule while keeping sessions alive across WebSocket drops in between.
type Manager struct {
	defaults Config // template: Shell, ScrollbackKiB, Env (Name is per-Get)

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager returns a Manager that spawns sessions from the given defaults.
func NewManager(defaults Config) *Manager {
	return &Manager{
		defaults: defaults,
		sessions: make(map[string]*Session),
	}
}

// Get returns the live session for name, creating it (and its shell) if absent.
// The returned session is guaranteed live at return; if its shell exits later,
// it is auto-removed and the following Get spawns a fresh one.
func (m *Manager) Get(name string) (*Session, error) {
	cfg := m.defaults
	cfg.Name = name
	return m.getOrCreate(name, cfg)
}

// GetAgent returns the live agent session for name, creating it if absent by
// spawning command (argv) with env overlaid on the manager's base environment
// instead of the login shell (Phase 5 decision #9). Like Get, the session
// outlives connections and is auto-removed when the command exits.
func (m *Manager) GetAgent(name string, command []string, env []string) (*Session, error) {
	cfg := m.defaults
	cfg.Name = name
	cfg.Command = command
	// Overlay the provider env on top of the base (home) env; later entries win.
	cfg.Env = append(append([]string{}, m.defaults.Env...), env...)
	return m.getOrCreate(name, cfg)
}

// getOrCreate returns the live session for name, creating it from cfg if absent.
func (m *Manager) getOrCreate(name string, cfg Config) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[name]; ok {
		return s, nil
	}

	s, err := newSession(cfg)
	if err != nil {
		return nil, err
	}
	m.sessions[name] = s

	// Auto-remove on exit so the next Get spawns fresh. Guard against removing a
	// replacement session that reused the name after this one exited.
	go func() {
		<-s.Done()
		m.mu.Lock()
		if cur, ok := m.sessions[name]; ok && cur == s {
			delete(m.sessions, name)
		}
		m.mu.Unlock()
	}()

	return s, nil
}

// Shutdown kills every live session. Used on agent shutdown and in tests.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}
