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
)

// Driver-level states reported by the agent. The control plane maps these onto
// its own machine states (see controlplane/internal/machine).
const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateError    = "error"
)

// EnsureRequest is the body of PUT /v1/machines/{id}: the desired VM shape and
// the pinned image refs. Idempotent — re-PUTing an already-running machine is a
// no-op that returns the existing handle.
type EnsureRequest struct {
	Vcpus     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	KernelRef string `json:"kernel_ref"`
	RootfsRef string `json:"rootfs_ref"`
}

// EnsureResponse is returned by PUT /v1/machines/{id} (202).
type EnsureResponse struct {
	Handle string `json:"handle"`
}

// MachineStatus is the body of GET /v1/machines/{id} (200). Reason is non-empty
// only in the error state. GuestIP is set once the agent has allocated it.
type MachineStatus struct {
	MachineID string `json:"machine_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Handle    string `json:"handle,omitempty"`
	GuestIP   string `json:"guest_ip,omitempty"`
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
	ErrUnknownMachine = "unknown_machine"
	ErrUnauthorized   = "unauthorized"
	ErrBadRequest     = "bad_request"
	ErrInternal       = "internal"
)
