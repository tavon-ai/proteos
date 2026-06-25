package machine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// seedMachineDirect creates a user, a host, and a machine in the 'requested'
// state, returning the machine id.
func seedMachineDirect(t *testing.T, q *store.Queries) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 1001, Login: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "host-1", AgentUrl: "http://agent:9090"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID:       user.ID,
		HostID:       host.ID,
		KernelRef:    "vmlinux-1",
		RootfsRef:    "rootfs-1",
		ResourceSpec: []byte(`{"vcpus":2,"mem_mib":2048}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != string(machine.StateRequested) {
		t.Fatalf("new machine state=%q, want requested", m.State)
	}
	return m.ID
}

func TestLegalTransitionWritesExactlyOneEvent(t *testing.T) {
	pool, q := testutil.Postgres(t)
	ctx := context.Background()
	id := seedMachineDirect(t, q)

	m, ev, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id,
		From:      machine.StateRequested,
		To:        machine.StateProvisioning,
		Actor:     machine.ActorAPI,
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if m.State != string(machine.StateProvisioning) {
		t.Fatalf("machine state=%q, want provisioning", m.State)
	}
	if ev.ID == 0 || ev.Type != machine.EventTransition {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// Exactly one event row for this machine.
	evs, err := q.ListMachineEventsRecent(ctx, store.ListMachineEventsRecentParams{MachineID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].FromState == nil || *evs[0].FromState != "requested" || evs[0].ToState == nil || *evs[0].ToState != "provisioning" {
		t.Fatalf("event from/to not recorded: %+v", evs[0])
	}
}

func TestIllegalTransitionRejectedAndWritesNoEvent(t *testing.T) {
	pool, q := testutil.Postgres(t)
	ctx := context.Background()
	id := seedMachineDirect(t, q)

	// requested→running is not in the table.
	_, _, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id,
		From:      machine.StateRequested,
		To:        machine.StateRunning,
		Actor:     machine.ActorAPI,
	})
	var inv machine.ErrInvalidTransition
	if !errors.As(err, &inv) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}

	// The machine did not move and no event was written.
	m, err := q.GetMachineByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != string(machine.StateRequested) {
		t.Fatalf("machine moved to %q on illegal transition", m.State)
	}
	evs, _ := q.ListMachineEventsRecent(ctx, store.ListMachineEventsRecentParams{MachineID: id, Limit: 50})
	if len(evs) != 0 {
		t.Fatalf("illegal transition wrote %d events", len(evs))
	}
}

func TestStateConflictWhenNotInFromState(t *testing.T) {
	pool, q := testutil.Postgres(t)
	ctx := context.Background()
	id := seedMachineDirect(t, q)

	// Move requested→provisioning first.
	if _, _, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id, From: machine.StateRequested, To: machine.StateProvisioning, Actor: machine.ActorAPI,
	}); err != nil {
		t.Fatal(err)
	}

	// Now attempt requested→provisioning again: legal edge, but the row is no
	// longer in 'requested' ⇒ CAS matches nothing ⇒ ErrStateConflict.
	_, _, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id, From: machine.StateRequested, To: machine.StateProvisioning, Actor: machine.ActorAPI,
	})
	if !errors.Is(err, machine.ErrStateConflict) {
		t.Fatalf("want ErrStateConflict, got %v", err)
	}

	// Still exactly one event (from the first, successful transition).
	evs, _ := q.ListMachineEventsRecent(ctx, store.ListMachineEventsRecentParams{MachineID: id, Limit: 50})
	if len(evs) != 1 {
		t.Fatalf("want 1 event after a conflicting attempt, got %d", len(evs))
	}
}

func TestErrorTransitionRecordsReason(t *testing.T) {
	pool, q := testutil.Postgres(t)
	ctx := context.Background()
	id := seedMachineDirect(t, q)
	if _, _, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id, From: machine.StateRequested, To: machine.StateProvisioning, Actor: machine.ActorAPI,
	}); err != nil {
		t.Fatal(err)
	}

	reason := "boot failed (dev:fail-boot)"
	m, ev, err := machine.Transition(ctx, pool, machine.TransitionParams{
		MachineID: id,
		From:      machine.StateProvisioning,
		To:        machine.StateError,
		Actor:     machine.ActorPoller,
		EventType: machine.EventError,
		Payload:   []byte(`{"reason":"boot failed (dev:fail-boot)"}`),
		LastError: &reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.LastError == nil || *m.LastError != reason {
		t.Fatalf("last_error not persisted: %v", m.LastError)
	}
	if ev.Type != machine.EventError {
		t.Fatalf("event type=%q, want error", ev.Type)
	}
}

func TestAllowedTableMatchesPlan(t *testing.T) {
	cases := []struct {
		from, to machine.State
		ok       bool
	}{
		{machine.StateRequested, machine.StateProvisioning, true},
		{machine.StateRequested, machine.StateError, true},
		{machine.StateProvisioning, machine.StateRunning, true},
		{machine.StateProvisioning, machine.StateError, true},
		{machine.StateRunning, machine.StateStopping, true},
		{machine.StateRunning, machine.StateError, true},
		{machine.StateStopping, machine.StateStopped, true},
		{machine.StateStopping, machine.StateError, true},
		{machine.StateStopped, machine.StateStarting, true},
		{machine.StateStarting, machine.StateRunning, true},
		{machine.StateStarting, machine.StateError, true},
		{machine.StateError, machine.StateStarting, true},
		// A few that must be rejected:
		{machine.StateRequested, machine.StateRunning, false},
		{machine.StateStopped, machine.StateRunning, false},
		{machine.StateRunning, machine.StateStopped, false},
		{machine.StateError, machine.StateRunning, false},
		{machine.StateStopped, machine.StateError, false},
	}
	for _, c := range cases {
		if got := machine.CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s,%s)=%v, want %v", c.from, c.to, got, c.ok)
		}
	}
}
