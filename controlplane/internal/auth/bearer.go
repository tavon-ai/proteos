package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrInvalidBearer is returned when an IdP access token does not resolve to a
// live identity. It is deliberately undifferentiated — expired, revoked,
// malformed and issued-by-someone-else all look the same to a caller.
var ErrInvalidBearer = errors.New("auth: invalid or expired IdP access token")

// bearerCacheTTL bounds how long a resolved token is reused without asking the
// IdP again.
//
// This is a revocation lag, and it is the price of not making a userinfo round
// trip on every API call: a token revoked at the IdP keeps working here for up
// to this long. Kept short deliberately — access tokens are minutes-lived, so a
// minute of lag is small next to their natural lifetime, and the browser
// session path (which has no such cache) is unaffected.
const bearerCacheTTL = time.Minute

// bearerCacheMax caps the cache so a stream of distinct invalid tokens cannot
// grow it without bound. Only successful resolutions are ever cached, so the
// realistic occupancy is "one entry per active caller".
const bearerCacheMax = 1024

// AuthenticateBearer resolves an IdP access token to a ProteOS user.
//
// # Why this exists
//
// The suite's other services (Orbit Chat, the agent harness) authenticate their
// users against the SAME Zitadel issuer ProteOS logs in with. Before this, they
// had no way to call the API on a user's behalf: the CLI's personal access
// tokens must be minted in a browser, so a service could only ever act as one
// shared identity. Accepting the issuer's own access tokens lets the harness
// dispatch a task as the signed-in user, which is what keeps "a user only ever
// reaches their own machines" true.
//
// # Trust boundary
//
// Validity is established by the userinfo endpoint: a token the IdP answers for
// is live, and the claims come back over TLS from the IdP itself. No signature
// verification happens here, and none is needed — an attacker cannot make
// userinfo answer for a token the issuer did not mint.
//
// The boundary is therefore the ISSUER, not the client: any access token that
// issuer minted for this user is accepted, whichever application obtained it.
// That is sound while every registered client is first-party, and it is exactly
// how the browser session is trusted today. It would stop being sound the
// moment a third-party client is registered in the same Zitadel instance —
// narrowing it to specific audiences needs token introspection (RFC 7662),
// which needs client credentials this package is not given.
//
// Identity resolution is shared verbatim with the browser login callback
// (resolveOIDCUser), so an API-first user is linked or created by exactly the
// same rules, and a user who signs in later lands on the same row.
func (h *Handler) AuthenticateBearer(ctx context.Context, accessToken string) (store.User, error) {
	if accessToken == "" {
		return store.User{}, ErrInvalidBearer
	}
	key := sha256.Sum256([]byte(accessToken))
	if user, ok := h.bearerCache.get(key); ok {
		return user, nil
	}

	info, err := h.oidc.GetUserInfo(ctx, accessToken)
	if err != nil {
		// Every failure mode here — a rejected token, an unreachable IdP — is
		// reported to the CALLER as a flat 401. Distinguishing them would tell
		// an unauthenticated caller about the IdP's state.
		//
		// The operator is a different audience, and gets the reason. Without
		// this, a service whose calls are being refused looks identical to a
		// deployment that predates bearer auth entirely, and there is nothing
		// on either side to tell them apart.
		slog.Warn("idp bearer rejected", "err", err)
		return store.User{}, ErrInvalidBearer
	}
	user, errCode := h.resolveOIDCUser(ctx, info)
	if errCode != "" {
		// The token was good; mapping it to a user was not. Worth its own line:
		// link_ambiguous is an operator problem, not a caller one.
		slog.Warn("idp bearer resolved no user", "reason", errCode, "subject", info.Subject)
		return store.User{}, ErrInvalidBearer
	}
	slog.Debug("idp bearer accepted", "subject", info.Subject)
	h.bearerCache.put(key, user)
	return user, nil
}

// bearerCache maps a token's SHA-256 to the user it resolved to. Keyed by hash
// so the plaintext credential is not held in memory any longer than the request
// that carried it.
type bearerCache struct {
	mu      sync.Mutex
	entries map[[32]byte]bearerEntry
}

type bearerEntry struct {
	user      store.User
	expiresAt time.Time
}

func (c *bearerCache) get(key [32]byte) (store.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return store.User{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return store.User{}, false
	}
	return e.user, true
}

func (c *bearerCache) put(key [32]byte, user store.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[[32]byte]bearerEntry)
	}
	if len(c.entries) >= bearerCacheMax {
		c.sweepLocked()
	}
	// Still full after sweeping: skip caching rather than evict something
	// live. The cost is an extra userinfo call, which is the safe direction.
	if len(c.entries) >= bearerCacheMax {
		return
	}
	c.entries[key] = bearerEntry{user: user, expiresAt: time.Now().Add(bearerCacheTTL)}
}

// sweepLocked drops expired entries. Entries are short-lived, so a full scan at
// the cap is cheaper than maintaining an eviction order.
func (c *bearerCache) sweepLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}
