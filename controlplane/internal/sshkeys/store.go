// Package sshkeys is the inbound SSH login-key store: the list of public keys a
// user has registered to authorize logging into their machines over SSH. This is
// deliberately separate from controlplane/internal/profile's single-slot,
// server-generated, OUTBOUND git-SSH key (profile.SetSSHKey /
// GET/POST/DELETE /api/profile/ssh) — that key is used BY the guest to talk to
// git hosts; this store holds keys used TO log INTO the guest. The two never
// share a code path beyond both ending up as FileDef pushes through the same
// injector.
//
// Public keys are not secret (that is the entire point of asymmetric login
// auth), so they live directly in Postgres — no OpenBao round-trip, no injector
// secrets.Store dependency.
package sshkeys

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/ssh"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// MaxLabelLen / MaxPublicKeyLen bound an accepted key (defensive; a real
// authorized_keys line is well under a kilobyte).
const (
	MaxLabelLen     = 256
	MaxPublicKeyLen = 8192
)

// ErrInvalidKey is returned by Add when the public key does not parse as an
// authorized_keys-format SSH public key.
var ErrInvalidKey = errors.New("sshkeys: not a valid SSH public key")

// ErrDuplicateKey is returned by Add when the user already has this key
// (matched by fingerprint) active.
var ErrDuplicateKey = errors.New("sshkeys: key already added")

// Key is one login key's metadata (never anything secret — the whole record is
// public-key material).
type Key struct {
	ID          string
	Label       string
	PublicKey   string
	Fingerprint string
	CreatedAt   time.Time
}

// Store persists login keys in Postgres.
type Store struct {
	q *store.Queries
}

// NewStore builds a Store backed by q.
func NewStore(q *store.Queries) *Store {
	return &Store{q: q}
}

// Add validates and inserts a new active login key for userID. label is
// trimmed and must be non-empty; publicKey must parse as a single
// authorized_keys-format line (ErrInvalidKey otherwise). A duplicate active
// fingerprint for this user yields ErrDuplicateKey.
func (s *Store) Add(ctx context.Context, userID, label, publicKey string) (Key, error) {
	uid, err := parseUID(userID)
	if err != nil {
		return Key{}, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return Key{}, fmt.Errorf("sshkeys: label is empty")
	}
	if len(label) > MaxLabelLen {
		return Key{}, fmt.Errorf("sshkeys: label too long")
	}
	if len(publicKey) > MaxPublicKeyLen {
		return Key{}, fmt.Errorf("sshkeys: public key too long")
	}
	fp, normalized, err := parseAndFingerprint(publicKey)
	if err != nil {
		return Key{}, ErrInvalidKey
	}

	row, err := s.q.InsertSSHLoginKey(ctx, store.InsertSSHLoginKeyParams{
		UserID:      uid,
		Label:       label,
		PublicKey:   normalized,
		Fingerprint: fp,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return Key{}, ErrDuplicateKey
		}
		return Key{}, fmt.Errorf("insert ssh login key: %w", err)
	}
	return keyFromRow(row), nil
}

// List returns a user's active (non-revoked) login keys, oldest first.
func (s *Store) List(ctx context.Context, userID string) ([]Key, error) {
	uid, err := parseUID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListActiveSSHLoginKeys(ctx, uid)
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, keyFromRow(r))
	}
	return out, nil
}

// Revoke soft-deletes an active key owned by userID. existed reports whether a
// matching active key was found (false ⇒ unknown/foreign/already-revoked,
// which the API maps to 404).
func (s *Store) Revoke(ctx context.Context, userID, keyID string) (existed bool, err error) {
	uid, err := parseUID(userID)
	if err != nil {
		return false, err
	}
	kid, err := parseUID(keyID)
	if err != nil {
		return false, nil // a malformed id can never match — 404, not a 400
	}
	n, err := s.q.RevokeSSHLoginKey(ctx, store.RevokeSSHLoginKeyParams{ID: kid, UserID: uid})
	if err != nil {
		return false, fmt.Errorf("revoke ssh login key: %w", err)
	}
	return n > 0, nil
}

// ActiveAuthorizedKeys renders every active key for userID as one
// authorized_keys-format blob (one key per line), the shape the injector's
// compose() step pushes verbatim to .ssh/authorized_keys. Empty (not an error)
// when the user has no active keys, so the injector can skip the file.
func (s *Store) ActiveAuthorizedKeys(ctx context.Context, userID string) (string, error) {
	keys, err := s.List(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k.PublicKey)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// parseAndFingerprint parses an authorized_keys-format public key line and
// returns its SHA256 fingerprint plus a normalized single-line rendering
// (dropping any options/trailing whitespace the client sent, keeping only the
// algorithm + key + comment) so stored keys have a consistent shape.
func parseAndFingerprint(publicKey string) (fingerprint, normalized string, err error) {
	pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return "", "", err
	}
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(pk)), "\n")
	if comment != "" {
		line += " " + comment
	}
	return ssh.FingerprintSHA256(pk), line, nil
}

func keyFromRow(r store.UserSshLoginKey) Key {
	return Key{
		ID:          uuidString(r.ID),
		Label:       r.Label,
		PublicKey:   r.PublicKey,
		Fingerprint: r.Fingerprint,
		CreatedAt:   r.CreatedAt.Time,
	}
}

func parseUID(userID string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return uid, fmt.Errorf("parse user id %q: %w", userID, err)
	}
	return uid, nil
}

// uuidString renders a pgtype.UUID in canonical 8-4-4-4-12 form (empty if
// invalid — never expected here, since every row comes straight from Postgres).
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	var buf [36]byte
	encodeHexDashed(buf[:], u.Bytes)
	return string(buf[:])
}

func encodeHexDashed(dst []byte, src [16]byte) {
	const hexdig = "0123456789abcdef"
	j := 0
	for i, b := range src {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexdig[b>>4]
		dst[j+1] = hexdig[b&0xf]
		j += 2
	}
}
