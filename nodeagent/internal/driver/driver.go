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
// passes.
type VMSpec struct {
	MachineID string
	Vcpus     int
	MemMiB    int
	KernelRef string
	RootfsRef string
	Disks     []Disk // Phase 4: the persistent disk(s) to attach (single entry today)

	// VolumeKey is the 32-byte LUKS key for the machine volume (Phase 4
	// decision #2), delivered fresh on every ensure and held only in memory for
	// luksOpen. Never persisted host-side, never logged.
	VolumeKey []byte
}

// Disk is a persistent block device attached to the VM (Phase 4). Single entry
// per machine this phase; the slice is the seam for future multi-disk / network
// storage behind the same abstraction.
type Disk struct {
	ID      string
	SizeMiB int
}

// StopMode selects how Stop shuts a machine down (Phase 4 decision #4).
type StopMode string

const (
	// StopModeHibernate pauses the VM, takes a Full snapshot onto the encrypted
	// volume, kills the VMM, and closes the volume. Start resumes from it.
	StopModeHibernate StopMode = "hibernate"
	// StopModePoweroff is the cold path (CtrlAltDel/poweroff): no snapshot.
	StopModePoweroff StopMode = "poweroff"
)

// Snapshot is the driver-level view of a machine's current hibernation snapshot.
type Snapshot struct {
	Present   bool
	CreatedAt string // RFC3339
	FCVersion string
	MemBytes  int64
}

// Status is the driver-level view of a machine. The control plane maps State
// onto its own machine states.
type Status struct {
	MachineID string
	State     string // agentapi.State*: creating|running|stopping|hibernating|stopped|error
	Reason    string // populated in the error state
	Handle    string
	GuestIP   string

	// Phase 4: how the current/last run started, the attached disk, and the
	// current snapshot (if hibernated).
	Boot     string // agentapi.BootCold | agentapi.BootResumed
	DiskID   string
	Snapshot Snapshot

	// ResumeHygiene is the outcome of the post-resume guest /resume hook
	// (decision #9): "ok" once the guest corrected its clock and reseeded entropy,
	// "failed" if the best-effort hook errored, empty on cold boot. ResumeSkewMS
	// is the skew the guest corrected (host − guest) in ms. Observability for the
	// Phase 4 acceptance test, which asserts resume hygiene actually ran.
	ResumeHygiene string
	ResumeSkewMS  int64
}

// Driver is the desired-state interface the agent drives. EnsureRunning is the
// idempotent verb the control-plane poller leans on: calling it for a machine
// that is already running is a no-op that returns the existing handle.
type Driver interface {
	// EnsureRunning ensures a VM matching spec is booting or running and
	// returns its stable handle. Idempotent. Returns quickly (202 semantics);
	// the actual boot completes asynchronously and is observed via Status.
	EnsureRunning(ctx context.Context, spec VMSpec) (handle string, err error)

	// Stop requests a shutdown (async). StopModeHibernate pauses + snapshots
	// (machine moves through hibernating → stopped); StopModePoweroff does a cold
	// shutdown (stopping → stopped). Unknown machine ⇒ ErrUnknownMachine.
	Stop(ctx context.Context, machineID string, mode StopMode) error

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
// port selects the in-guest vsock port (Phase 8): the terminal agent
// (agentapi.GuestTerminalPort) or the code-server forward
// (agentapi.GuestWebPort). A zero port means the driver's default terminal
// port, so Phase 3/4 callers stay source-compatible.
//
//   - FirecrackerDriver: connects to the jailed vsock uds and performs the
//     hybrid CONNECT/OK handshake to reach the requested guest port.
//   - DevDriver: dials the machine's guest.sock (terminal) or guest-web.sock
//     (web) unix socket.
//
// DialGuest returns ErrUnknownMachine for an id the driver does not track.
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error)
}
