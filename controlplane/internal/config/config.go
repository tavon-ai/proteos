// Package config parses and validates control-plane configuration from the
// environment. All runtime knobs live here so the rest of the code takes a
// typed Config rather than reading os.Getenv ad hoc.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// Config is the fully-resolved control-plane configuration.
type Config struct {
	// Addr is the host:port the HTTP server listens on.
	Addr string

	// DatabaseURL is the Postgres connection string (pgx-compatible DSN).
	DatabaseURL string

	// BaseURL is the externally-reachable origin of the control plane, used to
	// build OAuth callback URLs and cookie scoping (e.g. http://localhost:8080).
	BaseURL string

	// GitHub App user-authorization credentials.
	GitHubClientID     string
	GitHubClientSecret string

	// GitHubAppSlug is the App's URL slug (github.com/apps/<slug>), used to build
	// the installation-management ("grants") URL the Repos panel links to so the
	// user can choose which repos ProteOS may access (Phase 7 decision #7).
	GitHubAppSlug string

	// GitHost is the git host the credential handler will mint credentials for and
	// the only host clones target (Phase 7). Defaults to github.com; overridden
	// only by the e2e harness to point at its local git server.
	GitHost string

	// StateSigningKey is the HMAC key used to sign the short-lived OAuth state
	// cookie. Must be non-empty in any environment that runs the OAuth flow.
	StateSigningKey []byte

	// SecretsBackend selects the secrets.Store implementation: "file" (dev,
	// default) or "openbao" (production). The Phase 1 interface was built for
	// this swap — every caller moves to OpenBao by config alone.
	SecretsBackend string

	// SecretsFile is the path to the dev file-backed secrets store (file backend).
	SecretsFile string

	// OpenBao* configure the openbao backend. Mount defaults to "secret"; auth is
	// AppRole (RoleID + a file holding the secret_id).
	OpenBaoAddr         string
	OpenBaoMount        string
	OpenBaoRoleID       string
	OpenBaoSecretIDFile string

	// AllowedGitHubLogins, when non-empty, restricts sign-in to the listed
	// GitHub logins (signup allowlist). Empty means everyone is allowed.
	AllowedGitHubLogins []string

	// SessionTTL is how long a new session is valid before expiry.
	SessionTTL time.Duration

	// CookieSecure controls the Secure attribute on the session cookie. True in
	// all real environments; the only reason to disable is exotic local setups.
	CookieSecure bool

	// --- Phase 2: node-agent + machine spec ---------------------------------

	// HostName is the unique name of the single host this control plane manages
	// (multi-host scheduling is Phase 11). Upserted into `hosts` at startup.
	HostName string

	// NodeAgentURL is the private base URL the control plane dials the
	// node-agent at (e.g. http://127.0.0.1:9090).
	NodeAgentURL string

	// AgentToken is the shared bearer token presented to the node-agent. Must
	// match the agent's PROTEOS_AGENT_TOKEN.
	AgentToken string

	// NodeCAFile pins the node-agent's TLS certificate/CA (PEM). When set, the
	// control plane verifies the agent against it instead of the system trust
	// store (Phase 4 decision #3). Empty ⇒ plain HTTP / system roots (dev).
	NodeCAFile string

	// MachineVcpus / MachineMemMiB / MachineDiskMiB are the resource spec stamped
	// on every new machine row at create time. DiskMiB is the persistent disk
	// size provisioned per machine (Phase 4, default 10240).
	MachineVcpus   int
	MachineMemMiB  int
	MachineDiskMiB int

	// MachineMaxPerUser caps how many machines one user may own (default 3). The
	// cap protects the single fc-node host's RAM and guest-IP pool.
	MachineMaxPerUser int

	// MaxVcpus / MaxMemMiB / MaxDiskMiB are the upper bounds for user resource
	// overrides at create time (PROTEOS_MAX_VCPUS / _MEM_MIB / _DISK_MIB). The
	// floors are fixed (1 vcpu / 1024 MiB / 5120 MiB). A template's own defaults
	// must fall within these bounds or startup fails.
	MaxVcpus   int
	MaxMemMiB  int
	MaxDiskMiB int

	// KernelRef / RootfsRef are the pinned image refs stamped per machine; the
	// node-agent resolves them against its images dir. They are the legacy
	// single-image fallback used when TemplatesFile is unset (a one-entry "base"
	// catalog is synthesized from them).
	KernelRef string
	RootfsRef string

	// TemplatesFile (PROTEOS_TEMPLATES_FILE) is the path to a JSON machine-template
	// catalog ({"templates":[{id,label,description,rootfs_ref,kernel_ref,defaults}]}).
	// When set it is the source of truth for create-time image refs and default
	// resources; when unset, a single "base" template is synthesized from
	// RootfsRef/KernelRef + the Machine* resource defaults.
	TemplatesFile string

	// --- Phase 3: terminal gateway -----------------------------------------

	// AllowedWSOrigins is the exact-match allowlist for the terminal WebSocket's
	// Origin header (PROTEOS_ALLOWED_WS_ORIGINS, CSV). Defaults to the BaseURL
	// origin; in dev the Vite origin (http://localhost:5173) is also added.
	AllowedWSOrigins []string

	// --- Phase 8: machine-web (code-server) --------------------------------

	// MachineDomain (PROTEOS_MACHINE_DOMAIN) is the parent domain for per-machine
	// editor subdomains: a machine is served at m-<uuid>.<MachineDomain> (decision
	// #1). Empty ⇒ machine-web routing is disabled entirely (the default; non-web
	// deployments are unaffected). Dev/e2e use "localhost" (RFC 6761 loopback —
	// m-<uuid>.localhost needs no DNS). The token + subdomain cookie are signed
	// with StateSigningKey (reused, per decision #2).
	MachineDomain string

	// PreviewPortMin / PreviewPortMax (PROTEOS_PREVIEW_PORT_MIN/MAX) bound the
	// previewable application ports the web-session mint will issue a token for
	// (PP2). Reserved system ports 1024/1025 stay rejected regardless. Defaults to
	// the high range (agentapi.DefaultPreviewPortMin/Max). The node-agent reads
	// the same env names so the mint and the tunnel allowlist agree.
	PreviewPortMin uint32
	PreviewPortMax uint32

	// --- Phase 6: provider enablement --------------------------------------

	// ProvidersEnabled aligns the registry's enabled flag with the providers
	// actually baked into the rootfs (PROTEOS_PROVIDERS_ENABLED, CSV of provider
	// keys). When set, startup enables exactly these keys and disables all others,
	// so the UI never offers a provider whose CLI is not in the image. When the
	// var is absent the registry is left as seeded (ProvidersEnabledSet is false).
	ProvidersEnabled    []string
	ProvidersEnabledSet bool
}

