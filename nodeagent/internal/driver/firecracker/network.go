//go:build firecracker && linux

package firecracker

import (
	"fmt"
	"os/exec"
	"strings"
)

// Networking mirrors the spike's tap + NAT setup (03-network.sh / lib.sh), but
// tightens egress to the plan's "basic default-deny": each guest can reach the
// internet (masqueraded) but NOT the host, the node-agent, the control plane,
// or other RFC1918/link-local ranges. The node-agent runs as root, so we invoke
// `ip` and `nft` directly (no sudo, unlike the spike).
//
// Per-tap rules live in their own nft chain so Destroy can drop exactly one
// machine's policy without disturbing the others.

const (
	nftTable = "proteos"
)

// run executes a command, returning its combined output on failure for context.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runOut executes a command and returns trimmed stdout.
func runOut(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// egressDev returns the host's default-route interface (the one that reaches the
// internet), used as the NAT/forward egress device.
func egressDev() (string, error) {
	out, err := runOut("ip", "route", "get", "8.8.8.8")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("could not determine egress interface from %q", out)
}

// ensureNftTable creates the base table + a forward chain with a NAT
// postrouting chain once. Idempotent: `nft add` of an existing object is a
// no-op error we tolerate by pre-deleting on first setup.
func ensureNftTable() error {
	// `add table` is idempotent in nftables; chains likewise.
	if err := run("nft", "add", "table", "ip", nftTable); err != nil {
		return err
	}
	// Input chain (default policy accept; per-tap rules drop guest→host). The
	// forward hook never sees host-destined traffic, so guest→host services
	// (the node-agent) can only be blocked here.
	if err := run("nft", "add", "chain", "ip", nftTable, "input",
		"{ type filter hook input priority 0 ; }"); err != nil {
		return err
	}
	// Forward chain with default policy accept (per-tap rules enforce deny).
	if err := run("nft", "add", "chain", "ip", nftTable, "forward",
		"{ type filter hook forward priority 0 ; }"); err != nil {
		return err
	}
	if err := run("nft", "add", "chain", "ip", nftTable, "postrouting",
		"{ type nat hook postrouting priority 100 ; }"); err != nil {
		return err
	}
	return nil
}

// setupTap creates the tap owned by the agent, addresses it with the gateway IP,
// brings it up, enables forwarding, and installs the default-deny egress policy
// plus the masquerade rule for this guest.
func setupTap(tap, gatewayCIDR, guestCIDR string) error {
	if err := ensureNftTable(); err != nil {
		return err
	}

	// Idempotency: drop any rules a prior boot of this tap left behind. stop is
	// a plain shutdown that leaves the tap + rules in place, so start re-runs
	// setupTap; without this, every restart would append duplicate rules.
	deleteTapRules(tap)

	// Tap device (idempotent: skip if it already exists).
	if !linkExists(tap) {
		if err := run("ip", "tuntap", "add", tap, "mode", "tap"); err != nil {
			return err
		}
	}
	if err := run("ip", "addr", "replace", gatewayCIDR, "dev", tap); err != nil {
		return err
	}
	if err := run("ip", "link", "set", tap, "up"); err != nil {
		return err
	}
	// Pin a /32 route to THIS guest via THIS tap. Every machine shares the same
	// gateway /24, so each tap gets an identical connected 172.30.0.0/24 route;
	// with two+ taps the kernel would send a guest's return traffic out whichever
	// tap was added first, so only the first machine could reach the internet
	// (the 2nd+ machine's DNS/egress replies never arrive). The per-guest /32 is
	// more specific than the /24 and disambiguates the return path. It is removed
	// with the tap on teardown.
	if err := run("ip", "route", "replace", guestCIDR, "dev", tap); err != nil {
		return err
	}
	if err := run("sysctl", "-wq", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	egress, err := egressDev()
	if err != nil {
		return err
	}

	for _, r := range egressRules(tap, guestCIDR, egress) {
		if err := run("nft", r...); err != nil {
			return err
		}
	}

	// Punch this tap's forwarded traffic through the system FORWARD chain. A
	// default-deny FORWARD policy (Docker, ufw, or a manual `iptables -P FORWARD
	// DROP`) lives in the iptables-managed `ip filter` table — a separate base
	// chain whose drop our `proteos` accept cannot override, so the accept has
	// to be added there too. No-op on hosts without that chain.
	return allowForwardInFilter(tap, egress)
}

// commentTag is the nft comment value used to tag a tap's rules. It contains a
// ':', which nft rejects in an unquoted token, so callers that pass it to nft
// must quote it (see egressRules); deleteRulesByComment matches the unquoted
// value as it appears inside the quotes in `nft list` output.
func commentTag(tap string) string { return "proteos:" + tap }

// egressRules builds the ordered nft rule argument lists implementing the
// default-deny policy for one tap. It is pure (no I/O) so it can be unit tested.
//
// input hook (guest → host-local services):
//   - allow established/related return traffic (Phase 3 host→guest terminal)
//   - DROP everything else — the guest must not reach the node-agent etc.
//
// forward hook (guest → routed destinations):
//   - allow established/related return traffic
//   - DROP guest → private ranges (host, agent, control plane, peers)
//   - allow guest → anywhere else (the internet)
//
// postrouting: masquerade the guest to the internet.
//
// Every rule carries a `counter` (so `nft list table ip proteos` shows per-rule
// hit counts for debugging) and a comment tag (the tap name) so teardownTap can
// find and delete exactly this machine's rules. The comment is wrapped in
// literal double quotes because the tag contains a ':' that nft rejects
// unquoted; we exec nft directly (no shell), so the quotes must be in the
// argument itself.
func egressRules(tap, guestCIDR, egress string) [][]string {
	tag := `"` + commentTag(tap) + `"`
	return [][]string{
		// input: deny guest → host, except return traffic.
		{"add", "rule", "ip", nftTable, "input", "iifname", tap, "ct", "state", "established,related", "counter", "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "input", "iifname", tap, "counter", "drop", "comment", tag},
		// forward: default-deny egress.
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ct", "state", "established,related", "counter", "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "oifname", tap, "ct", "state", "established,related", "counter", "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "10.0.0.0/8", "counter", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "172.16.0.0/12", "counter", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "192.168.0.0/16", "counter", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "169.254.0.0/16", "counter", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "oifname", egress, "counter", "accept", "comment", tag},
		// postrouting: NAT the guest out to the internet.
		{"add", "rule", "ip", nftTable, "postrouting", "ip", "saddr", guestCIDR, "oifname", egress, "counter", "masquerade", "comment", tag},
	}
}

// allowForwardInFilter adds accept rules for this tap's forwarded traffic to the
// iptables-managed `ip filter` FORWARD chain, so a default-deny FORWARD policy
// there (Docker/ufw/manual) doesn't silently drop guest egress. Our proteos
// accept can't override another base chain's drop — the accept must live in that
// chain. No-op when the chain is absent (no such policy to defeat). The rules
// carry our comment tag so deleteTapRules removes them like the rest.
func allowForwardInFilter(tap, egress string) error {
	if !nftChainExists("filter", "FORWARD") {
		return nil
	}
	tag := `"` + commentTag(tap) + `"`
	if err := run("nft", "add", "rule", "ip", "filter", "FORWARD",
		"iifname", tap, "oifname", egress, "counter", "accept", "comment", tag); err != nil {
		return err
	}
	return run("nft", "add", "rule", "ip", "filter", "FORWARD",
		"iifname", egress, "oifname", tap, "ct", "state", "established,related", "counter", "accept", "comment", tag)
}

// teardownTap removes this machine's nft rules and deletes the tap. Best-effort:
// missing objects are not an error.
func teardownTap(tap string) {
	deleteTapRules(tap)
	if linkExists(tap) {
		_ = run("ip", "link", "del", tap)
	}
}

// deleteTapRules removes every rule tagged for this tap, across our proteos
// chains and the system filter FORWARD chain. Used by teardown and to make
// setupTap idempotent across a stop→start.
func deleteTapRules(tap string) {
	tag := commentTag(tap)
	deleteRulesByComment(nftTable, "input", tag)
	deleteRulesByComment(nftTable, "forward", tag)
	deleteRulesByComment(nftTable, "postrouting", tag)
	deleteRulesByComment("filter", "FORWARD", tag)
}

// deleteRulesByComment removes all rules in a table's chain whose comment
// matches tag. nft has no "delete by comment", so we list handles and delete
// each. Best-effort: a missing table/chain just yields no matches.
func deleteRulesByComment(table, chain, tag string) {
	out, err := runOut("nft", "-a", "list", "chain", "ip", table, chain)
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `comment "`+tag+`"`) {
			continue
		}
		// Lines end with: ... # handle N
		idx := strings.LastIndex(line, "# handle ")
		if idx < 0 {
			continue
		}
		handle := strings.TrimSpace(line[idx+len("# handle "):])
		_ = run("nft", "delete", "rule", "ip", table, chain, "handle", handle)
	}
}

// nftChainExists reports whether the given ip-family table/chain is present.
func nftChainExists(table, chain string) bool {
	return exec.Command("nft", "list", "chain", "ip", table, chain).Run() == nil
}

func linkExists(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}
