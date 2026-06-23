// Package profile implements the portable user profile: a small set of
// user-scoped items (credentials, and later dotfiles) that materialize into each
// of the user's machines at injection time, so state follows the *user* rather
// than being trapped on one machine's per-machine LUKS volume.
//
// Tier 0 (this package's first cut) ships exactly one item — the Claude
// subscription OAuth token — proving the whole path through every layer without
// any guestagent change: an env-kind item whose value is merged into the claude
// provider's env during injection, so a freshly created machine launches
// `claude` already authenticated.
//
// The item model is generic from day one. A Def (below) is the server-side
// authority for an item's kind/target/provider association; a client only ever
// supplies the value. This keeps a user from, e.g., targeting an arbitrary env
// var or claiming the not-yet-supported file kind.
package profile

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// Kind is the materialization shape of a profile item.
type Kind string

const (
	// KindEnv: target is an environment variable name. The value is exposed to
	// the machine's shells (and, when Provider is set, to agent sessions). Tier 0.
	KindEnv Kind = "env"
	// KindFile: target is a $HOME-relative path/mode. Requires a guest
	// materializer (Phase 3) and is not yet honored by injection.
	KindFile Kind = "file"
)

// MaxValueLen bounds an accepted profile item value (defensive; a Claude
// setup-token is a few hundred bytes).
const MaxValueLen = 8192

// ClaudeOAuthKey is the well-known key for the Claude subscription token minted
// by `claude setup-token`. Targeting CLAUDE_CODE_OAUTH_TOKEN authenticates the
// CLI for Pro/Max/Team/Enterprise subscription users without an API key.
const ClaudeOAuthKey = "claude-oauth"

// DefaultFileMode is applied to a file-kind item whose requested mode is 0.
const DefaultFileMode os.FileMode = 0o600

// Def is the server-side definition of a profile item type. For a registered
// item the kind/target are fixed here, not supplied by the client; a generic
// file item (Phase 3) is described by an ad-hoc Def the handler builds from the
// request. Provider, when non-empty, marks an env var as that provider's auth
// credential: the injector merges it into the provider's own ProviderDef.Env (not
// a standalone entry) so it reaches both login shells and agent-launched
// sessions. Mode is the file permission for kind=file (0 ⇒ DefaultFileMode). TTL,
// when non-zero, is recorded as the item's expires_at (drives the "needs
// reconnect" status).
type Def struct {
	Key      string
	Kind     Kind
	Target   string
	Provider string
	Mode     os.FileMode
	TTL      time.Duration
}

// MaxFilePathLen bounds a $HOME-relative file path (defensive).
const MaxFilePathLen = 1024

// FileDef builds a Def for a generic file-kind item (Phase 3): a client-specified
// $HOME-relative path and mode. It validates the path stays within $HOME; a 0
// mode is normalized to DefaultFileMode.
func FileDef(key, relPath string, mode os.FileMode) (Def, error) {
	if err := ValidateFilePath(relPath); err != nil {
		return Def{}, err
	}
	if mode == 0 {
		mode = DefaultFileMode
	}
	return Def{Key: key, Kind: KindFile, Target: path.Clean(relPath), Mode: mode & os.ModePerm}, nil
}

// ValidateFilePath checks a $HOME-relative file path: non-empty, within
// MaxFilePathLen, not absolute, and not escaping $HOME via "..". The guest
// re-validates (defense in depth), but rejecting here gives a clean 4xx.
func ValidateFilePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("file path is empty")
	}
	if len(p) > MaxFilePathLen {
		return fmt.Errorf("file path too long")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("file path must be $HOME-relative")
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("file path escapes $HOME")
	}
	return nil
}

// defs is the registry of known profile item types. Phase 1 ships exactly one.
var defs = map[string]Def{
	ClaudeOAuthKey: {
		Key:      ClaudeOAuthKey,
		Kind:     KindEnv,
		Target:   "CLAUDE_CODE_OAUTH_TOKEN",
		Provider: "claude",
		// `claude setup-token` mints a one-year token; record an expiry ~1y out.
		TTL: 365 * 24 * time.Hour,
	},
}

// Lookup returns the Def for a known item key, or ok=false for an unknown key.
func Lookup(key string) (Def, bool) {
	d, ok := defs[key]
	return d, ok
}

// SSH profile items (Phase 4). The key and config are server-managed file-kind
// items materialized under ~/.ssh by the injector; they are set through the typed
// /api/profile/ssh route, not the generic PUT, so they are not in the `defs`
// registry. The private key is stored 0600; ~/.ssh becomes 0700 via the guest's
// parent-dir creation.
const (
	SSHKeyItemKey    = "ssh-key"
	SSHConfigItemKey = "ssh-config"

	sshKeyPath    = ".ssh/id_ed25519"
	sshConfigPath = ".ssh/config"

	// sshPublicField is the sibling OpenBao field (under the SSH key item) holding
	// the non-secret public key, so the UI can show it without the private key.
	sshPublicField = "public"
)

// sshConfigContent makes git-over-SSH connect without an interactive host-key
// prompt on first use (accept-new is TOFU: it records the key but rejects a later
// change), so an SSH remote operation succeeds on a fresh machine. It is scoped to
// the common git hosts (not "Host *"), so it never weakens host-key checking for
// arbitrary SSH the user runs from the terminal.
const sshConfigContent = "Host github.com gitlab.com bitbucket.org ssh.dev.azure.com\n" +
	"    StrictHostKeyChecking accept-new\n" +
	"    IdentityFile ~/.ssh/id_ed25519\n"

func sshKeyDef() Def { return Def{Key: SSHKeyItemKey, Kind: KindFile, Target: sshKeyPath, Mode: 0o600} }
func sshConfigDef() Def {
	return Def{Key: SSHConfigItemKey, Kind: KindFile, Target: sshConfigPath, Mode: 0o600}
}
