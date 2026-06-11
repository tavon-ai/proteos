// Package driver defines the contract every VM backend implements. The dev
// driver (process-backed) and the firecracker driver (linux-only) both satisfy
// it, so the HTTP layer and re-attach logic are backend-agnostic.
package driver

import (
	"context"
	"errors"
	"net"
)

// ErrUnknownMachine is returned by Status/Stop/Destroy for an id the driver has
// no record of. The HTTP layer maps it to 404 unknown_machine.
var ErrUnknownMachine = errors.New("driver: unknown machine")

// VMSpec is the desired shape of a microVM. Net is filled by the driver/agent
// (it owns IP/tap allocation), so EnsureRunning ignores any Net the caller
// passes. Disks stays empty until Phase 4 (persistent disks); adding it now
// keeps the interface stable when snapshot/resume land.
type VMSpec struct {
	MachineID string
	Vcpus     int
	MemMiB    int
	KernelRef string
	RootfsRef string
	Disks     []Disk // empty this phase
}

// Disk is reserved for Phase 4 persistent disks.
type Disk struct {
	ID       string
	PathRef  string
	ReadOnly bool
}

// Status is the driver-level view of a machine. The control plane maps State
// onto its own machine states.
type Status struct {
	MachineID string
	State     string // agentapi.State*: creating|running|stopping|stopped|error
	Reason    string // populated in the error state
	Handle    string
	GuestIP   string
}

// Driver is the desired-state interface the agent drives. EnsureRunning is the
// idempotent verb the control-plane poller leans on: calling it for a machine
// that is already running is a no-op that returns the existing handle.
type Driver interface {
	// EnsureRunning ensures a VM matching spec is booting or running and
	// returns its stable handle. Idempotent. Returns quickly (202 semantics);
	// the actual boot completes asynchronously and is observed via Status.
	EnsureRunning(ctx context.Context, spec VMSpec) (handle string, err error)

	// Stop requests a graceful shutdown (async): the machine moves to stopping
	// and then stopped. Unknown machine ⇒ ErrUnknownMachine.
	Stop(ctx context.Context, machineID string) error

	// Status returns the current driver-level status. Unknown ⇒ ErrUnknownMachine.
	Status(ctx context.Context, machineID string) (Status, error)

	// Destroy stops (if needed) and removes all trace of the machine: child
	// process / VMM, tap, rules, jail, and on-disk state. Idempotent.
	Destroy(ctx context.Context, machineID string) error

	// List returns the status of every machine the driver tracks.
	List(ctx context.Context) ([]Status, error)

	// Reattach reconciles in-memory/runtime state with on-disk records at
	// startup: live processes are re-adopted; dead ones are marked stopped or
	// error. Called once before serving.
	Reattach(ctx context.Context) error
}

// GuestDialer is implemented by drivers that can open a byte stream to a
// machine's in-guest agent (Phase 3). The HTTP layer's guest-tunnel route
// type-asserts the driver to this interface; the returned conn is bridged 1:1
// to the caller (the control-plane gateway) so the node-agent never parses the
// terminal protocol — it is a dumb pipe (decision #4).
//
//   - FirecrackerDriver: connects to the jailed vsock uds and performs the
//     hybrid CONNECT/OK handshake to reach guest port 1024.
//   - DevDriver: dials the machine's guest.sock unix socket.
//
// DialGuest returns ErrUnknownMachine for an id the driver does not track.
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string) (net.Conn, error)
}
