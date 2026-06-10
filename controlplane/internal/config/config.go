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

	// StateSigningKey is the HMAC key used to sign the short-lived OAuth state
	// cookie. Must be non-empty in any environment that runs the OAuth flow.
	StateSigningKey []byte

	// SecretsFile is the path to the dev file-backed secrets store.
	SecretsFile string

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

	// MachineVcpus / MachineMemMiB are the resource spec stamped on every new
	// machine row at create time.
	MachineVcpus  int
	MachineMemMiB int

	// KernelRef / RootfsRef are the pinned image refs stamped per machine; the
	// node-agent resolves them against its images dir.
	KernelRef string
	RootfsRef string
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
		SecretsFile:         getenv("PROTEOS_SECRETS_FILE", ".data/secrets.json"),
		AllowedGitHubLogins: splitList(os.Getenv("ALLOWED_GITHUB_LOGINS")),
		SessionTTL:          30 * 24 * time.Hour,
		CookieSecure:        getenv("PROTEOS_COOKIE_SECURE", "true") == "true",

		HostName:      getenv("PROTEOS_HOST_NAME", "local"),
		NodeAgentURL:  getenv("PROTEOS_NODE_AGENT_URL", "http://127.0.0.1:9090"),
		AgentToken:    os.Getenv("PROTEOS_AGENT_TOKEN"),
		MachineVcpus:  getenvInt("PROTEOS_MACHINE_VCPUS", 2),
		MachineMemMiB: getenvInt("PROTEOS_MACHINE_MEM_MIB", 2048),
		KernelRef:     getenv("PROTEOS_KERNEL_REF", "vmlinux-6.1"),
		RootfsRef:     getenv("PROTEOS_ROOTFS_REF", "ubuntu-24.04"),
	}

	if key := os.Getenv("PROTEOS_STATE_KEY"); key != "" {
		c.StateSigningKey = []byte(key)
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
