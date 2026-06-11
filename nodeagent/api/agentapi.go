// Package agentapi is the wire contract between the control plane and the
// node-agent: the JSON request/response types and the route constants. It is
// the ONLY nodeagent package the control plane imports, so it must stay free of
// Firecracker/netlink/host dependencies — pure types and consts only.
//
// Direction of trust: the control plane dials the node-agent (never the
// reverse). Commands are synchronous "202 accepted"; the driver-level state is
// read back by the control plane's poller via GetMachine/ListMachines. The
// control plane owns the mapping from these driver states to machine states.
package agentapi

// AuthHeader is the HTTP header carrying the shared bearer token. Both sides
// compare it in constant time.
const AuthHeader = "Authorization"

// BearerPrefix precedes the token value in the Authorization header.
const BearerPrefix = "Bearer "

// Route patterns (net/http 1.22 syntax). The control plane's nodeclient builds
// concrete URLs from these; the agent registers them verbatim.
const (
	RouteHealthz     = "GET /healthz"
	RouteEnsure      = "PUT /v1/machines/{id}"
	RouteStop        = "POST /v1/machines/{id}/stop"
	RouteGetMachine  = "GET /v1/machines/{id}"
	RouteListMachine = "GET /v1/machines"
	RouteDestroy     = "DELETE /v1/machines/{id}"

	// RouteGuest opens an opaque byte tunnel to the machine's in-guest agent
	// (Phase 3). The control plane sends an HTTP Upgrade with UpgradeGuestProto;
	// on 101 the connection becomes a raw bidirectional stream bridged to the
	// VM's vsock port 1024 (dev: the machine's guest.sock). The node-agent never
	// parses what flows through — the gateway and guest speak WebSocket to each
	// other across it.
	RouteGuest = "GET /v1/machines/{id}/guest"
)

// UpgradeGuestProto is the token in the Connection/Upgrade headers of the guest
// tunnel handshake. It is deliberately not "websocket": the node-agent does not
// terminate the WebSocket, it only relays bytes.
const UpgradeGuestProto = "proteos-guest"

// Driver-level states reported by the agent. The control plane maps these onto
// its own machine states (see controlplane/internal/machine).
const (
	StateCreating    = "creating"
	StateRunning     = "running"
	StateStopping    = "stopping"    // cold poweroff in progress
	StateHibernating = "hibernating" // pause + snapshot in progress (Phase 4)
	StateStopped     = "stopped"
	StateError       = "error"
)

// Stop modes carried in StopRequest.Mode (Phase 4 decision #4). Hibernate is the
// default: pause + Full snapshot + close volume. Poweroff is the cold path
// (CtrlAltDel) used explicitly or as the automatic fallback when snapshotting
// fails.
const (
	StopModeHibernate = "hibernate"
	StopModePoweroff  = "poweroff"
)

// Boot kinds reported in MachineStatus.Boot: how the current/last run started.
const (
	BootCold    = "cold"
	BootResumed = "resumed"
)

// EnsureRequest is the body of PUT /v1/machines/{id}: the desired VM shape and
// the pinned image refs. Idempotent — re-PUTing an already-running machine is a
// no-op that returns the existing handle.
//
// VolumeKeyB64 carries the machine's 32-byte LUKS volume key (base64), minted
// and held by the control plane (Phase 4 decision #2). It is sent on every
// ensure (the only call that needs it, for luksOpen), held in agent memory, and
// MUST NEVER be logged or persisted host-side — the request logger redacts it.
type EnsureRequest struct {
	Vcpus        int    `json:"vcpus"`
	MemMiB       int    `json:"mem_mib"`
	KernelRef    string `json:"kernel_ref"`
	RootfsRef    string `json:"rootfs_ref"`
	DiskID       string `json:"disk_id,omitempty"`
	DiskMiB      int    `json:"disk_mib,omitempty"`
	VolumeKeyB64 string `json:"volume_key_b64,omitempty"`
}

// EnsureResponse is returned by PUT /v1/machines/{id} (202).
type EnsureResponse struct {
	Handle string `json:"handle"`
}

// StopRequest is the (optional) body of POST /v1/machines/{id}/stop. An empty
// body defaults to hibernate (Phase 4 decision #4).
type StopRequest struct {
	Mode string `json:"mode,omitempty"` // StopModeHibernate (default) | StopModePoweroff
}

// SnapshotInfo describes the current hibernation snapshot, if any. Reported in
// MachineStatus so the control-plane poller can record snapshot metadata in
// Postgres (Phase 4 decision #5/#6).
type SnapshotInfo struct {
	Present   bool   `json:"present"`
	CreatedAt string `json:"created_at,omitempty"` // RFC3339
	FCVersion string `json:"fc_version,omitempty"`
	MemBytes  int64  `json:"mem_bytes,omitempty"`
}

// MachineStatus is the body of GET /v1/machines/{id} (200). Reason is non-empty
// only in the error state. GuestIP is set once the agent has allocated it.
type MachineStatus struct {
	MachineID string `json:"machine_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Handle    string `json:"handle,omitempty"`
	GuestIP   string `json:"guest_ip,omitempty"`

	// Phase 4: how the current/last run started, the attached disk, and the
	// current snapshot (if the machine is hibernated).
	Boot     string       `json:"boot,omitempty"` // BootCold | BootResumed
	DiskID   string       `json:"disk_id,omitempty"`
	Snapshot SnapshotInfo `json:"snapshot"`
}

// ListResponse is the body of GET /v1/machines (200), used for reconciliation.
type ListResponse struct {
	Machines []MachineStatus `json:"machines"`
}

// HealthResponse is the body of GET /healthz (200).
type HealthResponse struct {
	Status string `json:"status"`
}

// ErrorResponse is the consistent error envelope: {"error": "<code>"}.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Error codes returned in ErrorResponse.
const (
	ErrUnknownMachine   = "unknown_machine"
	ErrUnauthorized     = "unauthorized"
	ErrBadRequest       = "bad_request"
	ErrInternal         = "internal"
	ErrNotRunning       = "not_running"       // guest tunnel: machine is not running (409)
	ErrGuestUnreachable = "guest_unreachable" // guest tunnel: could not reach the guest agent (502)
)
