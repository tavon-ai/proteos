package machine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

func TestValidateNetworkPolicy(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		domains []string
		wantErr bool
	}{
		{"allow_all", machine.NetworkPolicyAllowAll, nil, false},
		{"deny_all", machine.NetworkPolicyDenyAll, nil, false},
		{"allow_domains valid", machine.NetworkPolicyAllowDomains, []string{"github.com", "api.github.com"}, false},
		{"deny_domains valid", machine.NetworkPolicyDenyDomains, []string{"evil.example"}, false},
		{"allow_domains empty list", machine.NetworkPolicyAllowDomains, nil, false},
		{"unknown mode", "block_everything", nil, true},
		{"empty mode", "", nil, true},
		{"allow_domains bad domain", machine.NetworkPolicyAllowDomains, []string{"not a domain"}, true},
		{"allow_domains url not hostname", machine.NetworkPolicyAllowDomains, []string{"https://github.com"}, true},
		{"deny_domains blank entry", machine.NetworkPolicyDenyDomains, []string{"github.com", "  "}, true},
		// A non-domain mode ignores whatever is in domains — it isn't persisted
		// meaningfully, so garbage there shouldn't block allow_all/deny_all.
		{"allow_all ignores garbage domains", machine.NetworkPolicyAllowAll, []string{"not a domain"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := machine.ValidateNetworkPolicy(c.mode, c.domains)
			if c.wantErr && err == nil {
				t.Fatalf("ValidateNetworkPolicy(%q, %v) = nil, want error", c.mode, c.domains)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("ValidateNetworkPolicy(%q, %v) = %v, want nil", c.mode, c.domains, err)
			}
			if c.wantErr && err != nil && c.mode != "" {
				if got := machine.ErrInvalidNetworkPolicy; got == nil {
					t.Fatal("ErrInvalidNetworkPolicy is nil")
				}
			}
		})
	}
}

func TestNetworkPolicyDefaultsToAllowAll(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policy, err := h.svc.NetworkPolicyFor(ctx, h.userID, m.ID)
	if err != nil {
		t.Fatalf("NetworkPolicyFor: %v", err)
	}
	if policy.Mode != machine.NetworkPolicyAllowAll {
		t.Fatalf("default mode = %q, want allow_all", policy.Mode)
	}
	if len(policy.Domains) != 0 {
		t.Fatalf("default domains = %v, want empty", policy.Domains)
	}

	// TAV-116: a machine with no policy configured is wired to the agent as
	// allow_all on create.
	ensure := h.agent.lastEnsure[machine.UUIDString(m.ID)]
	if ensure.NetworkPolicy == nil || ensure.NetworkPolicy.Mode != agentapi.NetworkPolicyAllowAll {
		t.Fatalf("ensure request network policy = %+v, want allow_all", ensure.NetworkPolicy)
	}
}

func TestSetAndGetNetworkPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	saved, err := h.svc.SetNetworkPolicy(ctx, h.userID, m.ID, machine.NetworkPolicyAllowDomains, []string{"github.com", "githubusercontent.com"})
	if err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	if saved.Mode != machine.NetworkPolicyAllowDomains {
		t.Fatalf("saved mode = %q, want allow_domains", saved.Mode)
	}
	if len(saved.Domains) != 2 {
		t.Fatalf("saved domains = %v, want 2 entries", saved.Domains)
	}

	got, err := h.svc.NetworkPolicyFor(ctx, h.userID, m.ID)
	if err != nil {
		t.Fatalf("NetworkPolicyFor: %v", err)
	}
	if got.Mode != machine.NetworkPolicyAllowDomains || len(got.Domains) != 2 {
		t.Fatalf("read-back policy = %+v, want allow_domains with 2 domains", got)
	}

	// Invalid input is rejected and does not clobber the saved policy.
	if _, err := h.svc.SetNetworkPolicy(ctx, h.userID, m.ID, "not_a_mode", nil); !errors.Is(err, machine.ErrInvalidNetworkPolicy) {
		t.Fatalf("SetNetworkPolicy(bad mode): got %v, want ErrInvalidNetworkPolicy", err)
	}
	got2, err := h.svc.NetworkPolicyFor(ctx, h.userID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Mode != machine.NetworkPolicyAllowDomains {
		t.Fatalf("policy after rejected update = %q, want it unchanged (allow_domains)", got2.Mode)
	}
}

func TestSetNetworkPolicyAppliedOnNextEnsure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idStr := machine.UUIDString(m.ID)

	if _, err := h.svc.SetNetworkPolicy(ctx, h.userID, m.ID, machine.NetworkPolicyDenyDomains, []string{"evil.example"}); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}

	// Stop then start to trigger a fresh ensureOnAgent call (TAV-116: policy
	// changes apply on the machine's next (re)boot, not hot to a running one).
	h.agent.SetStatus(idStr, agentapi.StateRunning, "", "172.30.0.2")
	h.poller.AdvanceTransitional(ctx)
	if _, err := h.svc.Stop(ctx, h.userID, m.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	h.agent.SetStatus(idStr, agentapi.StateStopped, "", "")
	h.poller.AdvanceTransitional(ctx)
	if _, err := h.svc.Start(ctx, h.userID, m.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	ensure := h.agent.lastEnsure[idStr]
	if ensure.NetworkPolicy == nil {
		t.Fatal("ensure request carries no network policy")
	}
	if ensure.NetworkPolicy.Mode != agentapi.NetworkPolicyDenyDomains {
		t.Fatalf("ensure network policy mode = %q, want deny_domains", ensure.NetworkPolicy.Mode)
	}
	if len(ensure.NetworkPolicy.Domains) != 1 || ensure.NetworkPolicy.Domains[0] != "evil.example" {
		t.Fatalf("ensure network policy domains = %v, want [evil.example]", ensure.NetworkPolicy.Domains)
	}
}

func TestDeleteNetworkPolicyResetsToDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetNetworkPolicy(ctx, h.userID, m.ID, machine.NetworkPolicyDenyAll, nil); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	if err := h.svc.DeleteNetworkPolicy(ctx, h.userID, m.ID); err != nil {
		t.Fatalf("DeleteNetworkPolicy: %v", err)
	}
	// Idempotent: deleting again is not an error.
	if err := h.svc.DeleteNetworkPolicy(ctx, h.userID, m.ID); err != nil {
		t.Fatalf("second DeleteNetworkPolicy: %v", err)
	}

	got, err := h.svc.NetworkPolicyFor(ctx, h.userID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != machine.NetworkPolicyAllowAll {
		t.Fatalf("mode after delete = %q, want allow_all", got.Mode)
	}
}

func TestNetworkPolicyOwnershipRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	m, err := h.svc.Create(ctx, h.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 314, Login: "intruder"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.NetworkPolicyFor(ctx, other.ID, m.ID); err != machine.ErrNoMachine {
		t.Fatalf("foreign NetworkPolicyFor: got %v, want ErrNoMachine", err)
	}
	if _, err := h.svc.SetNetworkPolicy(ctx, other.ID, m.ID, machine.NetworkPolicyDenyAll, nil); err != machine.ErrNoMachine {
		t.Fatalf("foreign SetNetworkPolicy: got %v, want ErrNoMachine", err)
	}
	if err := h.svc.DeleteNetworkPolicy(ctx, other.ID, m.ID); err != machine.ErrNoMachine {
		t.Fatalf("foreign DeleteNetworkPolicy: got %v, want ErrNoMachine", err)
	}
}
