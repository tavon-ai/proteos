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
	// Forward chain with default policy accept (per-tap chains enforce deny).
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
	if err := run("sysctl", "-wq", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	egress, err := egressDev()
	if err != nil {
		return err
	}

	// Default-deny egress, evaluated in order on the forward hook for this tap:
	//   1. allow established/related return traffic
	//   2. DROP guest → private ranges (host, agent, control plane, peers)
	//   3. allow guest → anywhere else (the internet)
	// Rules are tagged with a comment carrying the tap name so Destroy can find
	// and delete exactly this machine's rules.
	tag := "proteos:" + tap
	rules := [][]string{
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ct", "state", "established,related", "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "oifname", tap, "ct", "state", "established,related", "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "10.0.0.0/8", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "172.16.0.0/12", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "192.168.0.0/16", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "ip", "daddr", "169.254.0.0/16", "drop", "comment", tag},
		{"add", "rule", "ip", nftTable, "forward", "iifname", tap, "oifname", egress, "accept", "comment", tag},
		{"add", "rule", "ip", nftTable, "postrouting", "ip", "saddr", guestCIDR, "oifname", egress, "masquerade", "comment", tag},
	}
	for _, r := range rules {
		if err := run("nft", r...); err != nil {
			return err
		}
	}
	return nil
}

// teardownTap removes this machine's nft rules (by comment tag) and deletes the
// tap. Best-effort: missing objects are not an error.
func teardownTap(tap string) {
	// Delete every rule tagged for this tap from both chains.
	deleteRulesByComment("forward", "proteos:"+tap)
	deleteRulesByComment("postrouting", "proteos:"+tap)
	if linkExists(tap) {
		_ = run("ip", "link", "del", tap)
	}
}

// deleteRulesByComment removes all rules in a chain whose comment matches tag.
// nft has no "delete by comment", so we list handles and delete each.
func deleteRulesByComment(chain, tag string) {
	out, err := runOut("nft", "-a", "list", "chain", "ip", nftTable, chain)
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
		_ = run("nft", "delete", "rule", "ip", nftTable, chain, "handle", handle)
	}
}

func linkExists(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}
