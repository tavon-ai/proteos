package nodeclient

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// Registry dials the correct node-agent for a given machine or host by
// resolving hosts.agent_url from Postgres (TAV-37: multi-host foundation). It
// implements the same per-machine method set every existing caller already
// depends on (machine.NodeClient, the gateway/guestctl/injector GuestDialer
// interfaces), so it is a drop-in replacement for the single-host *Client
// those callers used to hold — only the wiring in cmd/controlplane/main.go
// changes.
//
// Per-host clients are cached and rebuilt only if a host's agent_url changes
// (an operator re-pointing a host at a new agent).
type Registry struct {
	q      *store.Queries
	token  string
	caFile string

	mu      sync.RWMutex
	clients map[string]cachedClient // host id (canonical string) -> client
}

type cachedClient struct {
	agentURL string
	client   *Client
}

// NewRegistry builds a Registry that authenticates every dialed agent with
// the shared bearer token, optionally pinning its TLS certificate against
// caFile (empty ⇒ system trust store; see NewPinned).
func NewRegistry(pool *pgxpool.Pool, token, caFile string) *Registry {
	return &Registry{q: store.New(pool), token: token, caFile: caFile, clients: make(map[string]cachedClient)}
}

// clientForHost returns the (cached) client for a host row, rebuilding it if
// the host's agent_url has changed since it was last cached.
func (r *Registry) clientForHost(host store.Host) (*Client, error) {
	key := hostKey(host.ID)
	r.mu.RLock()
	cached, ok := r.clients[key]
	r.mu.RUnlock()
	if ok && cached.agentURL == host.AgentUrl {
		return cached.client, nil
	}

	c, err := NewPinned(host.AgentUrl, r.token, r.caFile)
	if err != nil {
		return nil, fmt.Errorf("dial host %q: %w", host.Name, err)
	}
	r.mu.Lock()
	r.clients[key] = cachedClient{agentURL: host.AgentUrl, client: c}
	r.mu.Unlock()
	return c, nil
}

// clientForMachine resolves the node-agent client for the host a machine is
// assigned to.
func (r *Registry) clientForMachine(ctx context.Context, machineID string) (*Client, error) {
	var id pgtype.UUID
	if err := id.Scan(machineID); err != nil {
		return nil, fmt.Errorf("invalid machine id %q: %w", machineID, err)
	}
	host, err := r.q.GetMachineHost(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("resolve host for machine %s: %w", machineID, err)
	}
	return r.clientForHost(host)
}

// ClientForHostID resolves the node-agent client for a host by id directly —
// used by the scheduler to query capacity before any machine exists yet to
// join against.
func (r *Registry) ClientForHostID(ctx context.Context, hostID pgtype.UUID) (*Client, error) {
	host, err := r.q.GetHostByID(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("get host: %w", err)
	}
	return r.clientForHost(host)
}

func hostKey(id pgtype.UUID) string {
	b := id.Bytes
	return string(b[:])
}

// --- per-machine methods: same signatures as Client, satisfying every
// interface (machine.NodeClient, gateway/guestctl/injector's GuestDialer)
// that currently holds a single *Client. ---

// Ensure resolves id's host and issues PUT /v1/machines/{id}.
func (r *Registry) Ensure(ctx context.Context, id string, req agentapi.EnsureRequest) (agentapi.EnsureResponse, error) {
	c, err := r.clientForMachine(ctx, id)
	if err != nil {
		return agentapi.EnsureResponse{}, err
	}
	return c.Ensure(ctx, id, req)
}

// Stop resolves id's host and issues POST /v1/machines/{id}/stop.
func (r *Registry) Stop(ctx context.Context, id, mode string) error {
	c, err := r.clientForMachine(ctx, id)
	if err != nil {
		return err
	}
	return c.Stop(ctx, id, mode)
}

// Status resolves id's host and issues GET /v1/machines/{id}.
func (r *Registry) Status(ctx context.Context, id string) (agentapi.MachineStatus, error) {
	c, err := r.clientForMachine(ctx, id)
	if err != nil {
		return agentapi.MachineStatus{}, err
	}
	return c.Status(ctx, id)
}

// Destroy resolves id's host and issues DELETE /v1/machines/{id}.
func (r *Registry) Destroy(ctx context.Context, id string) error {
	c, err := r.clientForMachine(ctx, id)
	if err != nil {
		return err
	}
	return c.Destroy(ctx, id)
}

// DialGuest resolves machineID's host and opens the guest tunnel through it.
func (r *Registry) DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error) {
	c, err := r.clientForMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	return c.DialGuest(ctx, machineID, port)
}

// List fans GET /v1/machines out to every active host and merges the results,
// so the poller's running sweep sees every tracked machine regardless of which
// host it lives on. Fails fast on the first unreachable host: SweepRunning
// treats a List error as "apply the blip grace period to every running
// machine", which is the safe, conservative behavior when part of the fleet's
// status is unknown (better than a machine on a healthy host being silently
// dropped from byID and mistaken for "agent no longer tracks it").
func (r *Registry) List(ctx context.Context) ([]agentapi.MachineStatus, error) {
	hosts, err := r.q.ListActiveHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active hosts: %w", err)
	}
	var out []agentapi.MachineStatus
	for _, h := range hosts {
		c, err := r.clientForHost(h)
		if err != nil {
			return nil, err
		}
		sts, err := c.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list host %q: %w", h.Name, err)
		}
		out = append(out, sts...)
	}
	return out, nil
}

// Capacity resolves hostID's node-agent and queries its available resources
// (TAV-37), used by the scheduler before placing a new machine.
func (r *Registry) Capacity(ctx context.Context, hostID pgtype.UUID) (agentapi.CapacityResponse, error) {
	c, err := r.ClientForHostID(ctx, hostID)
	if err != nil {
		return agentapi.CapacityResponse{}, err
	}
	return c.Capacity(ctx, hostID)
}
