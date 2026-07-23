package config

import (
	"os"
	"testing"
	"time"
)

// clearEnv blanks every environment variable Load consults so a test observes
// pure defaults regardless of what the surrounding shell exported.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PROTEOS_ADDR", "DATABASE_URL", "PROTEOS_BASE_URL",
		"GITHUB_APP_CLIENT_ID", "GITHUB_APP_CLIENT_SECRET", "GITHUB_APP_SLUG",
		"PROTEOS_GIT_HOST", "PROTEOS_GIT_PUBLIC_HOSTS", "PROTEOS_SECRETS_BACKEND", "PROTEOS_SECRETS_FILE",
		"PROTEOS_OPENBAO_ADDR", "PROTEOS_OPENBAO_MOUNT", "PROTEOS_OPENBAO_PREFIX", "PROTEOS_OPENBAO_ROLE_ID",
		"PROTEOS_OPENBAO_SECRET_ID_FILE", "ALLOWED_GITHUB_LOGINS", "PROTEOS_COOKIE_SECURE",
		"PROTEOS_HOST_NAME", "PROTEOS_NODE_AGENT_URL", "PROTEOS_AGENT_TOKEN", "PROTEOS_NODE_AGENT_INSECURE_HTTP",
		"PROTEOS_NODE_CA_FILE", "PROTEOS_HOSTS", "PROTEOS_MACHINE_VCPUS", "PROTEOS_MACHINE_MEM_MIB",
		"PROTEOS_MACHINE_DISK_MIB", "PROTEOS_KERNEL_REF", "PROTEOS_ROOTFS_REF",
		"PROTEOS_MACHINE_DOMAIN", "PROTEOS_STATE_KEY",
		"PROTEOS_ALLOWED_WS_ORIGINS", "PROTEOS_SESSION_EXPORT_DIR",
	} {
		t.Setenv(k, "")
	}
	// PROTEOS_PROVIDERS_ENABLED is read with LookupEnv, so "present but empty"
	// differs from "absent". Register restoration via Setenv, then remove it so
	// the default case observes a truly absent var.
	t.Setenv("PROTEOS_PROVIDERS_ENABLED", "")
	_ = os.Unsetenv("PROTEOS_PROVIDERS_ENABLED")
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.GitHost != "github.com" {
		t.Errorf("GitHost = %q, want github.com", c.GitHost)
	}
	if c.SecretsBackend != "file" {
		t.Errorf("SecretsBackend = %q, want file", c.SecretsBackend)
	}
	if c.OpenBaoMount != "secret" {
		t.Errorf("OpenBaoMount = %q, want secret", c.OpenBaoMount)
	}
	if c.OpenBaoPrefix != "proteos" {
		t.Errorf("OpenBaoPrefix = %q, want proteos", c.OpenBaoPrefix)
	}
	if c.HostName != "local" {
		t.Errorf("HostName = %q, want local", c.HostName)
	}
	if c.MachineVcpus != 2 || c.MachineMemMiB != 2048 || c.MachineDiskMiB != 10240 {
		t.Errorf("machine spec = %d/%d/%d, want 2/2048/10240", c.MachineVcpus, c.MachineMemMiB, c.MachineDiskMiB)
	}
	if c.SessionTTL != 30*24*time.Hour {
		t.Errorf("SessionTTL = %v", c.SessionTTL)
	}
	if !c.CookieSecure {
		t.Error("CookieSecure should default true")
	}
	if c.ProvidersEnabledSet {
		t.Error("ProvidersEnabledSet should be false when env var absent")
	}
	if c.StateSigningKey != nil {
		t.Error("StateSigningKey should be nil when PROTEOS_STATE_KEY unset")
	}
	if c.SessionExportDir != "./exports/sessions/" {
		t.Errorf("SessionExportDir = %q, want ./exports/sessions/", c.SessionExportDir)
	}
}

// TAV-141: PROTEOS_SESSION_EXPORT_DIR overrides the default directory Claude
// coding-agent sessions are exported to before a machine is destroyed.
func TestLoadSessionExportDirOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_SESSION_EXPORT_DIR", "/tmp/proteos-session-exports")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionExportDir != "/tmp/proteos-session-exports" {
		t.Errorf("SessionExportDir = %q, want /tmp/proteos-session-exports", c.SessionExportDir)
	}
}

func TestLoadDefaultWSOriginsIncludesViteForLocalhost(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// BaseURL defaults to localhost, so the Vite dev origin must be appended.
	want := map[string]bool{"http://localhost:8080": false, "http://localhost:5173": false}
	for _, o := range c.AllowedWSOrigins {
		if _, ok := want[o]; ok {
			want[o] = true
		}
	}
	for o, seen := range want {
		if !seen {
			t.Errorf("AllowedWSOrigins missing %q (got %v)", o, c.AllowedWSOrigins)
		}
	}
}

