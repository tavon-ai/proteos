// Package session manages server-side sessions. The opaque token is returned
// to the client in a cookie; only its SHA-256 hash is stored, so a database
// leak does not expose live sessions.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// ErrInvalidSession is returned when a token does not map to a live session.
var ErrInvalidSession = errors.New("session: invalid or expired")

// refreshThreshold: only slide the expiry forward when at least this much of
// the TTL has elapsed, to avoid a DB write on every authenticated request.
const refreshThreshold = time.Hour

// Manager creates and validates sessions.
type Manager struct {
	q   *store.Queries
	ttl time.Duration
}

// NewManager returns a session manager backed by store with the given TTL.
func NewManager(q *store.Queries, ttl time.Duration) *Manager {
	return &Manager{q: q, ttl: ttl}
}

// TTL is the configured session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Create issues a new session for userID and returns the opaque token. The
// caller is responsible for placing the token in a cookie.
func (m *Manager) Create(ctx context.Context, userID pgtype.UUID) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := hashToken(token)

	_, err := m.q.CreateSession(ctx, store.CreateSessionParams{
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(m.ttl), Valid: true},
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// Authenticate validates a token and returns the owning user. On success it
// slides the expiry forward (at most once per refreshThreshold window).
func (m *Manager) Authenticate(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, ErrInvalidSession
	}
	hash := hashToken(token)
	row, err := m.q.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, ErrInvalidSession
		}
		return store.User{}, fmt.Errorf("lookup session: %w", err)
	}

	// Sliding refresh: extend only if we're past the threshold.
	if remaining := time.Until(row.Session.ExpiresAt.Time); remaining < m.ttl-refreshThreshold {
		_ = m.q.TouchSession(ctx, store.TouchSessionParams{
			ID:        row.Session.ID,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(m.ttl), Valid: true},
		})
	}
	return row.User, nil
}

// Revoke marks the session for the given token as revoked. Revoking an unknown
// or already-revoked token is a no-op (no error).
func (m *Manager) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := m.q.RevokeSession(ctx, hashToken(token)); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// hashToken returns the SHA-256 of the token string. Constant-time comparison
// is not needed at lookup (the DB indexes the hash), but we expose Equal for
// callers comparing hashes directly.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal reports whether two hashes match in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
