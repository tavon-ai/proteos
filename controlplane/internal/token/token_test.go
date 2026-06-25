package token_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	"github.com/tavon-ai/proteos/controlplane/internal/token"
)

func newUser(t *testing.T, q *store.Queries) store.User {
	t.Helper()
	u, err := q.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 42, Login: "octocat", Email: "o@example.com",
	})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	return u
}

func TestCreateAndAuthenticate(t *testing.T) {
	_, q := testutil.Postgres(t)
	m := token.NewManager(q)
	u := newUser(t, q)
	ctx := context.Background()

	created, err := m.Create(ctx, u.ID, "laptop", 0) // never expires
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(created.Plaintext, "proteos_pat_") {
		t.Fatalf("plaintext %q lacks brand prefix", created.Plaintext)
	}
	if !strings.HasPrefix(created.Plaintext, created.Row.Prefix) {
		t.Fatalf("display prefix %q is not a prefix of the token", created.Row.Prefix)
	}
	if len(created.Row.TokenHash) != 32 {
		t.Fatalf("token hash len = %d, want 32 (sha-256)", len(created.Row.TokenHash))
	}

	gotUser, gotTok, err := m.Authenticate(ctx, created.Plaintext)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if gotUser.ID != u.ID {
		t.Fatalf("authenticated wrong user")
	}
	if gotTok.ID != created.Row.ID {
		t.Fatalf("authenticated wrong token row")
	}
}

func TestAuthenticateRejectsGarbageAndEmpty(t *testing.T) {
	_, q := testutil.Postgres(t)
	m := token.NewManager(q)
	ctx := context.Background()
	for _, tok := range []string{"", "proteos_pat_not-a-real-token"} {
		if _, _, err := m.Authenticate(ctx, tok); err != token.ErrInvalidToken {
			t.Fatalf("Authenticate(%q) err = %v, want ErrInvalidToken", tok, err)
		}
	}
}

func TestRevoke(t *testing.T) {
	_, q := testutil.Postgres(t)
	m := token.NewManager(q)
	u := newUser(t, q)
	ctx := context.Background()

	created, err := m.Create(ctx, u.ID, "ci", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err := m.Revoke(ctx, u.ID, created.Row.ID)
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	// Revoked token no longer authenticates.
	if _, _, err := m.Authenticate(ctx, created.Plaintext); err != token.ErrInvalidToken {
		t.Fatalf("revoked token authenticated: %v", err)
	}
	// Revoking again is a no-op (already revoked).
	if ok, _ := m.Revoke(ctx, u.ID, created.Row.ID); ok {
		t.Fatalf("second revoke reported success")
	}
}

func TestRevokeOtherUsersTokenFails(t *testing.T) {
	pool, q := testutil.Postgres(t)
	m := token.NewManager(q)
	u := newUser(t, q)
	other, err := q.UpsertUser(context.Background(), store.UpsertUserParams{GithubUserID: 7, Login: "mallory", Email: "m@example.com"})
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	_ = pool
	ctx := context.Background()

	created, _ := m.Create(ctx, u.ID, "mine", 0)
	ok, err := m.Revoke(ctx, other.ID, created.Row.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok {
		t.Fatalf("another user revoked my token")
	}
	// Token still works for the real owner.
	if _, _, err := m.Authenticate(ctx, created.Plaintext); err != nil {
		t.Fatalf("owner's token broke: %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	pool, q := testutil.Postgres(t)
	m := token.NewManager(q)
	u := newUser(t, q)
	ctx := context.Background()

	created, err := m.Create(ctx, u.ID, "shortlived", time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the expiry into the past.
	if _, err := pool.Exec(ctx, "UPDATE personal_access_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1", created.Row.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, _, err := m.Authenticate(ctx, created.Plaintext); err != token.ErrInvalidToken {
		t.Fatalf("expired token authenticated: %v", err)
	}
}

func TestListExcludesRevoked(t *testing.T) {
	_, q := testutil.Postgres(t)
	m := token.NewManager(q)
	u := newUser(t, q)
	ctx := context.Background()

	a, _ := m.Create(ctx, u.ID, "a", 0)
	b, _ := m.Create(ctx, u.ID, "b", 0)
	if _, err := m.Revoke(ctx, u.ID, a.Row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, err := m.List(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != b.Row.ID {
		t.Fatalf("list = %d tokens, want only the live one", len(list))
	}
}
