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

// RevocationListener is notified when a session is revoked, with its id in
// canonical UUID string form. The gateway implements this to close any live
// terminal WebSockets bound to the now-dead session (Phase 3 decision #9).
type RevocationListener interface {
	SessionRevoked(sessionID string)
}

// Manager creates and validates sessions.
type Manager struct {
	q       *store.Queries
	ttl     time.Duration
	revoker RevocationListener // optional; set via SetRevocationListener
}

// NewManager returns a session manager backed by store with the given TTL.
func NewManager(q *store.Queries, ttl time.Duration) *Manager {
	return &Manager{q: q, ttl: ttl}
}

// SetRevocationListener registers the listener notified on Revoke. Called once
// at wiring time (the gateway registry). Passing nil disables notification.
func (m *Manager) SetRevocationListener(l RevocationListener) { m.revoker = l }

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

// Authenticate validates a token and returns the owning user and the session
// row (the latter so middleware can bind the session id to the request, for
// gateway revocation). On success it slides the expiry forward (at most once
// per refreshThreshold window).
func (m *Manager) Authenticate(ctx context.Context, token string) (store.User, store.Session, error) {
	if token == "" {
		return store.User{}, store.Session{}, ErrInvalidSession
	}
	hash := hashToken(token)
	row, err := m.q.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, store.Session{}, ErrInvalidSession
		}
		return store.User{}, store.Session{}, fmt.Errorf("lookup session: %w", err)
	}

	// Sliding refresh: extend only if we're past the threshold.
	if remaining := time.Until(row.Session.ExpiresAt.Time); remaining < m.ttl-refreshThreshold {
		_ = m.q.TouchSession(ctx, store.TouchSessionParams{
			ID:        row.Session.ID,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(m.ttl), Valid: true},
		})
	}
	return row.User, row.Session, nil
}

// AliveByID returns the owning user for a live (unexpired, unrevoked) session
// id, or ErrInvalidSession. Unlike Authenticate it takes the session id (not the
// token), so the Phase 8 machine-web path can re-check a parent session whose
// token never reaches the subdomain (fact #1). It does NOT slide the expiry —
// the parent session's own requests own that.
func (m *Manager) AliveByID(ctx context.Context, id pgtype.UUID) (store.User, error) {
	if !id.Valid {
		return store.User{}, ErrInvalidSession
	}
	row, err := m.q.GetSessionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, ErrInvalidSession
		}
		return store.User{}, fmt.Errorf("lookup session by id: %w", err)
	}
	return row.User, nil
}

// Revoke marks the session for the given token as revoked and notifies the
// revocation listener (if any) with the revoked session id. Revoking an unknown
// or already-revoked token is a no-op (no error, no notification).
func (m *Manager) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	id, err := m.q.RevokeSession(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // unknown or already revoked
		}
		return fmt.Errorf("revoke session: %w", err)
	}
	if m.revoker != nil && id.Valid {
		m.revoker.SessionRevoked(SessionIDString(id))
	}
	return nil
}

// SessionIDString renders a session UUID in canonical 8-4-4-4-12 form (the key
// the gateway registry uses). Returns "" for an invalid UUID.
func SessionIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	const hexd = "0123456789abcdef"
	var s [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			s[j] = '-'
			j++
		}
		s[j] = hexd[b[i]>>4]
		s[j+1] = hexd[b[i]&0x0f]
		j += 2
	}
	return string(s[:])
}

// hashToken returns the SHA-256 of the token string. Constant-time comparison
// is not needed at lookup (the DB indexes the hash), but we expose Equal for
// callers comparing hashes directly.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal reports whether two hashes match in constant time.
func qual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
