package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// Service errors mapped by the HTTP layer to status codes.
var (
	ErrNoMachine    = errors.New("machine: none for user")          // 404
	ErrInvalidState = errors.New("machine: invalid state for op")   // 409
	ErrMachineLimit = errors.New("machine: per-user limit reached") // 409
	ErrAmbiguous    = errors.New("machine: which one (multiple)")   // 400 (resolver)
)

// defaultMaxPerUser caps how many machines a user may own when Spec.MaxPerUser
// is unset (≤0). The cap protects the single fc-node host's RAM and guest-IP pool.
const defaultMaxPerUser = 3

// NodeClient is the subset of the node-agent client the lifecycle needs. Kept
// as an interface so the service and poller are testable against a fake agent.
type NodeClient interface {
	Ensure(ctx context.Context, id string, req agentapi.EnsureRequest) (agentapi.EnsureResponse, error)
	Stop(ctx context.Context, id, mode string) error
	Status(ctx context.Context, id string) (agentapi.MachineStatus, error)
	Destroy(ctx context.Context, id string) error
}

// Spec is the resource shape and pinned image refs stamped on new machines.
type Spec struct {
	Vcpus      int
	MemMiB     int
	DiskMiB    int // Phase 4: persistent disk size (default 10240)
	KernelRef  string
	RootfsRef  string
	MaxPerUser int // per-user machine cap; ≤0 ⇒ defaultMaxPerUser
}

// Service owns machine lifecycle operations driven by the user-facing API. All
// state changes go through machine.Transition, so the audit log and SSE stream
// stay consistent with the machines table.
type Service struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	nodes   NodeClient
	broker  *Broker
	secrets secrets.Store
	hostID  pgtype.UUID
	spec    Spec
}

// NewService wires a lifecycle service. broker may be nil (publishing is then a
// no-op), which keeps unit tests that don't care about SSE simple. sec holds
// per-machine volume keys (Phase 4); it must be non-nil.
func NewService(pool *pgxpool.Pool, nodes NodeClient, broker *Broker, sec secrets.Store, hostID pgtype.UUID, spec Spec) *Service {
	return &Service{pool: pool, q: store.New(pool), nodes: nodes, broker: broker, secrets: sec, hostID: hostID, spec: spec}
}

// Get returns the user's machine, or ErrNoMachine. With multi-machine it returns
// an arbitrary row if the user has several, so it is retained only for the
// gateway resolver's single-machine fallback; id-based callers use getOwned.
func (s *Service) Get(ctx context.Context, userID pgtype.UUID) (store.Machine, error) {
	m, err := s.q.GetMachineByUserID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Machine{}, ErrNoMachine
	}
	return m, err
}

// List returns all of a user's machines, oldest-first. Empty slice if none.
func (s *Service) List(ctx context.Context, userID pgtype.UUID) ([]store.Machine, error) {
	return s.q.ListMachinesByUserID(ctx, userID)
}

// OnlyMachine returns the user's machine iff they own exactly one. ErrNoMachine
// if they have none; ErrAmbiguous if they have more than one (the caller must
// then require an explicit machine id). Backs the gateway resolver's empty-param
// fallback so it never silently picks an arbitrary machine.
func (s *Service) OnlyMachine(ctx context.Context, userID pgtype.UUID) (store.Machine, error) {
	ms, err := s.q.ListMachinesByUserID(ctx, userID)
	if err != nil {
		return store.Machine{}, err
	}
	switch len(ms) {
	case 0:
		return store.Machine{}, ErrNoMachine
	case 1:
		return ms[0], nil
	default:
		return store.Machine{}, ErrAmbiguous
	}
}

// getOwned resolves a machine by id and verifies the user owns it. A missing or
// foreign machine yields ErrNoMachine (never leaking whether the id exists).
func (s *Service) getOwned(ctx context.Context, userID, machineID pgtype.UUID) (store.Machine, error) {
	m, err := s.q.GetMachineByID(ctx, machineID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Machine{}, ErrNoMachine
	}
	if err != nil {
		return store.Machine{}, err
	}
	if !m.UserID.Valid || !userID.Valid || m.UserID.Bytes != userID.Bytes {
		return store.Machine{}, ErrNoMachine
	}
	return m, nil
}

