package sshkeys_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/sshkeys"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// genKey mints a fresh ed25519 keypair and returns its authorized_keys-format
// public key line with the given comment.
func genKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	return line + " " + comment
}

func newUser(t *testing.T, q *store.Queries, githubID int64) string {
	t.Helper()
	u, err := q.UpsertUser(context.Background(), store.UpsertUserParams{GithubUserID: githubID, Login: "sshuser"})
	if err != nil {
		t.Fatal(err)
	}
	return machine.UUIDString(u.ID)
}

// TestStoreAddListRevoke proves the basic lifecycle: adding a key makes it
// appear in List (metadata only), revoking it removes it from List (soft
// delete — the row survives, just filtered out).
func TestStoreAddListRevoke(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	s := sshkeys.NewStore(q)
	uid := newUser(t, q, 201)

	pub := genKey(t, "laptop")
	k, err := s.Add(ctx, uid, "  My laptop  ", pub)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if k.Label != "My laptop" {
		t.Fatalf("label not trimmed: %q", k.Label)
	}
	if k.Fingerprint == "" || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("bad fingerprint: %q", k.Fingerprint)
	}
	if k.ID == "" {
		t.Fatal("expected a non-empty id")
	}

	keys, err := s.List(ctx, uid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != k.ID {
		t.Fatalf("list = %+v, want one key matching %q", keys, k.ID)
	}

	existed, err := s.Revoke(ctx, uid, k.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !existed {
		t.Fatal("expected revoke to report the key existed")
	}

	keys, err = s.List(ctx, uid)
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("list after revoke = %+v, want empty", keys)
	}
}

// TestStoreRevokeUnknownOrForeign proves Revoke reports existed=false (not an
// error) for an id that does not exist, belongs to another user, or was already
// revoked — every case the API maps to 404.
func TestStoreRevokeUnknownOrForeign(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	s := sshkeys.NewStore(q)
	owner := newUser(t, q, 202)
	other := newUser(t, q, 203)

	// Unknown id (well-formed but never inserted).
	existed, err := s.Revoke(ctx, owner, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("revoke unknown: %v", err)
	}
	if existed {
		t.Fatal("revoke of an unknown id reported existed=true")
	}

	k, err := s.Add(ctx, owner, "laptop", genKey(t, "laptop"))
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// Foreign user cannot revoke another user's key.
	existed, err = s.Revoke(ctx, other, k.ID)
	if err != nil {
		t.Fatalf("revoke foreign: %v", err)
	}
	if existed {
		t.Fatal("a foreign user revoked someone else's key")
	}
	keys, _ := s.List(ctx, owner)
	if len(keys) != 1 {
		t.Fatalf("key was removed by a foreign revoke: %+v", keys)
	}

	// Revoking twice: the second call reports existed=false.
	if existed, err = s.Revoke(ctx, owner, k.ID); err != nil || !existed {
		t.Fatalf("first revoke: existed=%v err=%v", existed, err)
	}
	if existed, err = s.Revoke(ctx, owner, k.ID); err != nil || existed {
		t.Fatalf("second revoke: existed=%v err=%v, want false/nil", existed, err)
	}
}

// TestStoreAddDuplicateFingerprint proves a user cannot add the same active key
// twice (the unique index on (user_id, fingerprint) WHERE revoked_at IS NULL);
// a different user CAN add the identical key (no cross-user collision).
func TestStoreAddDuplicateFingerprint(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	s := sshkeys.NewStore(q)
	uid := newUser(t, q, 204)
	other := newUser(t, q, 205)
	pub := genKey(t, "shared")

	if _, err := s.Add(ctx, uid, "first", pub); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.Add(ctx, uid, "second", pub); err != sshkeys.ErrDuplicateKey {
		t.Fatalf("duplicate add err = %v, want ErrDuplicateKey", err)
	}
	if _, err := s.Add(ctx, other, "diff user", pub); err != nil {
		t.Fatalf("a different user adding the same key should succeed: %v", err)
	}
}

// TestStoreAddInvalidKey proves an unparsable public key is rejected with
// ErrInvalidKey rather than reaching Postgres.
func TestStoreAddInvalidKey(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	s := sshkeys.NewStore(q)
	uid := newUser(t, q, 206)

	if _, err := s.Add(ctx, uid, "bad", "not an ssh key"); err != sshkeys.ErrInvalidKey {
		t.Fatalf("err = %v, want ErrInvalidKey", err)
	}
}

// TestStoreActiveAuthorizedKeys proves the rendered blob is one
// authorized_keys line per active key, empty for a user with none, and drops a
// revoked key on the next render — exactly the shape the injector pushes
// verbatim to .ssh/authorized_keys.
func TestStoreActiveAuthorizedKeys(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	s := sshkeys.NewStore(q)
	uid := newUser(t, q, 207)

	blob, err := s.ActiveAuthorizedKeys(ctx, uid)
	if err != nil {
		t.Fatalf("empty blob: %v", err)
	}
	if blob != "" {
		t.Fatalf("blob for a user with no keys = %q, want empty", blob)
	}

	k1, err := s.Add(ctx, uid, "one", genKey(t, "one"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Add(ctx, uid, "two", genKey(t, "two"))
	if err != nil {
		t.Fatal(err)
	}

	blob, err = s.ActiveAuthorizedKeys(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(blob, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("blob = %q, want 2 lines", blob)
	}
	if !strings.Contains(blob, k1.PublicKey) || !strings.Contains(blob, k2.PublicKey) {
		t.Fatalf("blob missing a key: %q", blob)
	}

	if _, err := s.Revoke(ctx, uid, k1.ID); err != nil {
		t.Fatal(err)
	}
	blob, err = s.ActiveAuthorizedKeys(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blob, k1.PublicKey) {
		t.Fatalf("revoked key still in blob: %q", blob)
	}
	if !strings.Contains(blob, k2.PublicKey) {
		t.Fatalf("active key missing from blob: %q", blob)
	}
}
