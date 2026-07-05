// Package config parses node-agent configuration from the environment. As with
// the control plane, all runtime knobs are resolved here into a typed Config so
// the rest of the daemon never reads os.Getenv ad hoc.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// Config is the fully-resolved node-agent configuration.
type Config struct {
	// Addr is the host:port the agent's HTTP API listens on. This should be a
	// private address reachable only by the control plane.
	Addr string

	// Token is the shared bearer token. The control plane must present the same
	// value; both sides compare in constant time. Required (non-empty).
	Token string

	// TLSCert / TLSKey, when both set, make the agent serve HTTPS instead of
	// plain HTTP. The channel carries per-machine LUKS volume keys and the
	// bearer token, so TLS is mandatory (TAV-27): startup fails without certs
	// unless InsecureHTTP explicitly opts out.
	TLSCert string
	TLSKey  string

	// InsecureHTTP (PROTEOS_AGENT_INSECURE_HTTP=1) permits serving plain HTTP
	// on a non-loopback Addr. Dev-only escape hatch; never set in production —
	// volume keys would transit cleartext. A loopback Addr is always allowed
	// without certs (that traffic never leaves the host).
	InsecureHTTP bool

	// DataDir is where per-machine state.json files and the IP allocator table
	// are persisted, so the agent re-attaches across restarts.
	DataDir string

	// Driver selects the VM backend: "dev" (process-backed stub) or
	// "firecracker" (linux-only, built behind the `firecracker` tag).
	Driver string

	// Subnet is the per-host guest subnet; the agent allocates the lowest free
	// host address from it. Gateway is its first usable address (.1).
	Subnet netip.Prefix

	// BootDelay is how long the dev driver pretends a boot takes before
	// transitioning creating→running. Ignored by the firecracker driver.
	BootDelay time.Duration

	// StubPath is the executable the dev driver runs as a stand-in VM process.
	// Empty means "resolve `sleep` from PATH".
	StubPath string

	// GuestAgentBin (PROTEOS_DEV_GUESTAGENT_BIN), when set, makes the dev driver
	// run the real guest agent per machine on a unix socket instead of the stub
	// (Phase 3): the whole terminal path then works on a Mac with no hypervisor.
	GuestAgentBin string

	// DevGuestWebBackend (PROTEOS_DEV_GUEST_WEB_BACKEND), when set, enables the
	// guest agent's Phase 8 web forward (code-server stand-in) in the dev driver,
	// pointing it at this address. In dev/e2e it is a stub HTTP+WS server, so the
	// code-server tunnel (DialGuest on the web port) round-trips with no real
	// code-server. Empty ⇒ terminal-only dev (no web listener). Unused by the
	// firecracker driver, which always runs the in-image code-server on 1025.
	DevGuestWebBackend string

	// GuestVsockPort is the fixed guest port the in-VM agent listens on; the
	// firecracker driver connects to it via the jailed vsock uds (Phase 3,
	// decision #3). Unused by the dev driver.
	GuestVsockPort int

	// PreviewPortMin / PreviewPortMax (PROTEOS_PREVIEW_PORT_MIN/MAX) bound the
	// previewable application ports the guest tunnel may reach (PP2). Reserved
	// system ports 1024/1025 stay rejected regardless; anything outside the range
	// is 400 before any dial. Defaults to the high range
	// (agentapi.DefaultPreviewPortMin..Max). The control plane reads the same env
	// names so the mint and the allowlist agree.
	PreviewPortMin uint32
	PreviewPortMax uint32

	// --- firecracker driver (Task 2.7); unused by the dev driver -------------

	// FirecrackerBin / JailerBin are absolute paths to the pinned binaries.
	FirecrackerBin string
	JailerBin      string

	// ChrootBaseDir is the jailer --chroot-base-dir; each VM gets
	// <ChrootBaseDir>/firecracker/<id>/root.
	ChrootBaseDir string

	// ImagesDir holds the pinned kernel + base rootfs referenced by kernel_ref
	// / rootfs_ref (resolved relative to this dir).
	ImagesDir string

	// JailUIDStart / JailUIDCount define the per-VM uid range the jailer drops
	// to (uid = JailUIDStart + offset derived from the machine).
	JailUIDStart int
	JailUIDCount int

	// --- Phase 4: persistent disk + hibernate/resume -------------------------

	// VolumesDir holds the per-machine LUKS volume container files
	// (<VolumesDir>/<machine-id>.luks). It lives OUTSIDE the jail tree so
	// prepareChroot never deletes it (decision #1). Unused by the dev driver.
	VolumesDir string

	// CryptsetupBin is the absolute path to cryptsetup, used to format/open/close
	// the machine volume. Unused by the dev driver.
	CryptsetupBin string

	// MgmtIfaces (PROTEOS_AGENT_MGMT_IFACES, comma-separated) are the host
	// interfaces allowed to reach SSH and the agent API through the firecracker
	// driver's fail-closed nftables input chain. The token "egress" resolves to
	// the default-route interface. Defaults to "egress,tailscale0": the
	// production control plane arrives over tailscale, so an egress-only list
	// firewalls it out (the July 2026 machine-create outage). Unused by the dev
	// driver.
	MgmtIfaces []string
}

