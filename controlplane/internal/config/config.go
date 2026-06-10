// Package config parses and validates control-plane configuration from the
// environment. All runtime knobs live here so the rest of the code takes a
// typed Config rather than reading os.Getenv ad hoc.
package config

import (
	"fmt"
	"os"
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
