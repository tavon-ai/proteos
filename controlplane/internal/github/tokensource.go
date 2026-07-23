package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrReconnectGitHub means the user's GitHub grant is unusable (never linked,
// revoked, or the refresh token is dead) and the user must re-run the login
// flow. The HTTP layer maps it to 409 reconnect_github; the credential handler
// maps it to the reconnect_github channel error.
var ErrReconnectGitHub = errors.New("github: reconnect required")

// refreshSkew is how long before expiry a token is proactively refreshed. GitHub
// user-to-server access tokens last ~8h; refreshing with 10 min to spare avoids
// handing out a token that dies mid-operation.
const refreshSkew = 10 * time.Minute

// Secret-blob field names (stored in secrets.Store at the github_links
// secret_ref). The absolute expiry timestamps are co-located with the tokens so
// rotation is atomic — a refreshed pair and its expiries are written together.
const (
	fieldAccessToken      = "access_token"
	fieldRefreshToken     = "refresh_token"
	fieldTokenType        = "token_type"
	fieldScope            = "scope"
	fieldAccessExpiresAt  = "access_token_expires_at"  // RFC3339
	fieldRefreshExpiresAt = "refresh_token_expires_at" // RFC3339
)

// Credential is a valid access token and its absolute expiry.
type Credential struct {
	AccessToken string
	Expiry      time.Time
}

// TokenSource returns a valid GitHub user access token for a user, refreshing it
// before expiry and persisting the rotated refresh token before releasing the
// access token (Phase 7 decision #4). A single control-plane instance is assumed;
// a per-user lock serialises concurrent callers so only one refresh runs and the
// rest observe the fresh token (the singleflight property). For multi-instance
// (Phase 11) this lock becomes a Postgres advisory lock.
type TokenSource struct {
	gh      *Client
	q       *store.Queries
	secrets secrets.Store

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewTokenSource wires a TokenSource.
func NewTokenSource(gh *Client, q *store.Queries, sec secrets.Store) *TokenSource {
	return &TokenSource{gh: gh, q: q, secrets: sec, locks: map[string]*sync.Mutex{}}
}

// userLock returns the per-user mutex, creating it on first use.
func (ts *TokenSource) userLock(userID string) *sync.Mutex {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	l, ok := ts.locks[userID]
	if !ok {
		l = &sync.Mutex{}
		ts.locks[userID] = l
	}
	return l
}

// Token returns a currently-valid access token for userID, refreshing if it is
// missing or within refreshSkew of expiry. A revoked grant returns
// ErrReconnectGitHub.
func (ts *TokenSource) Token(ctx context.Context, userID string) (Credential, error) {
	lock := ts.userLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return Credential{}, fmt.Errorf("bad user id: %w", err)
	}

	link, err := ts.q.GetGitHubLink(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrReconnectGitHub
	}
	if err != nil {
		return Credential{}, fmt.Errorf("load github link: %w", err)
	}
	if metadataRevoked(link.Metadata) {
		return Credential{}, ErrReconnectGitHub
	}

	data, err := ts.secrets.Get(link.SecretRef)
	if errors.Is(err, secrets.ErrNotFound) {
		return Credential{}, ErrReconnectGitHub
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read github tokens: %w", err)
	}

	access := data[fieldAccessToken]
	accessExp := parseTime(data[fieldAccessExpiresAt])
	if access != "" && !accessExp.IsZero() && time.Until(accessExp) > refreshSkew {
		return Credential{AccessToken: access, Expiry: accessExp}, nil
	}

	return ts.refreshLocked(ctx, uid, link.SecretRef, link.Metadata, data)
}

