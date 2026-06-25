package store_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

func TestUpsertUserIsIdempotentByGitHubID(t *testing.T) {
	_, q := testutil.Postgres(t)
	ctx := context.Background()

	u1, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 42, Login: "octocat", Email: "oc@example.com", AvatarUrl: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Re-login with changed profile fields: same row id, updated fields.
	u2, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 42, Login: "octocat-renamed", Email: "new@example.com", AvatarUrl: "a2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID != u2.ID {
		t.Fatalf("expected stable id, got %v then %v", u1.ID, u2.ID)
	}
	if u2.Login != "octocat-renamed" || u2.Email != "new@example.com" || u2.AvatarUrl != "a2" {
		t.Fatalf("profile not updated: %+v", u2)
	}
}

func TestSessionLifecycle(t *testing.T) {
	_, q := testutil.Postgres(t)
	ctx := context.Background()

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 1, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256([]byte("tok"))
	_, err = q.CreateSession(ctx, store.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: hash[:],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Live session resolves to the user.
	row, err := q.GetSessionByTokenHash(ctx, hash[:])
	if err != nil {
		t.Fatalf("lookup live session: %v", err)
	}
	if row.User.ID != user.ID {
		t.Fatalf("wrong user on session")
	}

	// After revoke, lookup finds nothing.
	if _, err := q.RevokeSession(ctx, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetSessionByTokenHash(ctx, hash[:]); err == nil {
		t.Fatal("expected revoked session to be unresolvable")
	}
}

func TestExpiredSessionNotReturned(t *testing.T) {
	_, q := testutil.Postgres(t)
	ctx := context.Background()

	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 2, Login: "u2"})
	hash := sha256.Sum256([]byte("expired"))
	_, err := q.CreateSession(ctx, store.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: hash[:],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetSessionByTokenHash(ctx, hash[:]); err == nil {
		t.Fatal("expected expired session to be unresolvable")
	}
}

func TestUpsertGitHubLinkStoresOnlyRef(t *testing.T) {
	_, q := testutil.Postgres(t)
	ctx := context.Background()

	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 3, Login: "u3"})
	link, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{
		UserID:    user.ID,
		Metadata:  []byte(`{"scope":"repo"}`),
		SecretRef: "secret/users/x/github",
	})
	if err != nil {
		t.Fatal(err)
	}
	if link.SecretRef != "secret/users/x/github" {
		t.Fatalf("secret_ref not persisted: %q", link.SecretRef)
	}

	// Upsert again updates ref + metadata, keeps single row.
	link2, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{
		UserID:    user.ID,
		Metadata:  []byte(`{"scope":"repo,user"}`),
		SecretRef: "secret/users/x/github",
	})
	if err != nil {
		t.Fatal(err)
	}
	if link2.UserID != link.UserID {
		t.Fatal("expected same link row")
	}
}

func TestMigrateDownUp(t *testing.T) {
	// Verifies migrations are reversible: down to zero, then back up clean.
	url := testutil.DatabaseURL(t)
	if err := store.MigrateDown(url); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := store.Migrate(url); err != nil {
		t.Fatalf("up: %v", err)
	}
}
