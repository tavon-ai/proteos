package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// valueField is the single field name under a profile item's OpenBao secret.
const valueField = "value"

// Store persists profile items: the secret value goes to OpenBao (values live
// there only), the presence/metadata row to Postgres. The two are written
// value-first so a mid-write failure leaves at worst an inert orphan secret (the
// injector fetches by metadata, so a value without a row is never injected),
// never a metadata row promising a value that isn't there.
type Store struct {
	q     *store.Queries
	sec   secrets.Store
	audit *audit.Recorder
}

// NewStore builds a profile Store.
func NewStore(q *store.Queries, sec secrets.Store, rec *audit.Recorder) *Store {
	return &Store{q: q, sec: sec, audit: rec}
}

// Item is the non-secret metadata view of a profile item (never the value). Mode
// is set only for file-kind items (the file permission); 0 otherwise.
type Item struct {
	Key       string
	Kind      Kind
	Target    string
	Mode      os.FileMode
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

// Resolved is an env-kind profile item with its value read from OpenBao, used by
// the injector. Provider (from the Def registry) routes a credential into the
// matching provider's env.
type Resolved struct {
	Key      string
	Target   string
	Provider string
	Value    string
}

// ResolvedFile is a file-kind profile item with its value read from OpenBao, used
// by the injector to populate SecretsRequest.Files. Path is $HOME-relative.
type ResolvedFile struct {
	Key   string
	Path  string
	Mode  os.FileMode
	Value string
}

// Set stores value in OpenBao and upserts the metadata row. kind/target/ttl come
// from def (server authority), so the client supplies only the value.
func (s *Store) Set(ctx context.Context, userID string, def Def, value string) error {
	return s.setFields(ctx, userID, def, map[string]string{valueField: value})
}

// setFields is the core write: it stores the given OpenBao fields under the
// item's path and upserts its metadata row (kind/target/mode/expiry). Most items
// use a single `value` field; the SSH key additionally stores its public key in a
// sibling field so the UI can show it without the private key.
func (s *Store) setFields(ctx context.Context, userID string, def Def, fields map[string]string) error {
	uid, err := parseUID(userID)
	if err != nil {
		return err
	}
	path := secrets.UserProfilePath(userID, def.Key)
	if err := s.sec.Put(path, fields); err != nil {
		return fmt.Errorf("store profile value: %w", err)
	}
	var expires pgtype.Timestamptz
	if def.TTL > 0 {
		if err := expires.Scan(time.Now().Add(def.TTL)); err != nil {
			return fmt.Errorf("compute expiry: %w", err)
		}
	}
	var mode *int32
	if def.Kind == KindFile {
		m := int32(def.Mode & os.ModePerm)
		mode = &m
	}
	if _, err := s.q.UpsertProfileItem(ctx, store.UpsertProfileItemParams{
		UserID:    uid,
		Key:       def.Key,
		Kind:      string(def.Kind),
		Target:    def.Target,
		ExpiresAt: expires,
		Mode:      mode,
	}); err != nil {
		return fmt.Errorf("upsert profile metadata: %w", err)
	}
	s.audit.Record(ctx, audit.Entry{
		UserID: userID,
		Actor:  audit.UserActor(userID),
		Action: audit.ActionSecretPut,
		Target: path,
	})
	return nil
}

// Delete removes both the OpenBao value and the metadata row, stopping
// propagation. It returns whether an item existed (false ⇒ the user had no such
// item, which the API maps to 404). Deleting a missing item is otherwise a no-op.
func (s *Store) Delete(ctx context.Context, userID, key string) (existed bool, err error) {
	uid, err := parseUID(userID)
	if err != nil {
		return false, err
	}
	path := secrets.UserProfilePath(userID, key)
	if err := s.sec.Delete(path); err != nil {
		return false, fmt.Errorf("delete profile value: %w", err)
	}
	rows, err := s.q.DeleteProfileItem(ctx, store.DeleteProfileItemParams{UserID: uid, Key: key})
	if err != nil {
		return false, fmt.Errorf("delete profile metadata: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	s.audit.Record(ctx, audit.Entry{
		UserID: userID,
		Actor:  audit.UserActor(userID),
		Action: audit.ActionSecretDelete,
		Target: path,
	})
	return true, nil
}

// List returns a user's profile items as metadata only (never the value).
func (s *Store) List(ctx context.Context, userID string) ([]Item, error) {
	uid, err := parseUID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListProfileItems(ctx, uid)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		it := Item{
			Key:       r.Key,
			Kind:      Kind(r.Kind),
			Target:    r.Target,
			CreatedAt: r.CreatedAt.Time,
			UpdatedAt: r.UpdatedAt.Time,
		}
		if r.Mode != nil {
			it.Mode = os.FileMode(*r.Mode) & os.ModePerm
		}
		if r.ExpiresAt.Valid {
			t := r.ExpiresAt.Time
			it.ExpiresAt = &t
		}
		out = append(out, it)
	}
	return out, nil
}

