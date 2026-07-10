package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrNoCapacity is returned by Create when no active host has enough spare
// capacity for the requested machine shape (TAV-37: multi-host foundation).
var ErrNoCapacity = errors.New("machine: no host with sufficient capacity") // 503

// chooseHost picks the host a new machine should be placed on (TAV-37: this is
// the first real scheduling decision — every machine used to go to a single
// hardcoded host). With exactly one active host — today's default,
// single-KVM-host deployments — it is returned directly with no agent
// round-trip, preserving the pre-TAV-37 behavior exactly. With more than one
// active host, each is queried for its current capacity (GET /v1/capacity) and
// the machine is placed on the host with the most free memory that can fit the
// request (both vcpus and mem must fit); ties break on host name for
// determinism. A host whose agent can't be reached is skipped rather than
// failing the whole placement decision — the point of having more than one
// host is that one being down doesn't stop new machines from scheduling.
func (s *Service) chooseHost(ctx context.Context, res Resources) (store.Host, error) {
	hosts, err := s.q.ListActiveHosts(ctx)
	if err != nil {
		return store.Host{}, fmt.Errorf("list active hosts: %w", err)
	}
	if len(hosts) == 0 {
		return store.Host{}, ErrNoCapacity
	}
	if len(hosts) == 1 {
		return hosts[0], nil
	}

	var best store.Host
	var bestFreeMem int
	found := false
	for _, h := range hosts {
		c, err := s.nodes.Capacity(ctx, h.ID)
		if err != nil {
			slog.Warn("scheduler: capacity query failed, skipping host", "host", h.Name, "err", err)
			continue
		}
		freeVcpus := c.TotalVcpus - c.UsedVcpus
		freeMem := c.TotalMemMiB - c.UsedMemMiB
		if freeVcpus < res.Vcpus || freeMem < res.MemMiB {
			continue
		}
		if !found || freeMem > bestFreeMem || (freeMem == bestFreeMem && h.Name < best.Name) {
			best, bestFreeMem, found = h, freeMem, true
		}
	}
	if !found {
		return store.Host{}, ErrNoCapacity
	}
	return best, nil
}
