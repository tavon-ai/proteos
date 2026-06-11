// Package machine owns the machine state machine: the canonical state
// constants, the single allowed-transitions table, and the Transition helper
// that performs a guarded compare-and-set together with the machine_events
// audit insert in one transaction. Keeping the rules here means illegal
// transitions and missing audit rows are impossible by construction — no other
// package mutates machines.state directly.
package machine

// State is a machine lifecycle state. The string values match the CHECK
// constraint on machines.state.
type State string

const (
	StateRequested    State = "requested"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StateStarting     State = "starting"
	StateStopping     State = "stopping"
	StateHibernating  State = "hibernating" // reserved for Phase 4
	StateStopped      State = "stopped"
	StateError        State = "error"
)

// Event types recorded in machine_events.type.
const (
	EventTransition = "transition"
	EventError      = "error"
	EventInfo       = "info"
)

// Actor strings recorded in machine_events.actor.
const (
	ActorAPI    = "system:api"
	ActorPoller = "system:poller"
)

// ActorUser formats a user actor string for the audit log.
func ActorUser(userID string) string { return "user:" + userID }

// allowed is the single source of truth for legal transitions. A transition
// from→to is permitted iff allowed[from] contains to. Phase 4 adds the
// hibernate path: running → hibernating → stopped (resume is stopped → starting
// → running, already present). stopping stays the cold/poweroff path (and the
// driver's automatic fallback when snapshotting fails).
var allowed = map[State]map[State]bool{
	StateRequested:    {StateProvisioning: true, StateError: true},
	StateProvisioning: {StateRunning: true, StateError: true},
	StateRunning:      {StateStopping: true, StateHibernating: true, StateError: true},
	StateStopping:     {StateStopped: true, StateError: true},
	StateHibernating:  {StateStopped: true, StateError: true},
	StateStopped:      {StateStarting: true},
	StateStarting:     {StateRunning: true, StateError: true},
	StateError:        {StateStarting: true},
}

// CanTransition reports whether from→to is a legal transition.
func CanTransition(from, to State) bool {
	return allowed[from][to]
}

// Transitional reports whether a state is one the poller actively advances
// (the control plane is waiting on the agent to finish an async operation).
func (s State) Transitional() bool {
	switch s {
	case StateProvisioning, StateStarting, StateStopping, StateHibernating:
		return true
	default:
		return false
	}
}