// EnvValues resolves every env-kind profile item for the user to its target env
// var and value (read from OpenBao, each read audited as a system-injector read).
// An item whose metadata row exists but whose value is missing in OpenBao is
// skipped (the replace-all delete race), not an error.
func (s *Store) EnvValues(ctx context.Context, userID string) ([]Resolved, error) {
	items, err := s.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Resolved, 0, len(items))
	for _, it := range items {
		if it.Kind != KindEnv {
			continue
		}
		path := secrets.UserProfilePath(userID, it.Key)
		data, err := s.sec.Get(path)
		if errors.Is(err, secrets.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read profile %s: %w", it.Key, err)
		}
		v, ok := data[valueField]
		if !ok || v == "" {
			continue
		}
		s.audit.Record(ctx, audit.Entry{
			Actor:  audit.ActorSystemInjector,
			Action: audit.ActionSecretRead,
			Target: path,
		})
		// The Def registry is authoritative for the provider association; fall
		// back to no provider for an item not (or no longer) in the registry.
		provider := ""
		if def, ok := Lookup(it.Key); ok {
			provider = def.Provider
		}
		out = append(out, Resolved{Key: it.Key, Target: it.Target, Provider: provider, Value: v})
	}
	return out, nil
}

// FileValues resolves every file-kind profile item for the user to its
// $HOME-relative path, mode, and value (read from OpenBao, each read audited).
// An item whose value is missing in OpenBao is skipped (the replace-all delete
// race), not an error. The injector turns these into SecretsRequest.Files.
func (s *Store) FileValues(ctx context.Context, userID string) ([]ResolvedFile, error) {
	items, err := s.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedFile, 0, len(items))
	for _, it := range items {
		if it.Kind != KindFile {
			continue
		}
		path := secrets.UserProfilePath(userID, it.Key)
		data, err := s.sec.Get(path)
		if errors.Is(err, secrets.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read profile %s: %w", it.Key, err)
		}
		v, ok := data[valueField]
		if !ok {
			continue
		}
		mode := it.Mode
		if mode == 0 {
			mode = DefaultFileMode
		}
		s.audit.Record(ctx, audit.Entry{
			Actor:  audit.ActorSystemInjector,
			Action: audit.ActionSecretRead,
			Target: path,
		})
		out = append(out, ResolvedFile{Key: it.Key, Path: it.Target, Mode: mode, Value: v})
	}
	return out, nil
}

// --- Git identity (Phase 4) -------------------------------------------------
//
// Git identity is non-secret, so it lives in Postgres and is read by the
// git.configure control op (the single ~/.gitconfig writer). It is NOT a
// file-kind item, so the injector never materializes a competing ~/.gitconfig.

// SetGitIdentity sets/replaces the user's portable git identity.
func (s *Store) SetGitIdentity(ctx context.Context, userID, name, email string) error {
	uid, err := parseUID(userID)
	if err != nil {
		return err
	}
	_, err = s.q.UpsertGitIdentity(ctx, store.UpsertGitIdentityParams{UserID: uid, Name: name, Email: email})
	if err != nil {
		return fmt.Errorf("upsert git identity: %w", err)
	}
	return nil
}

// GitIdentity returns the user's portable git identity, or ok=false when unset.
func (s *Store) GitIdentity(ctx context.Context, userID string) (name, email string, ok bool, err error) {
	uid, err := parseUID(userID)
	if err != nil {
		return "", "", false, err
	}
	row, err := s.q.GetGitIdentity(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return row.Name, row.Email, true, nil
}

// ClearGitIdentity removes the portable git identity (revert to GitHub default).
// existed reports whether one was set (false ⇒ 404).
func (s *Store) ClearGitIdentity(ctx context.Context, userID string) (existed bool, err error) {
	uid, err := parseUID(userID)
	if err != nil {
		return false, err
	}
	n, err := s.q.DeleteGitIdentity(ctx, uid)
	if err != nil {
		return false, fmt.Errorf("delete git identity: %w", err)
	}
	return n > 0, nil
}

// --- SSH key (Phase 4) ------------------------------------------------------

// SetSSHKey stores the private key (the materialized file content) and the public
// key (sibling field, for the UI) under the SSH key item, plus the SSH client
// config item that lets git over SSH connect non-interactively. Both are
// file-kind items the injector materializes under ~/.ssh.
func (s *Store) SetSSHKey(ctx context.Context, userID, privatePEM, publicKey string) error {
	if err := s.setFields(ctx, userID, sshKeyDef(), map[string]string{
		valueField:     privatePEM,
		sshPublicField: publicKey,
	}); err != nil {
		return err
	}
	// Seed ~/.ssh/config so the first connection to a new host is accepted without
	// an interactive prompt (TOFU), keeping later host-key changes protected.
	return s.Set(ctx, userID, sshConfigDef(), sshConfigContent)
}

// SSHPublicKey returns the stored public key (non-secret), or ok=false when no SSH
// key is set. The private key is never returned by this or any other method.
func (s *Store) SSHPublicKey(ctx context.Context, userID string) (public string, ok bool, err error) {
	data, err := s.sec.Get(secrets.UserProfilePath(userID, SSHKeyItemKey))
	if errors.Is(err, secrets.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	p := data[sshPublicField]
	return p, p != "", nil
}

// DeleteSSHKey removes the SSH key and its config item. existed reports whether a
// key was present (false ⇒ 404).
func (s *Store) DeleteSSHKey(ctx context.Context, userID string) (existed bool, err error) {
	existed, err = s.Delete(ctx, userID, SSHKeyItemKey)
	if err != nil {
		return false, err
	}
	if _, err := s.Delete(ctx, userID, SSHConfigItemKey); err != nil {
		return existed, err
	}
	return existed, nil
}

func parseUID(userID string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return uid, fmt.Errorf("parse user id %q: %w", userID, err)
	}
	return uid, nil
}
