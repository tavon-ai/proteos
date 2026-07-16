package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// Network policy modes (TAV-116). AllowAll is the default for every machine
// that has no network_policies row (see defaultNetworkPolicy). Re-exported
// from agentapi — the shared wire contract with the node-agent that actually
// enforces the policy — so callers only need to import this package.
const (
	NetworkPolicyAllowAll     = agentapi.NetworkPolicyAllowAll
	NetworkPolicyDenyAll      = agentapi.NetworkPolicyDenyAll
	NetworkPolicyAllowDomains = agentapi.NetworkPolicyAllowDomains
	NetworkPolicyDenyDomains  = agentapi.NetworkPolicyDenyDomains
)

// ErrInvalidNetworkPolicy is returned for an unknown mode or a malformed
// domain in the list. The HTTP layer maps it to 400.
var ErrInvalidNetworkPolicy = errors.New("machine: invalid network policy")

// validNetworkPolicyModes is the Go-side mirror of the network_policies.mode
// CHECK constraint.
var validNetworkPolicyModes = map[string]bool{
	NetworkPolicyAllowAll:     true,
	NetworkPolicyDenyAll:      true,
	NetworkPolicyAllowDomains: true,
	NetworkPolicyDenyDomains:  true,
}

// domainPattern accepts a bare DNS hostname (e.g. "github.com",
// "api.github.com"). No scheme, path, port, or wildcard — resolution
// (resolveDomainIPs on the node-agent) looks each name up directly, so a
// pattern it can't look up would silently match nothing.
var domainPattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// NetworkPolicy is the API/service-level view of a machine's network policy
// (TAV-116). Domains is meaningful only for the two domain-list modes.
type NetworkPolicy struct {
	MachineID string
	Mode      string
	Domains   []string
	UpdatedAt time.Time
}

// defaultNetworkPolicy is what every machine has until a user explicitly sets
// one: unrestricted network access.
func defaultNetworkPolicy(machineID string) NetworkPolicy {
	return NetworkPolicy{MachineID: machineID, Mode: NetworkPolicyAllowAll, Domains: []string{}}
}

// ValidateNetworkPolicy checks a mode/domains pair before it is persisted: mode
// must be one of the four constants, and (only for the domain-list modes)
// every domain must look like a bare DNS hostname.
func ValidateNetworkPolicy(mode string, domains []string) error {
	if !validNetworkPolicyModes[mode] {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidNetworkPolicy, mode)
	}
	if mode != NetworkPolicyAllowDomains && mode != NetworkPolicyDenyDomains {
		return nil
	}
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if d == "" || !domainPattern.MatchString(d) {
			return fmt.Errorf("%w: invalid domain %q", ErrInvalidNetworkPolicy, d)
		}
	}
	return nil
}

// NetworkPolicyFor returns the machine's configured policy, or the allow_all
// default if none has been set. Missing/foreign machine ⇒ ErrNoMachine.
func (s *Service) NetworkPolicyFor(ctx context.Context, userID, machineID pgtype.UUID) (NetworkPolicy, error) {
	if _, err := s.getOwned(ctx, userID, machineID); err != nil {
		return NetworkPolicy{}, err
	}
	return s.getNetworkPolicy(ctx, machineID)
}

// getNetworkPolicy loads the row without an ownership check — used internally
// by ensureOnAgent, which already holds a resolved machine.
func (s *Service) getNetworkPolicy(ctx context.Context, machineID pgtype.UUID) (NetworkPolicy, error) {
	row, err := s.q.GetNetworkPolicy(ctx, machineID)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultNetworkPolicy(UUIDString(machineID)), nil
	}
	if err != nil {
		return NetworkPolicy{}, err
	}
	return toNetworkPolicy(row), nil
}

// SetNetworkPolicy validates and persists a machine's network policy. It takes
// effect the next time the machine (re)boots on the node-agent (Create/Start) —
// like a resource-spec override, it is not hot-applied to an already-running
// machine. Missing/foreign machine ⇒ ErrNoMachine; invalid input ⇒
// ErrInvalidNetworkPolicy.
func (s *Service) SetNetworkPolicy(ctx context.Context, userID, machineID pgtype.UUID, mode string, domains []string) (NetworkPolicy, error) {
	if _, err := s.getOwned(ctx, userID, machineID); err != nil {
		return NetworkPolicy{}, err
	}
	if err := ValidateNetworkPolicy(mode, domains); err != nil {
		return NetworkPolicy{}, err
	}
	if domains == nil {
		domains = []string{}
	}
	domainsJSON, err := json.Marshal(domains)
	if err != nil {
		return NetworkPolicy{}, fmt.Errorf("marshal domains: %w", err)
	}
	row, err := s.q.UpsertNetworkPolicy(ctx, store.UpsertNetworkPolicyParams{
		MachineID: machineID,
		Mode:      mode,
		Domains:   domainsJSON,
	})
	if err != nil {
		return NetworkPolicy{}, fmt.Errorf("upsert network policy: %w", err)
	}
	return toNetworkPolicy(row), nil
}

// DeleteNetworkPolicy resets a machine to the default policy (allow_all) by
// dropping its row. Idempotent. Missing/foreign machine ⇒ ErrNoMachine.
func (s *Service) DeleteNetworkPolicy(ctx context.Context, userID, machineID pgtype.UUID) error {
	if _, err := s.getOwned(ctx, userID, machineID); err != nil {
		return err
	}
	if err := s.q.DeleteNetworkPolicy(ctx, machineID); err != nil {
		return fmt.Errorf("delete network policy: %w", err)
	}
	return nil
}

// toNetworkPolicy renders a store row as the service-level type. domains is
// never null (the column defaults to '[]'::jsonb), but an unmarshal failure
// degrades to an empty list rather than failing the read.
func toNetworkPolicy(row store.NetworkPolicy) NetworkPolicy {
	var domains []string
	_ = json.Unmarshal(row.Domains, &domains)
	if domains == nil {
		domains = []string{}
	}
	np := NetworkPolicy{MachineID: UUIDString(row.MachineID), Mode: row.Mode, Domains: domains}
	if row.UpdatedAt.Valid {
		np.UpdatedAt = row.UpdatedAt.Time
	}
	return np
}