// Load reads configuration from the environment and validates it. The
// requireAuth flag controls whether GitHub/OAuth settings are mandatory: the
// /healthz-only smoke path and some tests can run without them.
func Load() (*Config, error) {
	c := &Config{
		Addr:                getenv("PROTEOS_ADDR", ":8080"),
		DatabaseURL:         getenv("DATABASE_URL", "postgres://proteos:proteos@localhost:5432/proteos?sslmode=disable"),
		BaseURL:             getenv("PROTEOS_BASE_URL", "http://localhost:8080"),
		GitHubClientID:      os.Getenv("GITHUB_APP_CLIENT_ID"),
		GitHubClientSecret:  os.Getenv("GITHUB_APP_CLIENT_SECRET"),
		GitHubAppSlug:       os.Getenv("GITHUB_APP_SLUG"),
		GitHost:             getenv("PROTEOS_GIT_HOST", "github.com"),
		SecretsBackend:      getenv("PROTEOS_SECRETS_BACKEND", "file"),
		SecretsFile:         getenv("PROTEOS_SECRETS_FILE", ".data/secrets.json"),
		OpenBaoAddr:         getenv("PROTEOS_OPENBAO_ADDR", "http://127.0.0.1:8200"),
		OpenBaoMount:        getenv("PROTEOS_OPENBAO_MOUNT", "secret"),
		OpenBaoRoleID:       os.Getenv("PROTEOS_OPENBAO_ROLE_ID"),
		OpenBaoSecretIDFile: os.Getenv("PROTEOS_OPENBAO_SECRET_ID_FILE"),
		AllowedGitHubLogins: splitList(os.Getenv("ALLOWED_GITHUB_LOGINS")),
		SessionTTL:          30 * 24 * time.Hour,
		CookieSecure:        getenv("PROTEOS_COOKIE_SECURE", "true") == "true",

		HostName:       getenv("PROTEOS_HOST_NAME", "local"),
		NodeAgentURL:   getenv("PROTEOS_NODE_AGENT_URL", "http://127.0.0.1:9090"),
		AgentToken:     os.Getenv("PROTEOS_AGENT_TOKEN"),
		NodeCAFile:     os.Getenv("PROTEOS_NODE_CA_FILE"),
		MachineVcpus:   getenvInt("PROTEOS_MACHINE_VCPUS", 2),
		MachineMemMiB:  getenvInt("PROTEOS_MACHINE_MEM_MIB", 2048),
		MachineDiskMiB: getenvInt("PROTEOS_MACHINE_DISK_MIB", 10240),

		MachineMaxPerUser: getenvInt("PROTEOS_MAX_MACHINES_PER_USER", 3),
		KernelRef:         getenv("PROTEOS_KERNEL_REF", "vmlinux-6.1"),
		RootfsRef:         getenv("PROTEOS_ROOTFS_REF", "ubuntu-24.04"),
		TemplatesFile:     os.Getenv("PROTEOS_TEMPLATES_FILE"),
		MaxVcpus:          getenvInt("PROTEOS_MAX_VCPUS", 8),
		MaxMemMiB:         getenvInt("PROTEOS_MAX_MEM_MIB", 16384),
		MaxDiskMiB:        getenvInt("PROTEOS_MAX_DISK_MIB", 51200),

		MachineDomain:  os.Getenv("PROTEOS_MACHINE_DOMAIN"),
		PreviewPortMin: getenvUint32("PROTEOS_PREVIEW_PORT_MIN", agentapi.DefaultPreviewPortMin),
		PreviewPortMax: getenvUint32("PROTEOS_PREVIEW_PORT_MAX", agentapi.DefaultPreviewPortMax),
	}

	if c.PreviewPortMin < 1 || c.PreviewPortMax > 65535 || c.PreviewPortMin > c.PreviewPortMax {
		return nil, fmt.Errorf("PROTEOS_PREVIEW_PORT_MIN/MAX invalid range: %d..%d", c.PreviewPortMin, c.PreviewPortMax)
	}

	if key := os.Getenv("PROTEOS_STATE_KEY"); key != "" {
		c.StateSigningKey = []byte(key)
	}

	// Provider enablement: presence of the var (even empty) triggers reconcile;
	// an empty value disables every provider, a CSV enables exactly those keys.
	if v, ok := os.LookupEnv("PROTEOS_PROVIDERS_ENABLED"); ok {
		c.ProvidersEnabledSet = true
		c.ProvidersEnabled = splitList(v)
	}

	// Allowed WebSocket origins: explicit CSV wins; otherwise default to the
	// BaseURL origin, plus the Vite dev origin when BaseURL is localhost.
	if origins := splitList(os.Getenv("PROTEOS_ALLOWED_WS_ORIGINS")); len(origins) > 0 {
		c.AllowedWSOrigins = origins
	} else {
		c.AllowedWSOrigins = []string{c.BaseURL}
		if strings.Contains(c.BaseURL, "localhost") || strings.Contains(c.BaseURL, "127.0.0.1") {
			c.AllowedWSOrigins = append(c.AllowedWSOrigins, "http://localhost:5173")
		}
	}

	return c, nil
}

// ValidateOAuth returns an error if the settings required for the GitHub OAuth
// flow are missing. Called at startup once we know the server will serve auth.
func (c *Config) ValidateOAuth() error {
	var missing []string
	if c.GitHubClientID == "" {
		missing = append(missing, "GITHUB_APP_CLIENT_ID")
	}
	if c.GitHubClientSecret == "" {
		missing = append(missing, "GITHUB_APP_CLIENT_SECRET")
	}
	if len(c.StateSigningKey) == 0 {
		missing = append(missing, "PROTEOS_STATE_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
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

func getenvUint32(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(n)
		}
	}
	return def
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
