// Package token manages personal access tokens (PATs): long-lived bearer
// credentials a user mints to authenticate the CLI / programmatic callers
// without a browser session. The opaque token is shown to the user exactly once
// at creation; only its SHA-256 hash is stored, so a database leak does not
// expose live tokens. This mirrors the session package by design — the only
// differences are a user-chosen name, an optional (possibly absent) expiry, and
// a non-secret display prefix.
package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrInvalidToken is returned when a token does not map to a live PAT.
var ErrInvalidToken = errors.New("token: invalid, expired, or revoked")

const (
	// plaintextPrefix brands a ProteOS PAT so it is recognizable in logs/config
	// and distinguishable from a session token.
	plaintextPrefix = "proteos_pat_"
	// displayPrefixLen is how many leading characters of the plaintext are kept
	// as the non-secret `prefix` shown in listings. Revealing these few chars
	// leaks no meaningful entropy from the 256-bit secret.
	displayPrefixLen = len(plaintextPrefix) + 6
	// touchThreshold throttles the best-effort last_used_at bump so an active
	// CLI does not write to the row on every single request.
	touchThreshold = time.Minute
)

// Manager creates and validates personal access tokens.
type Manager struct {
	q *store.Queries
}

// NewManager returns a token manager backed by store.
func NewManager(q *store.Queries) *Manager { return &Manager{q: q} }

// Created is the result of minting a token: the one-time plaintext plus the
// stored row (without exposing the hash to callers beyond the row itself).
type Created struct {
	Plaintext string
	Row       store.PersonalAccessToken
}

// Create issues a new PAT for userID with the given name. expiresIn == 0 means
// the token never expires. The plaintext is returned only here; it cannot be
// recovered later.
func (m *Manager) Create(ctx context.Context, userID pgtype.UUID, name string, expiresIn time.Duration) (Created, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Created{}, fmt.Errorf("generate token: %w", err)
	}
	plaintext := plaintextPrefix + base64.RawURLEncoding.EncodeToString(raw)
	prefix := plaintext[:displayPrefixLen]

	var expiresAt pgtype.Timestamptz
	if expiresIn > 0 {
		expiresAt = pgtype.Timestamptz{Time: time.Now().Add(expiresIn), Valid: true}
	}

	row, err := m.q.CreatePAT(ctx, store.CreatePATParams{
		UserID:    userID,
		Name:      name,
		TokenHash: hashToken(plaintext),
		Prefix:    prefix,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return Created{}, fmt.Errorf("create token: %w", err)
	}
	return Created{Plaintext: plaintext, Row: row}, nil
}

// Authenticate validates a token and returns the owning user and the token row.
// On success it bumps last_used_at (at most once per touchThreshold window).
func (m *Manager) Authenticate(ctx context.Context, plaintext string) (store.User, store.PersonalAccessToken, error) {
	if plaintext == "" {
		return store.User{}, store.PersonalAccessToken{}, ErrInvalidToken
	}
	row, err := m.q.GetPATByTokenHash(ctx, hashToken(plaintext))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, store.PersonalAccessToken{}, ErrInvalidToken
		}
		return store.User{}, store.PersonalAccessToken{}, fmt.Errorf("lookup token: %w", err)
	}
	pat := row.PersonalAccessToken
	if !pat.LastUsedAt.Valid || time.Since(pat.LastUsedAt.Time) > touchThreshold {
		_ = m.q.TouchPATLastUsed(ctx, pat.ID)
	}
	return row.User, pat, nil
}

// List returns a user's live (unrevoked) tokens, newest first.
func (m *Manager) List(ctx context.Context, userID pgtype.UUID) ([]store.PersonalAccessToken, error) {
	return m.q.ListPATsByUserID(ctx, userID)
}

// Revoke revokes a token owned by userID. It reports whether a live token was
// actually revoked (false ⇒ unknown, not owned, or already revoked).
func (m *Manager) Revoke(ctx context.Context, userID, tokenID pgtype.UUID) (bool, error) {
	_, err := m.q.RevokePAT(ctx, store.RevokePATParams{ID: tokenID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("revoke token: %w", err)
	}
	return true, nil
}

// hashToken returns the SHA-256 of the plaintext token.
func hashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}
