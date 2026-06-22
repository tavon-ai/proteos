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

import "time"

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

// Def is the server-side definition of a profile item type. The kind/target are
// fixed here, not supplied by the client. Provider, when non-empty, marks the
// env var as that provider's auth credential: the injector merges it into the
// provider's own ProviderDef.Env (not a standalone entry) so it reaches both
// login shells and agent-launched sessions. TTL, when non-zero, is recorded as
// the item's expires_at (drives the Phase 2 "needs reconnect" status).
type Def struct {
	Key      string
	Kind     Kind
	Target   string
	Provider string
	TTL      time.Duration
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
