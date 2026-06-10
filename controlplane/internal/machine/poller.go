package machine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/store"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// Poll cadences. Transitional machines (provisioning/starting/stopping) are
// polled often because a user is waiting; running machines are swept rarely
// just to detect crashes. This is the embryo of Phase 11's reconciliation loop.
const (
	transitionalInterval = 2 * time.Second
	runningSweepInterval = 30 * time.Second
)

// Poller advances asynchronous machine operations by reconciling the
// control-plane state against what the node-agent reports. It is the only actor
// that moves machines out of transitional states (and into error on
// failure/crash); the user-facing Service only initiates transitions.
type Poller struct {
	pool   *pgxpool.Pool
	q      *store.Queries
	nodes  NodeClient
	broker *Broker
}

// NewPoller builds a poller. broker may be nil.
func NewPoller(pool *pgxpool.Pool, nodes NodeClient, broker *Broker) *Poller {
	return &Poller{pool: pool, q: store.New(pool), nodes: nodes, broker: broker}
}

// Run drives the poller until ctx is cancelled: a fast tick advances
// transitional machines and a slow tick sweeps running ones for crashes.
func (p *Poller) Run(ctx context.Context) {
	fast := time.NewTicker(transitionalInterval)
	slow := time.NewTicker(runningSweepInterval)
	defer fast.Stop()
	defer slow.Stop()
	slog.Info("machine poller started")
	for {
		select {
		case <-ctx.Done():
			slog.Info("machine poller stopped")
			return
		case <-fast.C:
			p.AdvanceTransitional(ctx)
		case <-slow.C:
			p.SweepRunning(ctx)
		}
	}
}

// AdvanceTransitional runs one reconciliation pass over machines in
// provisioning/starting/stopping. Exported so tests can drive it deterministically.
func (p *Poller) AdvanceTransitional(ctx context.Context) {
	states := []string{string(StateProvisioning), string(StateStarting), string(StateStopping)}
	machines, err := p.q.ListMachinesInStates(ctx, states)
	if err != nil {
		slog.Error("poller: list transitional", "err", err)
		return
	}
	for _, m := range machines {
		p.advanceOne(ctx, m)
	}
}

// SweepRunning checks each running machine is still alive on its agent; a
// machine the agent no longer reports as running has crashed and is moved to
// error. Exported for deterministic tests.
func (p *Poller) SweepRunning(ctx context.Context) {
	machines, err := p.q.ListMachinesInStates(ctx, []string{string(StateRunning)})
	if err != nil {
		slog.Error("poller: list running", "err", err)
		return
	}
	for _, m := range machines {
		id := UUIDString(m.ID)
		st, err := p.nodes.Status(ctx, id)
		if err != nil {
			p.toError(ctx, m, StateRunning, "node-agent unreachable during running sweep: "+err.Error())
			continue
		}
		if st.State != agentapi.StateRunning {
			reason := "vm no longer running (agent reports " + st.State + ")"
			if st.Reason != "" {
				reason += ": " + st.Reason
			}
			p.toError(ctx, m, StateRunning, reason)
		}
	}
}

// advanceOne reconciles a single transitional machine against its agent status.
func (p *Poller) advanceOne(ctx context.Context, m store.Machine) {
	id := UUIDString(m.ID)
	from := State(m.State)

	st, err := p.nodes.Status(ctx, id)
	if err != nil {
		// Unreachable, or the agent forgot the machine: a transitional op can't
		// complete, so fail with a reason (the retry path is Start for a
		// provisioning/starting machine; a stopping failure is terminal-ish).
		if errors.Is(err, nodeclient.ErrUnknownMachine) {
			if from == StateStopping {
				// Agent no longer tracks it ⇒ effectively stopped.
				p.transition(ctx, m, from, StateStopped, ActorPoller, EventTransition, nil, nil)
				return
			}
			p.toError(ctx, m, from, "node-agent no longer tracks this machine")
			return
		}
		p.toError(ctx, m, from, "node-agent unreachable: "+err.Error())
		return
	}

	switch from {
	case StateProvisioning, StateStarting:
		switch st.State {
		case agentapi.StateRunning:
			p.toRunning(ctx, m, from, st)
		case agentapi.StateError:
			p.toError(ctx, m, from, agentReason(st, "boot failed"))
		case agentapi.StateCreating:
			// still booting; wait for the next tick
		default: // stopping/stopped during boot is unexpected
			p.toError(ctx, m, from, "unexpected agent state during boot: "+st.State)
		}
	case StateStopping:
		switch st.State {
		case agentapi.StateStopped:
			p.transition(ctx, m, from, StateStopped, ActorPoller, EventTransition, nil, nil)
		case agentapi.StateError:
			p.toError(ctx, m, from, agentReason(st, "stop failed"))
		default: // stopping / still running: wait
		}
	}
}

// toRunning records the agent-reported guest IP + handle, then transitions the
// machine to running.
func (p *Poller) toRunning(ctx context.Context, m store.Machine, from State, st agentapi.MachineStatus) {
	handle := st.Handle
	params := store.SetMachineRuntimeParams{ID: m.ID, VmHandle: &handle}
	if st.GuestIP != "" {
		if addr, err := netip.ParseAddr(st.GuestIP); err == nil {
			params.GuestIp = &addr
		}
	}
	if _, err := p.q.SetMachineRuntime(ctx, params); err != nil {
		slog.Error("poller: set runtime", "machine", UUIDString(m.ID), "err", err)
		// fall through; the transition still records running
	}
	p.transition(ctx, m, from, StateRunning, ActorPoller, EventTransition, nil, nil)
}

// toError moves a machine to the error state with a reason, recorded in both
// last_error and the event payload.
func (p *Poller) toError(ctx context.Context, m store.Machine, from State, reason string) {
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	p.transition(ctx, m, from, StateError, ActorPoller, EventError, payload, &reason)
}

// transition performs the guarded transition and publishes the update. Poller
// transitions can legitimately lose a CAS race (e.g. the user stopped the
// machine between the list and now); such conflicts are logged, not fatal.
func (p *Poller) transition(ctx context.Context, m store.Machine, from, to State, actor, eventType string, payload []byte, lastErr *string) {
	updated, ev, err := Transition(ctx, p.pool, TransitionParams{
		MachineID: m.ID, From: from, To: to, Actor: actor, EventType: eventType, Payload: payload, LastError: lastErr,
	})
	if err != nil {
		if errors.Is(err, ErrStateConflict) {
			slog.Debug("poller: transition raced, skipping", "machine", UUIDString(m.ID), "from", from, "to", to)
			return
		}
		slog.Error("poller: transition", "machine", UUIDString(m.ID), "from", from, "to", to, "err", err)
		return
	}
	p.broker.Publish(Update{Machine: updated, Event: ev})
}

func agentReason(st agentapi.MachineStatus, fallback string) string {
	if st.Reason != "" {
		return st.Reason
	}
	return fallback
}
