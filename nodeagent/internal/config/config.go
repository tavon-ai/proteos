// Package config parses node-agent configuration from the environment. As with
// the control plane, all runtime knobs are resolved here into a typed Config so
// the rest of the daemon never reads os.Getenv ad hoc.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"time"
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
	// plain HTTP (Phase 4 decision #3: the channel now carries volume keys). Dev
	// stacks (loopback/Mac) leave them empty and stay on plain HTTP.
	TLSCert string
	TLSKey  string

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

	// GuestVsockPort is the fixed guest port the in-VM agent listens on; the
	// firecracker driver connects to it via the jailed vsock uds (Phase 3,
	// decision #3). Unused by the dev driver.
	GuestVsockPort int

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
}

// Load reads and validates configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Addr:           getenv("PROTEOS_AGENT_ADDR", ":9090"),
		Token:          os.Getenv("PROTEOS_AGENT_TOKEN"),
		TLSCert:        os.Getenv("PROTEOS_AGENT_TLS_CERT"),
		TLSKey:         os.Getenv("PROTEOS_AGENT_TLS_KEY"),
		DataDir:        getenv("PROTEOS_AGENT_DATA_DIR", ".data/agent"),
		Driver:         getenv("PROTEOS_AGENT_DRIVER", "dev"),
		StubPath:       os.Getenv("PROTEOS_DEV_STUB"),
		GuestAgentBin:  os.Getenv("PROTEOS_DEV_GUESTAGENT_BIN"),
		GuestVsockPort: getenvInt("PROTEOS_GUEST_VSOCK_PORT", 1024),
		FirecrackerBin: getenv("PROTEOS_FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
		JailerBin:      getenv("PROTEOS_JAILER_BIN", "/usr/local/bin/jailer"),
		ChrootBaseDir:  getenv("PROTEOS_CHROOT_BASE_DIR", "/srv/jailer"),
		ImagesDir:      getenv("PROTEOS_AGENT_IMAGES_DIR", "/var/lib/proteos/images"),
		JailUIDStart:   getenvInt("PROTEOS_JAIL_UID_START", 100000),
		JailUIDCount:   getenvInt("PROTEOS_JAIL_UID_COUNT", 1000),
		VolumesDir:     getenv("PROTEOS_AGENT_VOLUMES_DIR", "/var/lib/proteos/volumes"),
		CryptsetupBin:  getenv("PROTEOS_CRYPTSETUP_BIN", "/usr/sbin/cryptsetup"),
	}

	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("PROTEOS_AGENT_TLS_CERT and PROTEOS_AGENT_TLS_KEY must be set together")
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
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