// DiskFor returns the machine's persistent disk, or nil if none is provisioned.
func (s *Service) DiskFor(ctx context.Context, machineID pgtype.UUID) (*store.Disk, error) {
	d, err := s.q.GetDiskByMachineID(ctx, machineID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SnapshotFor returns the machine's current hibernation snapshot, or nil if the
// machine is not hibernated.
func (s *Service) SnapshotFor(ctx context.Context, machineID pgtype.UUID) (*store.Snapshot, error) {
	snap, err := s.q.GetSnapshot(ctx, machineID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetByID returns the machine with the given id, or ErrNoMachine. Ownership is
// the caller's responsibility (the gateway treats a foreign machine as
// ErrNoMachine to avoid leaking existence).
func (s *Service) GetByID(ctx context.Context, id pgtype.UUID) (store.Machine, error) {
	m, err := s.q.GetMachineByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Machine{}, ErrNoMachine
	}
	return m, err
}

// Create provisions a new machine for the user: insert (requested) → transition
// to provisioning → ask the agent to ensure-running. If the agent call fails
// the machine is moved to error (with the reason), and the errored machine is
// still returned (the create "succeeded" in that a machine row now exists; the
// user can retry via Start). Returns ErrMachineLimit if the user is at their
// per-user cap. name is the display label; empty ⇒ auto-named machine-<n>.
func (s *Service) Create(ctx context.Context, userID pgtype.UUID, name string) (store.Machine, error) {
	count, err := s.q.CountMachinesByUserID(ctx, userID)
	if err != nil {
		return store.Machine{}, fmt.Errorf("count machines: %w", err)
	}
	max := s.spec.MaxPerUser
	if max <= 0 {
		max = defaultMaxPerUser
	}
	if count >= int64(max) {
		return store.Machine{}, ErrMachineLimit
	}
	if name == "" {
		name = fmt.Sprintf("machine-%d", count+1)
	}

	diskMiB := s.spec.DiskMiB
	if diskMiB <= 0 {
		diskMiB = 10240
	}
	specJSON, _ := json.Marshal(map[string]int{"vcpus": s.spec.Vcpus, "mem_mib": s.spec.MemMiB, "disk_mib": diskMiB})
	m, err := s.q.CreateMachine(ctx, store.CreateMachineParams{
		UserID:       userID,
		HostID:       s.hostID,
		Name:         name,
		KernelRef:    s.spec.KernelRef,
		RootfsRef:    s.spec.RootfsRef,
		ResourceSpec: specJSON,
	})
	if err != nil {
		return store.Machine{}, fmt.Errorf("create machine: %w", err)
	}

	// Phase 4: allocate the persistent disk (1:1) and attach it to the machine,
	// then mint the machine's LUKS volume key into the secret store. The key is
	// delivered to the agent on every ensure and never persisted in Postgres.
	disk, err := s.q.CreateDisk(ctx, store.CreateDiskParams{MachineID: m.ID, SizeMib: int32(diskMiB)})
	if err != nil {
		return store.Machine{}, fmt.Errorf("create disk: %w", err)
	}
	m, err = s.q.SetMachineDisk(ctx, store.SetMachineDiskParams{ID: m.ID, DiskID: disk.ID})
	if err != nil {
		return store.Machine{}, fmt.Errorf("attach disk: %w", err)
	}
	if _, err := secrets.MintMachineVolumeKey(s.secrets, rand.Reader, UUIDString(m.ID)); err != nil {
		return store.Machine{}, fmt.Errorf("mint volume key: %w", err)
	}

	m, _, err = s.transition(ctx, m.ID, StateRequested, StateProvisioning, ActorUser(UUIDString(userID)), EventTransition, nil, nil)
	if err != nil {
		return store.Machine{}, err
	}

	return s.ensureOnAgent(ctx, m, s.spec.KernelRef, s.spec.RootfsRef)
}

// Start cold-boots a stopped or errored machine: transition to starting → ask
// the agent to ensure-running. Invalid current state ⇒ ErrInvalidState; missing
// or foreign machine ⇒ ErrNoMachine.
func (s *Service) Start(ctx context.Context, userID, machineID pgtype.UUID) (store.Machine, error) {
	m, err := s.getOwned(ctx, userID, machineID)
	if err != nil {
		return store.Machine{}, err
	}
	from := State(m.State)
	if from != StateStopped && from != StateError {
		return store.Machine{}, ErrInvalidState
	}
	m, _, err = s.transition(ctx, m.ID, from, StateStarting, ActorUser(UUIDString(userID)), EventTransition, nil, nil)
	if err != nil {
		return store.Machine{}, mapTransitionErr(err)
	}
	return s.ensureOnAgent(ctx, m, m.KernelRef, m.RootfsRef)
}

// Stop hibernates a running machine (Phase 4 decision #4: stop = hibernate):
// transition running → hibernating → ask the agent to pause+snapshot. The agent
// falls back to a cold poweroff internally if snapshotting fails; either way the
// poller advances the machine to stopped. Invalid current state ⇒ ErrInvalidState.
func (s *Service) Stop(ctx context.Context, userID, machineID pgtype.UUID) (store.Machine, error) {
	m, err := s.getOwned(ctx, userID, machineID)
	if err != nil {
		return store.Machine{}, err
	}
	if State(m.State) != StateRunning {
		return store.Machine{}, ErrInvalidState
	}
	m, _, err = s.transition(ctx, m.ID, StateRunning, StateHibernating, ActorUser(UUIDString(userID)), EventTransition, nil, nil)
	if err != nil {
		return store.Machine{}, mapTransitionErr(err)
	}

	id := UUIDString(m.ID)
	if err := s.nodes.Stop(ctx, id, agentapi.StopModeHibernate); err != nil {
		return s.fail(ctx, m, StateHibernating, "node-agent stop failed: "+err.Error())
	}
	return m, nil
}

// Destroy tears a machine down completely and removes it. Unlike Stop (which
// hibernates and is reversible via Start), Destroy is irreversible: the VM and
// all its host resources are torn down on the agent, the persistent disk is
// wiped, the LUKS volume key is dropped, and the machine row is hard-deleted
// (cascading to its disk, snapshot, and event log). Allowed from any state — the
// point is to wipe the machine regardless of where it is in its lifecycle.
// Missing or foreign machine ⇒ ErrNoMachine.
func (s *Service) Destroy(ctx context.Context, userID, machineID pgtype.UUID) error {
	m, err := s.getOwned(ctx, userID, machineID)
	if err != nil {
		return err
	}
	id := UUIDString(m.ID)

	// Tear the VM and its host resources down on the agent first. A machine the
	// agent no longer tracks is already gone (idempotent); any other failure
	// aborts the destroy so the row survives and the user can retry.
	if err := s.nodes.Destroy(ctx, id); err != nil && !errors.Is(err, nodeclient.ErrUnknownMachine) {
		return fmt.Errorf("node-agent destroy failed: %w", err)
	}

	// Best-effort: drop the volume key. A leftover key is harmless (its volume is
	// already destroyed) but should not linger in the secret store.
	if err := s.secrets.Delete(secrets.MachineVolumeKeyPath(id)); err != nil {
		slog.Warn("destroy: delete volume key", "machine", id, "err", err)
	}

	// Hard-delete the row; the cascade removes the disk, snapshot, and event log.
	if err := s.q.DeleteMachine(ctx, m.ID); err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}

	// Notify live SSE subscribers the machine is gone (the row no longer exists,
	// so this carries the pre-delete row only for the user-id filter).
	s.broker.Publish(Update{Machine: m, Deleted: true})
	return nil
}

// Rename sets a machine's display name and publishes the change so the switcher
// updates live. Missing or foreign machine ⇒ ErrNoMachine. The rename is a
// metadata-only update (no state transition, no audit event).
func (s *Service) Rename(ctx context.Context, userID, machineID pgtype.UUID, name string) (store.Machine, error) {
	if _, err := s.getOwned(ctx, userID, machineID); err != nil {
		return store.Machine{}, err
	}
	m, err := s.q.RenameMachine(ctx, store.RenameMachineParams{ID: machineID, Name: name})
	if err != nil {
		return store.Machine{}, fmt.Errorf("rename machine: %w", err)
	}
	// Record an info event so the change flows over the SSE stream with a real
	// (bigserial) id — the live loop and Last-Event-ID replay both key off it.
	// Best-effort: the rename is already durable if this insert fails.
	payload, _ := json.Marshal(map[string]string{"name": name})
	if ev, err := s.q.InsertMachineEvent(ctx, store.InsertMachineEventParams{
		MachineID: machineID, Type: EventInfo, Actor: ActorUser(UUIDString(userID)), Payload: payload,
	}); err == nil {
		s.broker.Publish(Update{Machine: m, Event: ev})
	}
	return m, nil
}

// ensureOnAgent issues the agent ensure-running for a machine already moved to a
// transitional state (provisioning or starting). On agent failure it moves the
// machine to error; on success it records the returned handle and leaves the
// machine transitional for the poller to advance to running.
func (s *Service) ensureOnAgent(ctx context.Context, m store.Machine, kernelRef, rootfsRef string) (store.Machine, error) {
	id := UUIDString(m.ID)

	// Phase 4: deliver the disk and the volume key on every ensure (the only
	// call that needs the key — for luksOpen). The key is fetched fresh from the
	// secret store and never logged.
	req := agentapi.EnsureRequest{
		Vcpus:     s.spec.Vcpus,
		MemMiB:    s.spec.MemMiB,
		KernelRef: kernelRef,
		RootfsRef: rootfsRef,
	}
	if disk, err := s.q.GetDiskByMachineID(ctx, m.ID); err == nil {
		req.DiskID = UUIDString(disk.ID)
		req.DiskMiB = int(disk.SizeMib)
	}
	keyB64, err := secrets.GetMachineVolumeKey(s.secrets, id)
	if err != nil {
		return s.fail(ctx, m, State(m.State), "fetch volume key: "+err.Error())
	}
	req.VolumeKeyB64 = keyB64

	resp, err := s.nodes.Ensure(ctx, id, req)
	if err != nil {
		return s.fail(ctx, m, State(m.State), "node-agent ensure failed: "+err.Error())
	}

	handle := resp.Handle
	updated, err := s.q.SetMachineRuntime(ctx, store.SetMachineRuntimeParams{ID: m.ID, VmHandle: &handle})
	if err != nil {
		return store.Machine{}, fmt.Errorf("record handle: %w", err)
	}
	return updated, nil
}

// fail moves a machine from its current transitional state to error with the
// given reason (recorded both in last_error and the event payload) and returns
// the errored machine. The create/start/stop flows return this with a nil error
// so the API surfaces the errored machine summary rather than a 5xx.
func (s *Service) fail(ctx context.Context, m store.Machine, from State, reason string) (store.Machine, error) {
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	errored, _, terr := s.transition(ctx, m.ID, from, StateError, ActorAPI, EventError, payload, &reason)
	if terr != nil {
		// The transition itself failed (e.g. a race). Surface the original
		// reason to the caller rather than the secondary error.
		return store.Machine{}, fmt.Errorf("%s (and could not record error: %v)", reason, terr)
	}
	return errored, nil
}

// transition wraps machine.Transition and publishes the resulting Update to the
// broker after the commit.
func (s *Service) transition(ctx context.Context, id pgtype.UUID, from, to State, actor, eventType string, payload []byte, lastErr *string) (store.Machine, store.MachineEvent, error) {
	m, ev, err := Transition(ctx, s.pool, TransitionParams{
		MachineID: id, From: from, To: to, Actor: actor, EventType: eventType, Payload: payload, LastError: lastErr,
	})
	if err != nil {
		return store.Machine{}, store.MachineEvent{}, err
	}
	s.broker.Publish(Update{Machine: m, Event: ev})
	return m, ev, nil
}

// mapTransitionErr collapses transition rule/conflict errors to ErrInvalidState
// for the HTTP layer (409), passing other errors through.
func mapTransitionErr(err error) error {
	var inv ErrInvalidTransition
	if errors.As(err, &inv) || errors.Is(err, ErrStateConflict) {
		return ErrInvalidState
	}
	return err
}

// ParseUUID parses a canonical (or hyphen-free) UUID string into a pgtype.UUID.
// Used to resolve the gateway's ?machine=<uuid> parameter; a malformed value
// yields !Valid (callers map that to "not found", not a 400, to avoid leaking
// whether the id exists).
func ParseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	err := u.Scan(s)
	return u, err
}

// UUIDString renders a pgtype.UUID in canonical 8-4-4-4-12 form. An invalid
// UUID renders empty (callers only pass valid ids).
func UUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
