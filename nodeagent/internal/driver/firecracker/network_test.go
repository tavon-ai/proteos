//go:build firecracker && linux

package firecracker

import (
	"strings"
	"testing"
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