func TestLoadWSOriginsNoViteForNonLocal(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_BASE_URL", "https://proteos.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowedWSOrigins) != 1 || c.AllowedWSOrigins[0] != "https://proteos.example.com" {
		t.Errorf("AllowedWSOrigins = %v, want only the base origin", c.AllowedWSOrigins)
	}
}

func TestLoadExplicitWSOriginsWin(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_ALLOWED_WS_ORIGINS", "https://a.test, https://b.test")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AllowedWSOrigins) != 2 || c.AllowedWSOrigins[0] != "https://a.test" || c.AllowedWSOrigins[1] != "https://b.test" {
		t.Errorf("AllowedWSOrigins = %v", c.AllowedWSOrigins)
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_ADDR", ":9999")
	t.Setenv("PROTEOS_COOKIE_SECURE", "false")
	t.Setenv("PROTEOS_MACHINE_VCPUS", "8")
	t.Setenv("PROTEOS_MACHINE_MEM_MIB", "not-a-number") // falls back to default
	t.Setenv("PROTEOS_STATE_KEY", "hunter2")
	t.Setenv("ALLOWED_GITHUB_LOGINS", "alice, bob ,")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":9999" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.CookieSecure {
		t.Error("CookieSecure should be false")
	}
	if c.MachineVcpus != 8 {
		t.Errorf("MachineVcpus = %d, want 8", c.MachineVcpus)
	}
	if c.MachineMemMiB != 2048 {
		t.Errorf("MachineMemMiB = %d, want default 2048 on bad input", c.MachineMemMiB)
	}
	if string(c.StateSigningKey) != "hunter2" {
		t.Errorf("StateSigningKey = %q", c.StateSigningKey)
	}
	if len(c.AllowedGitHubLogins) != 2 || c.AllowedGitHubLogins[0] != "alice" || c.AllowedGitHubLogins[1] != "bob" {
		t.Errorf("AllowedGitHubLogins = %v, want [alice bob] (trimmed, empties dropped)", c.AllowedGitHubLogins)
	}
}

// Gitea/Forgejo phase 1: PROTEOS_GIT_PUBLIC_HOSTS is a CSV of bare host[:port]
// entries, lowercased on load; anything URL-shaped fails startup loudly.
func TestLoadGitPublicHosts(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.GitPublicHosts) != 0 {
		t.Errorf("GitPublicHosts = %v, want empty by default", c.GitPublicHosts)
	}

	clearEnv(t)
	t.Setenv("PROTEOS_GIT_PUBLIC_HOSTS", "Codeberg.org, git.example.com:3000 ,")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.GitPublicHosts) != 2 || c.GitPublicHosts[0] != "codeberg.org" || c.GitPublicHosts[1] != "git.example.com:3000" {
		t.Errorf("GitPublicHosts = %v, want [codeberg.org git.example.com:3000] (lowercased, trimmed, empties dropped)", c.GitPublicHosts)
	}

	for _, bad := range []string{"https://codeberg.org", "codeberg.org/gitea", "user@codeberg.org", "codeberg.org:port"} {
		clearEnv(t)
		t.Setenv("PROTEOS_GIT_PUBLIC_HOSTS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("Load with PROTEOS_GIT_PUBLIC_HOSTS=%q succeeded; want bare-host validation error", bad)
		}
	}
}

// TAV-27: the node-agent channel carries volume keys, so a plain-HTTP URL to a
// non-loopback host must be rejected unless dev explicitly opts out. Loopback
// stays allowed with no flag (dev stacks; the traffic never leaves the host).
func TestLoadNodeAgentURLRequiresHTTPS(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		insecure string
		wantErr  bool
	}{
		{"loopback http default ok", "", "", false},
		{"localhost http ok", "http://localhost:9090", "", false},
		{"https ok", "https://100.72.173.19:9090", "", false},
		{"non-loopback http rejected", "http://192.168.2.84:9090", "", true},
		{"non-loopback http with opt-out ok", "http://192.168.2.84:9090", "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			if tc.url != "" {
				t.Setenv("PROTEOS_NODE_AGENT_URL", tc.url)
			}
			if tc.insecure != "" {
				t.Setenv("PROTEOS_NODE_AGENT_INSECURE_HTTP", tc.insecure)
			}
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatalf("Load(%q) succeeded; want https-required error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Load(%q): %v", tc.url, err)
			}
		})
	}
}

