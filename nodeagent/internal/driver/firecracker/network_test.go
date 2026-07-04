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
	err := ensureNftTable("")
	if err == nil {
		t.Fatal("ensureNftTable(\"\") succeeded; want an explicit error")
	}
	if !strings.Contains(err.Error(), "AgentPort") {
		t.Fatalf("error %q does not point at Config.AgentPort", err)
	}
}