// ForceRefresh rotates the token pair even when the stored access token looks
// unexpired. Callers use it after GitHub rejects a token (ErrUnauthorized): a
// grant revoked at github.com invalidates tokens server-side without touching
// the local expiry, so the stored pair cannot be trusted. A dead refresh token
// marks the grant revoked and returns ErrReconnectGitHub.
func (ts *TokenSource) ForceRefresh(ctx context.Context, userID string) (Credential, error) {
	lock := ts.userLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return Credential{}, fmt.Errorf("bad user id: %w", err)
	}

	link, err := ts.q.GetGitHubLink(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrReconnectGitHub
	}
	if err != nil {
		return Credential{}, fmt.Errorf("load github link: %w", err)
	}
	data, err := ts.secrets.Get(link.SecretRef)
	if errors.Is(err, secrets.ErrNotFound) {
		return Credential{}, ErrReconnectGitHub
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read github tokens: %w", err)
	}
	return ts.refreshLocked(ctx, uid, link.SecretRef, link.Metadata, data)
}

// refreshLocked rotates the token pair (caller holds the user lock). The
// rotated pair is persisted before the access token is returned, so a crash
// after refresh never strands the user with a dead refresh token.
func (ts *TokenSource) refreshLocked(ctx context.Context, uid pgtype.UUID, secretRef string, metadata []byte, data map[string]string) (Credential, error) {
	refresh := data[fieldRefreshToken]
	if refresh == "" {
		return Credential{}, ErrReconnectGitHub
	}
	tok, err := ts.gh.Refresh(ctx, refresh)
	if errors.Is(err, ErrBadRefreshToken) {
		if mErr := ts.markRevoked(ctx, uid, metadata); mErr != nil {
			return Credential{}, fmt.Errorf("mark revoked: %w", mErr)
		}
		return Credential{}, ErrReconnectGitHub
	}
	if err != nil {
		return Credential{}, fmt.Errorf("refresh token: %w", err)
	}

	now := time.Now()
	newAccessExp := now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	newRefreshExp := now.Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second)
	if err := ts.secrets.Put(secretRef, map[string]string{
		fieldAccessToken:      tok.AccessToken,
		fieldRefreshToken:     tok.RefreshToken,
		fieldTokenType:        tok.TokenType,
		fieldScope:            tok.Scope,
		fieldAccessExpiresAt:  newAccessExp.UTC().Format(time.RFC3339),
		fieldRefreshExpiresAt: newRefreshExp.UTC().Format(time.RFC3339),
	}); err != nil {
		return Credential{}, fmt.Errorf("persist rotated tokens: %w", err)
	}

	// Keep the non-sensitive metadata hints fresh and revoked=false.
	meta, _ := json.Marshal(map[string]any{
		"expires_in":               tok.ExpiresIn,
		"refresh_token_expires_in": tok.RefreshTokenExpiresIn,
		"scope":                    tok.Scope,
		"revoked":                  false,
	})
	if _, err := ts.q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{
		UserID:    uid,
		Metadata:  meta,
		SecretRef: secretRef,
	}); err != nil {
		return Credential{}, fmt.Errorf("update github link metadata: %w", err)
	}

	return Credential{AccessToken: tok.AccessToken, Expiry: newAccessExp}, nil
}

// markRevoked flags the grant as revoked in github_links.metadata, preserving the
// secret_ref. All git ops then fail with ErrReconnectGitHub until re-login.
func (ts *TokenSource) markRevoked(ctx context.Context, uid pgtype.UUID, current []byte) error {
	m := map[string]any{}
	_ = json.Unmarshal(current, &m)
	m["revoked"] = true
	meta, err := json.Marshal(m)
	if err != nil {
		return err
	}
	link, err := ts.q.GetGitHubLink(ctx, uid)
	if err != nil {
		return err
	}
	_, err = ts.q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{
		UserID:    uid,
		Metadata:  meta,
		SecretRef: link.SecretRef,
	})
	return err
}

// metadataRevoked reports whether github_links.metadata marks the grant revoked.
func metadataRevoked(meta []byte) bool {
	var m struct {
		Revoked bool `json:"revoked"`
	}
	_ = json.Unmarshal(meta, &m)
	return m.Revoked
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
