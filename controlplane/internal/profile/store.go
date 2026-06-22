package profile

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// Item is the non-secret metadata view of a profile item (never the value).
type Item struct {
	Key       string
	Kind      Kind
	Target    string
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

// Set stores value in OpenBao and upserts the metadata row. kind/target/ttl come
// from def (server authority), so the client supplies only the value.
func (s *Store) Set(ctx context.Context, userID string, def Def, value string) error {
	uid, err := parseUID(userID)
	if err != nil {
		return err
	}
	path := secrets.UserProfilePath(userID, def.Key)
	if err := s.sec.Put(path, map[string]string{valueField: value}); err != nil {
		return fmt.Errorf("store profile value: %w", err)
	}
	var expires pgtype.Timestamptz
	if def.TTL > 0 {
		if err := expires.Scan(time.Now().Add(def.TTL)); err != nil {
			return fmt.Errorf("compute expiry: %w", err)
		}
	}
	if _, err := s.q.UpsertProfileItem(ctx, store.UpsertProfileItemParams{
		UserID:    uid,
		Key:       def.Key,
		Kind:      string(def.Kind),
		Target:    def.Target,
		ExpiresAt: expires,
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
// propagation. Deleting a missing item is not an error.
func (s *Store) Delete(ctx context.Context, userID, key string) error {
	uid, err := parseUID(userID)
	if err != nil {
		return err
	}
	path := secrets.UserProfilePath(userID, key)
	if err := s.sec.Delete(path); err != nil {
		return fmt.Errorf("delete profile value: %w", err)
	}
	if err := s.q.DeleteProfileItem(ctx, store.DeleteProfileItemParams{UserID: uid, Key: key}); err != nil {
		return fmt.Errorf("delete profile metadata: %w", err)
	}
	s.audit.Record(ctx, audit.Entry{
		UserID: userID,
		Actor:  audit.UserActor(userID),
		Action: audit.ActionSecretDelete,
		Target: path,
	})
	return nil
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

func parseUID(userID string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return uid, fmt.Errorf("parse user id %q: %w", userID, err)
	}
	return uid, nil
}