// TAV-37: PROTEOS_HOSTS lists the additional KVM hosts beyond the primary one;
// unset means the fleet is just the primary host (today's default).
func TestLoadAdditionalHostsUnsetIsEmpty(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AdditionalHosts) != 0 {
		t.Errorf("AdditionalHosts = %v, want empty", c.AdditionalHosts)
	}
}

func TestLoadAdditionalHostsParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"gpu-1","url":"https://gpu-1.internal:9090"},{"name":"gpu-2","url":"http://127.0.0.1:9091"}]`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AdditionalHosts) != 2 {
		t.Fatalf("AdditionalHosts = %v, want 2 entries", c.AdditionalHosts)
	}
	if c.AdditionalHosts[0].Name != "gpu-1" || c.AdditionalHosts[0].URL != "https://gpu-1.internal:9090" {
		t.Errorf("AdditionalHosts[0] = %+v", c.AdditionalHosts[0])
	}
	if c.AdditionalHosts[1].Name != "gpu-2" || c.AdditionalHosts[1].URL != "http://127.0.0.1:9091" {
		t.Errorf("AdditionalHosts[1] = %+v", c.AdditionalHosts[1])
	}
}

func TestLoadAdditionalHostsInvalidJSON(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `not json`)
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject invalid PROTEOS_HOSTS JSON")
	}
}

func TestLoadAdditionalHostsMissingFieldsRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"gpu-1"}]`)
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject a host missing url")
	}
}

func TestLoadAdditionalHostsDuplicateNameRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"local","url":"http://127.0.0.1:9091"}]`)
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject an additional host reusing the primary host's name")
	}

	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"gpu-1","url":"http://127.0.0.1:9091"},{"name":"gpu-1","url":"http://127.0.0.1:9092"}]`)
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject two additional hosts sharing a name")
	}
}

// TAV-27's https/loopback rule applies to every additional host too.
func TestLoadAdditionalHostsRequiresHTTPS(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"gpu-1","url":"http://192.168.2.84:9090"}]`)
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject a non-loopback plain-HTTP additional host")
	}

	clearEnv(t)
	t.Setenv("PROTEOS_HOSTS", `[{"name":"gpu-1","url":"http://192.168.2.84:9090"}]`)
	t.Setenv("PROTEOS_NODE_AGENT_INSECURE_HTTP", "1")
	if _, err := Load(); err != nil {
		t.Fatalf("Load with insecure opt-out should succeed: %v", err)
	}
}

func TestLoadProvidersEnabledSetByPresence(t *testing.T) {
	clearEnv(t)
	// Empty value still sets the flag (disables every provider on reconcile).
	t.Setenv("PROTEOS_PROVIDERS_ENABLED", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ProvidersEnabledSet {
		t.Error("ProvidersEnabledSet should be true when env var present, even if empty")
	}
	if len(c.ProvidersEnabled) != 0 {
		t.Errorf("ProvidersEnabled = %v, want empty", c.ProvidersEnabled)
	}
}

func TestLoadProvidersEnabledList(t *testing.T) {
	clearEnv(t)
	t.Setenv("PROTEOS_PROVIDERS_ENABLED", "claude, codex")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ProvidersEnabledSet {
		t.Error("ProvidersEnabledSet should be true")
	}
	if len(c.ProvidersEnabled) != 2 || c.ProvidersEnabled[0] != "claude" || c.ProvidersEnabled[1] != "codex" {
		t.Errorf("ProvidersEnabled = %v", c.ProvidersEnabled)
	}
}

func TestValidateOAuth(t *testing.T) {
	good := &Config{
		GitHubClientID:     "id",
		GitHubClientSecret: "secret",
		StateSigningKey:    []byte("key"),
	}
	if err := good.ValidateOAuth(); err != nil {
		t.Errorf("complete config rejected: %v", err)
	}

	empty := &Config{}
	err := empty.ValidateOAuth()
	if err == nil {
		t.Fatal("empty config should fail ValidateOAuth")
	}
	for _, want := range []string{"GITHUB_APP_CLIENT_ID", "GITHUB_APP_CLIENT_SECRET", "PROTEOS_STATE_KEY"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
		{",,,", nil},
	}
	for _, tc := range cases {
		got := splitList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestGetenvInt(t *testing.T) {
	t.Setenv("CFG_TEST_INT", "")
	if got := getenvInt("CFG_TEST_INT", 42); got != 42 {
		t.Errorf("empty => %d, want default 42", got)
	}
	t.Setenv("CFG_TEST_INT", "17")
	if got := getenvInt("CFG_TEST_INT", 42); got != 17 {
		t.Errorf("set => %d, want 17", got)
	}
	t.Setenv("CFG_TEST_INT", "abc")
	if got := getenvInt("CFG_TEST_INT", 42); got != 42 {
		t.Errorf("non-numeric => %d, want default 42", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
