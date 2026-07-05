package config

import (
	"strings"
	"testing"
)

// setBaseEnv provides the minimum env Load requires, clearing the knobs these
// tests exercise so the surrounding shell can't leak into them.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PROTEOS_AGENT_TOKEN", "test-token")
	for _, k := range []string{
		"PROTEOS_AGENT_ADDR", "PROTEOS_AGENT_TLS_CERT", "PROTEOS_AGENT_TLS_KEY",
		"PROTEOS_AGENT_INSECURE_HTTP", "PROTEOS_AGENT_MGMT_IFACES",
	} {
		t.Setenv(k, "")
	}
}

// TAV-27: the agent API carries volume keys, so serving plain HTTP on a
// non-loopback address must be rejected unless dev explicitly opts out.
// Loopback addresses stay allowed without certs (dev stacks).
func TestLoadRequiresTLSOffLoopback(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		insecure string
		wantErr  bool
	}{
		{"default all-interfaces rejected", "", "", true},
		{"explicit wildcard rejected", ":9090", "", true},
		{"lan address rejected", "192.168.2.84:9090", "", true},
		{"loopback ok", "127.0.0.1:9090", "", false},
		{"localhost ok", "localhost:9090", "", false},
		{"wildcard with opt-out ok", ":9090", "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			if tc.addr != "" {
				t.Setenv("PROTEOS_AGENT_ADDR", tc.addr)
			}
			if tc.insecure != "" {
				t.Setenv("PROTEOS_AGENT_INSECURE_HTTP", tc.insecure)
			}
			_, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load(addr=%q) succeeded; want TLS-required error", tc.addr)
				}
				if !strings.Contains(err.Error(), "TLS") {
					t.Fatalf("error %q does not mention TLS", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(addr=%q): %v", tc.addr, err)
			}
		})
	}
}

// The management interface list feeds the fail-closed nftables input chain:
// the default must include tailscale0 (the control plane's arrival path — the
// July 2026 outage) alongside the egress token, and a list that resolves to
// nothing must fail loudly rather than firewall everyone out.
func TestLoadMgmtIfaces(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("PROTEOS_AGENT_INSECURE_HTTP", "1")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.MgmtIfaces) != 2 || c.MgmtIfaces[0] != "egress" || c.MgmtIfaces[1] != "tailscale0" {
		t.Errorf("default MgmtIfaces = %v, want [egress tailscale0]", c.MgmtIfaces)
	}

	t.Setenv("PROTEOS_AGENT_MGMT_IFACES", " eth0 , wg0 ")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load with custom ifaces: %v", err)
	}
	if len(c.MgmtIfaces) != 2 || c.MgmtIfaces[0] != "eth0" || c.MgmtIfaces[1] != "wg0" {
		t.Errorf("MgmtIfaces = %v, want [eth0 wg0] (trimmed)", c.MgmtIfaces)
	}

	t.Setenv("PROTEOS_AGENT_MGMT_IFACES", " , ,")
	if _, err := Load(); err == nil {
		t.Error("Load with an all-empty interface list succeeded; want an error (fail-closed chain would lock everyone out)")
	}
}