// Load reads and validates configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Addr:               getenv("PROTEOS_AGENT_ADDR", ":9090"),
		Token:              os.Getenv("PROTEOS_AGENT_TOKEN"),
		TLSCert:            os.Getenv("PROTEOS_AGENT_TLS_CERT"),
		TLSKey:             os.Getenv("PROTEOS_AGENT_TLS_KEY"),
		InsecureHTTP:       getenvBool("PROTEOS_AGENT_INSECURE_HTTP"),
		DataDir:            getenv("PROTEOS_AGENT_DATA_DIR", ".data/agent"),
		Driver:             getenv("PROTEOS_AGENT_DRIVER", "dev"),
		StubPath:           os.Getenv("PROTEOS_DEV_STUB"),
		GuestAgentBin:      os.Getenv("PROTEOS_DEV_GUESTAGENT_BIN"),
		DevGuestWebBackend: os.Getenv("PROTEOS_DEV_GUEST_WEB_BACKEND"),
		GuestVsockPort:     getenvInt("PROTEOS_GUEST_VSOCK_PORT", 1024),
		PreviewPortMin:     getenvUint32("PROTEOS_PREVIEW_PORT_MIN", api.DefaultPreviewPortMin),
		PreviewPortMax:     getenvUint32("PROTEOS_PREVIEW_PORT_MAX", api.DefaultPreviewPortMax),
		FirecrackerBin:     getenv("PROTEOS_FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
		JailerBin:          getenv("PROTEOS_JAILER_BIN", "/usr/local/bin/jailer"),
		ChrootBaseDir:      getenv("PROTEOS_CHROOT_BASE_DIR", "/srv/jailer"),
		ImagesDir:          getenv("PROTEOS_AGENT_IMAGES_DIR", "/var/lib/proteos/images"),
		JailUIDStart:       getenvInt("PROTEOS_JAIL_UID_START", 100000),
		JailUIDCount:       getenvInt("PROTEOS_JAIL_UID_COUNT", 1000),
		VolumesDir:         getenv("PROTEOS_AGENT_VOLUMES_DIR", "/var/lib/proteos/volumes"),
		CryptsetupBin:      getenv("PROTEOS_CRYPTSETUP_BIN", "/usr/sbin/cryptsetup"),
	}

	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("PROTEOS_AGENT_TLS_CERT and PROTEOS_AGENT_TLS_KEY must be set together")
	}
	if c.TLSCert == "" && !c.InsecureHTTP && !isLoopbackAddr(c.Addr) {
		return nil, fmt.Errorf("TLS is required on non-loopback PROTEOS_AGENT_ADDR %q (the agent API carries volume keys): set PROTEOS_AGENT_TLS_CERT/_KEY, or PROTEOS_AGENT_INSECURE_HTTP=1 for dev-only plain HTTP", c.Addr)
	}

	subnetStr := getenv("PROTEOS_AGENT_SUBNET", "172.30.0.0/24")
	prefix, err := netip.ParsePrefix(subnetStr)
	if err != nil {
		return nil, fmt.Errorf("PROTEOS_AGENT_SUBNET %q: %w", subnetStr, err)
	}
	c.Subnet = prefix.Masked()

	bootDelay := getenv("PROTEOS_DEV_BOOT_DELAY", "2s")
	d, err := time.ParseDuration(bootDelay)
	if err != nil {
		return nil, fmt.Errorf("PROTEOS_DEV_BOOT_DELAY %q: %w", bootDelay, err)
	}
	c.BootDelay = d

	if c.Token == "" {
		return nil, fmt.Errorf("PROTEOS_AGENT_TOKEN is required (shared bearer token with the control plane)")
	}

	if c.PreviewPortMin < 1 || c.PreviewPortMax > 65535 || c.PreviewPortMin > c.PreviewPortMax {
		return nil, fmt.Errorf("PROTEOS_PREVIEW_PORT_MIN/MAX invalid range: %d..%d", c.PreviewPortMin, c.PreviewPortMax)
	}

	c.MgmtIfaces = splitCSV(getenv("PROTEOS_AGENT_MGMT_IFACES", "egress,tailscale0"))
	if len(c.MgmtIfaces) == 0 {
		return nil, fmt.Errorf("PROTEOS_AGENT_MGMT_IFACES resolves to no interfaces — the fail-closed nftables input chain would firewall the control plane out")
	}
	return c, nil
}

// isLoopbackAddr reports whether a listen address binds only loopback
// (localhost or a 127.x/::1 address). An empty host (":9090") binds every
// interface and is NOT loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// splitCSV splits a comma-separated value into trimmed, non-empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvUint32(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(n)
		}
	}
	return def
}
