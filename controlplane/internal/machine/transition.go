package machine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrInvalidTransition is returned when from→to is not in the allowed table.
// It is a static rule violation (a bug or a stale request), distinct from a
// race.
type ErrInvalidTransition struct {
	From State
	To   State
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("machine: illegal transition %s→%s", e.From, e.To)
}

// ErrStateConflict is returned when the transition is legal but the machine was
// not in the expected from-state at write time (the guarded CAS matched zero
// rows) — i.e. the machine moved underneath us, or does not exist. Callers map
// this to HTTP 409.
var ErrStateConflict = errors.New("machine: state conflict (not in expected state)")

// TransitionParams describes one guarded state change plus its audit event.
type TransitionParams struct {
	MachineID pgtype.UUID
	From      State
	To        State
	Actor     string  // ActorAPI / ActorPoller / ActorUser(id)
	EventType string  // EventTransition / EventError / EventInfo; defaults to EventTransition
	Payload   []byte  // jsonb event payload; nil ⇒ "{}"
	LastError *string // written to machines.last_error; nil clears it
}

// Transition performs the guarded CAS update and the machine_events insert in a
// single transaction. It first rejects statically-illegal transitions
// (ErrInvalidTransition) so a bad request never opens a transaction; then the
// CAS guard (state=From) makes raced/incorrect transitions affect zero rows,
// surfaced as ErrStateConflict. On success exactly one event row is written for
// the change. Returns the updated machine and the inserted event (the event id
// is the SSE Last-Event-ID).
func Transition(ctx context.Context, pool *pgxpool.Pool, p TransitionParams) (store.Machine, store.MachineEvent, error) {
	if !CanTransition(p.From, p.To) {
		return store.Machine{}, store.MachineEvent{}, ErrInvalidTransition{From: p.From, To: p.To}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return store.Machine{}, store.MachineEvent{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	qtx := store.New(tx)

	m, err := qtx.UpdateMachineState(ctx, store.UpdateMachineStateParams{
		ID:        p.MachineID,
		FromState: string(p.From),
		ToState:   string(p.To),
		LastError: p.LastError,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Machine{}, store.MachineEvent{}, ErrStateConflict
		}
		return store.Machine{}, store.MachineEvent{}, fmt.Errorf("update state: %w", err)
	}

	eventType := p.EventType
	if eventType == "" {
		eventType = EventTransition
	}
	payload := p.Payload
	if payload == nil {
		payload = []byte("{}")
	}
	from := string(p.From)
	to := string(p.To)
	ev, err := qtx.InsertMachineEvent(ctx, store.InsertMachineEventParams{
		MachineID: p.MachineID,
		Type:      eventType,
		FromState: &from,
		ToState:   &to,
		Actor:     p.Actor,
		Payload:   payload,
	})
	if err != nil {
		return store.Machine{}, store.MachineEvent{}, fmt.Errorf("insert event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return store.Machine{}, store.MachineEvent{}, fmt.Errorf("commit: %w", err)
	}
	return m, ev, nil
}
