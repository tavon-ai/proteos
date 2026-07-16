//go:build firecracker && linux

package firecracker

import (
	"strings"
	"testing"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// commentArg returns the value following "comment" in an nft rule arg list, or
// "" if the rule carries no comment.
func commentArg(rule []string) string {
	for i := 0; i+1 < len(rule); i++ {
		if rule[i] == "comment" {
			return rule[i+1]
		}
	}
	return ""
}

// Regression for the nft "unexpected colon" boot failure: the comment tag
// contains a ':', which nft rejects in an unquoted token. egressRules must wrap
// it in literal double quotes (we exec nft directly, so the quotes have to be
// in the argument, not added by a shell).
func TestEgressRulesQuoteCommentTag(t *testing.T) {
	const tap = "tape87d0754"
	rules := egressRules(tap, "172.30.0.2/24", "eth0")
	if len(rules) == 0 {
		t.Fatal("egressRules returned no rules")
	}

	want := `"` + commentTag(tap) + `"`
	for i, r := range rules {
		c := commentArg(r)
		if c == "" {
			t.Errorf("rule %d %v: no comment tag (teardown could not find it)", i, r)
			continue
		}
		if c != want {
			t.Errorf("rule %d comment = %q, want %q", i, c, want)
		}
		if !strings.HasPrefix(c, `"`) || !strings.HasSuffix(c, `"`) {
			t.Errorf("rule %d comment %q is not quoted; nft rejects the ':' unquoted", i, c)
		}
	}
}

// ruleHas reports whether token appears anywhere in the rule arg list.
func ruleHas(rule []string, token string) bool {
	for _, a := range rule {
		if a == token {
			return true
		}
	}
	return false
}

// Regression for "guest could reach the node-agent on the gateway IP": the
// forward hook never sees host-destined traffic, so there must be an input-hook
// drop for the tap. We also require the established/related accept so Phase 3's
// host→guest terminal keeps working.
func TestEgressRulesDenyGuestToHost(t *testing.T) {
	rules := egressRules("tape87d0754", "172.30.0.2/32", "eth0")

	var inputDrop, inputReturn bool
	for _, r := range rules {
		if !ruleHas(r, "input") {
			continue
		}
		if ruleHas(r, "drop") {
			inputDrop = true
		}
		if ruleHas(r, "established,related") && ruleHas(r, "accept") {
			inputReturn = true
		}
	}
	if !inputDrop {
		t.Error("no input-hook drop for the tap: guest could reach host services (node-agent)")
	}
	if !inputReturn {
		t.Error("no input-hook established/related accept: host→guest return traffic would be dropped")
	}
}

// The value nft stores (the comment with its surrounding quotes stripped) must
// equal the tag teardownTap searches for via deleteRulesByComment, or cleanup
// would never match these rules.
func TestEgressCommentMatchesTeardownTag(t *testing.T) {
	const tap = "tape87d0754"
	rules := egressRules(tap, "172.30.0.2/24", "eth0")

	stored := strings.Trim(commentArg(rules[0]), `"`)
	if stored != commentTag(tap) {
		t.Fatalf("stored comment %q != teardown tag %q; teardownTap would not delete these rules",
			stored, commentTag(tap))
	}
}

// An empty agent port must be rejected before any nft invocation: it would
// render `tcp dport ` (an nft syntax error) and, if skipped instead, the
// fail-closed input chain would firewall the control plane out of the agent
// API. Regression test for the integration suite booting with a zero-value
// Config.AgentPort.
func TestEnsureNftTableRejectsEmptyAgentPort(t *testing.T) {
	err := ensureNftTable("", []string{"eth0"})
	if err == nil {
		t.Fatal("ensureNftTable with empty port succeeded; want an explicit error")
	}
	if !strings.Contains(err.Error(), "AgentPort") {
		t.Fatalf("error %q does not point at Config.AgentPort", err)
	}
}

// An empty management interface list must be rejected the same way: the
// default-drop input chain with no interface accepts would firewall everyone
// (including the control plane) out of the host.
func TestEnsureNftTableRejectsEmptyMgmtIfaces(t *testing.T) {
	err := ensureNftTable("9090", nil)
	if err == nil {
		t.Fatal("ensureNftTable with no mgmt interfaces succeeded; want an explicit error")
	}
	if !strings.Contains(err.Error(), "MgmtIfaces") {
		t.Fatalf("error %q does not point at Config.MgmtIfaces", err)
	}
}

// Regression for the July 2026 machine-create outage: the input chain allowed
// SSH and the agent port only on the egress interface, but the control plane's
// requests arrive over tailscale — every management interface in the list must
// get both accepts, and the rules must carry the quoted global tag so the
// idempotent replace on restart can find them.
func TestMgmtInputRulesCoverEveryInterface(t *testing.T) {
	ifaces := []string{"eth0", "tailscale0"}
	rules := mgmtInputRules("9090", ifaces)

	want := `"` + nftGlobalTag + `"`
	for i, r := range rules {
		if c := commentArg(r); c != want {
			t.Errorf("rule %d comment = %q, want %q", i, c, want)
		}
	}

	for _, ifc := range ifaces {
		for _, port := range []string{"22", "9090"} {
			found := false
			for _, r := range rules {
				if ruleHas(r, "iifname") && ruleHas(r, ifc) && ruleHas(r, port) && ruleHas(r, "accept") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("no accept rule for %s dport %s; that interface's traffic would hit the default drop", ifc, port)
			}
		}
	}
}

// forwardRulesWithVerdict returns the subset of rules that are in the forward
// chain and end in the given verdict ("accept" or "drop").
func forwardRulesWithVerdict(rules [][]string, verdict string) [][]string {
	var out [][]string
	for _, r := range rules {
		if ruleHas(r, "forward") && ruleHas(r, verdict) {
			out = append(out, r)
		}
	}
	return out
}

// TAV-116: mode "" (a pre-TAV-116/legacy record) must behave exactly like
// allow_all — networkPolicyRules is what setupTap actually calls, and it must
// not regress the existing default-deny-private/allow-rest behavior egressRules
// documents and TestEgressRulesDenyGuestToHost etc. exercise.
func TestNetworkPolicyRulesEmptyModeMatchesAllowAll(t *testing.T) {
	empty := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", "", nil, nil)
	allowAll := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", api.NetworkPolicyAllowAll, nil, nil)
	if len(empty) != len(allowAll) {
		t.Fatalf("mode \"\" produced %d rules, allow_all produced %d", len(empty), len(allowAll))
	}
	for i := range empty {
		if strings.Join(empty[i], " ") != strings.Join(allowAll[i], " ") {
			t.Errorf("rule %d differs: %q vs %q", i, empty[i], allowAll[i])
		}
	}
}

// deny_all must not accept a guest's forward traffic to the egress interface
// (or anywhere else) — the only forward accepts left are the established/
// related return-traffic ones (harmless: nothing gets established without an
// initial accept), and every other forward-hook drop rule is unconditional
// (matching the tap alone, no daddr/oifname carve-out).
func TestNetworkPolicyRulesDenyAll(t *testing.T) {
	rules := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", api.NetworkPolicyDenyAll, nil, nil)
	for _, r := range forwardRulesWithVerdict(rules, "accept") {
		if !ruleHas(r, "established,related") {
			t.Errorf("deny_all has a non-established forward accept: %v", r)
		}
	}
}

// allow_domains must accept forward traffic only to the resolved allow-list
// IPs (to the egress interface), and must not carry the allow_all catch-all
// (an unconditional iifname-tap forward accept with no daddr).
func TestNetworkPolicyRulesAllowDomains(t *testing.T) {
	allowIPs := []string{"93.184.216.34"}
	rules := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", api.NetworkPolicyAllowDomains, allowIPs, nil)

	found := false
	for _, r := range forwardRulesWithVerdict(rules, "accept") {
		if ruleHas(r, "established,related") {
			continue
		}
		if !ruleHas(r, "ip") || !ruleHas(r, "daddr") || !ruleHas(r, allowIPs[0]) {
			t.Errorf("allow_domains forward accept without a daddr allowlist match: %v", r)
			continue
		}
		found = true
	}
	if !found {
		t.Error("no forward accept for the allow-listed IP")
	}
}

// deny_domains must drop forward traffic to the deny-list IPs specifically,
// while still accepting everything else (the allow_all-style catch-all).
func TestNetworkPolicyRulesDenyDomains(t *testing.T) {
	denyIPs := []string{"93.184.216.34"}
	rules := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", api.NetworkPolicyDenyDomains, nil, denyIPs)

	var dropsDeniedIP, hasCatchAll bool
	for _, r := range forwardRulesWithVerdict(rules, "drop") {
		if ruleHas(r, denyIPs[0]) {
			dropsDeniedIP = true
		}
	}
	for _, r := range forwardRulesWithVerdict(rules, "accept") {
		if !ruleHas(r, "established,related") && !ruleHas(r, denyIPs[0]) {
			hasCatchAll = true
		}
	}
	if !dropsDeniedIP {
		t.Error("no forward drop for the deny-listed IP")
	}
	if !hasCatchAll {
		t.Error("no forward catch-all accept; deny_domains should allow everything else")
	}
}

// Every mode must still carry the private-range drops and the input-hook
// guest→host deny — TestEgressRulesDenyGuestToHost already covers allow_all;
// this is the regression for the other three modes.
func TestNetworkPolicyRulesAlwaysDenyPrivateRanges(t *testing.T) {
	for _, mode := range []string{api.NetworkPolicyAllowAll, api.NetworkPolicyDenyAll, api.NetworkPolicyAllowDomains, api.NetworkPolicyDenyDomains} {
		rules := networkPolicyRules("tape87d0754", "172.30.0.2/24", "eth0", mode, nil, nil)
		for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
			found := false
			for _, r := range forwardRulesWithVerdict(rules, "drop") {
				if ruleHas(r, cidr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("mode %q: no forward drop for private range %s", mode, cidr)
			}
		}
	}
}

// resolveDomainIPs must degrade gracefully (empty, no panic/error) for a
// domain that cannot resolve, so one bad entry in the list doesn't break the
// rest of setupTap.
func TestResolveDomainIPsUnresolvable(t *testing.T) {
	ips := resolveDomainIPs([]string{"this-domain-should-not-resolve.invalid"})
	if len(ips) != 0 {
		t.Errorf("resolveDomainIPs of an unresolvable domain = %v, want empty", ips)
	}
}
