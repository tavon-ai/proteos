package secrets_test

import (
	"context"
	"errors"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tavon/proteos/controlplane/internal/secrets"
)

// devRootToken is the fixed root token the OpenBao dev-mode server boots with.
const devRootToken = "root"

// startBao boots an OpenBao dev-mode container (KV v2 auto-mounted at secret/),
// creates the proteos-user token role that BaoStore mints child tokens against,
// and returns the API address. The container is terminated on test cleanup.
func startBao(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "openbao/openbao:2.5.1",
			ExposedPorts: []string{"8200/tcp"},
			Env: map[string]string{
				"BAO_DEV_ROOT_TOKEN_ID":  devRootToken,
				"BAO_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
			},
			Cmd:        []string{"server", "-dev"},
			CapAdd:     []string{"IPC_LOCK"},
			WaitingFor: wait.ForListeningPort("8200/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start openbao container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8200/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	addr := "http://" + host + ":" + port.Port()

	// Create the proteos-user token role (init-script equivalent) so BaoStore can
	// mint per-user child tokens. orphan + short ttl + no renew, scoped by glob.
	client := rootClient(t, addr)
	if _, err := client.Logical().WriteWithContext(ctx, "auth/token/roles/proteos-user", map[string]any{
		"allowed_policies_glob": "user-*",
		"orphan":                true,
		"renewable":             false,
		"token_ttl":             "90s",
		"token_type":            "service",
	}); err != nil {
		t.Fatalf("create proteos-user token role: %v", err)
	}
	return addr
}

func rootClient(t *testing.T, addr string) *bao.Client {
	t.Helper()
	cfg := bao.DefaultConfig()
	cfg.Address = addr
	c, err := bao.NewClient(cfg)
	if err != nil {
		t.Fatalf("bao client: %v", err)
	}
	c.SetToken(devRootToken)
	return c
}

func newStore(t *testing.T, addr string) *secrets.BaoStore {
	t.Helper()
	s, err := secrets.NewBaoStore(secrets.BaoConfig{Address: addr, Token: devRootToken})
	if err != nil {
		t.Fatalf("new bao store: %v", err)
	}
	return s
}

// TestBaoStoreUserRoundTrip exercises a user path through put/get/delete with
// the per-user child-token machinery (policy ensure + token mint).
func TestBaoStoreUserRoundTrip(t *testing.T) {
	addr := startBao(t)
	s := newStore(t, addr)

	path := secrets.UserGitHubPath("alice")
	want := map[string]string{"access_token": "ghs_abc", "refresh_token": "ghr_xyz"}
	if err := s.Put(path, want); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.Get(path)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["access_token"] != "ghs_abc" || got["refresh_token"] != "ghr_xyz" {
		t.Fatalf("round-trip mismatch: %v", got)
	}

	if err := s.Delete(path); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(path); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestBaoStoreMachineRoundTrip exercises a machine path, which uses the base
// token directly (no child token).
func TestBaoStoreMachineRoundTrip(t *testing.T) {
	addr := startBao(t)
	s := newStore(t, addr)

	keyB64, err := secrets.MintMachineVolumeKey(s, deterministicReader{}, "m-1")
	if err != nil {
		t.Fatalf("mint volume key: %v", err)
	}
	got, err := secrets.GetMachineVolumeKey(s, "m-1")
	if err != nil {
		t.Fatalf("get volume key: %v", err)
	}
	if got != keyB64 {
		t.Fatalf("volume key mismatch: %q != %q", got, keyB64)
	}
}

// TestBaoStoreGetNotFound matches FileStore's ErrNotFound semantics on a path
// that was never written.
func TestBaoStoreGetNotFound(t *testing.T) {
	addr := startBao(t)
	s := newStore(t, addr)
	if _, err := s.Get(secrets.UserGitHubPath("nobody")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Deleting a missing path is not an error.
	if err := s.Delete(secrets.UserGitHubPath("nobody")); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// TestBaoStorePolicyIdempotency proves the in-process ensure cache and repeated
// policy writes coexist: many ops for the same user succeed.
func TestBaoStorePolicyIdempotency(t *testing.T) {
	addr := startBao(t)
	s := newStore(t, addr)
	path := secrets.UserGitHubPath("bob")
	for i := range 3 {
		if err := s.Put(path, map[string]string{"access_token": "v"}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// A second store (cold ensure cache) re-writes the identical policy fine.
	s2 := newStore(t, addr)
	if _, err := s2.Get(path); err != nil {
		t.Fatalf("get from second store: %v", err)
	}
}

// TestBaoStoreCrossUserDenial is the decision #2 proof: a child token scoped to
// user A's policy physically cannot read user B's path — the denial is enforced
// inside OpenBao, not in our Go code.
func TestBaoStoreCrossUserDenial(t *testing.T) {
	addr := startBao(t)
	s := newStore(t, addr)
	ctx := context.Background()

	// Seed user B's secret via the store (mints B's policy + token).
	if err := s.Put(secrets.UserGitHubPath("userB"), map[string]string{"access_token": "secretB"}); err != nil {
		t.Fatalf("seed userB: %v", err)
	}
	// Ensure user A's policy exists too (store-side) by writing A's own secret.
	if err := s.Put(secrets.UserGitHubPath("userA"), map[string]string{"access_token": "secretA"}); err != nil {
		t.Fatalf("seed userA: %v", err)
	}

	// Mint a raw child token scoped to user A's policy, exactly as BaoStore does.
	root := rootClient(t, addr)
	tokSec, err := root.Logical().WriteWithContext(ctx, "auth/token/create/proteos-user", map[string]any{
		"policies": []string{"user-userA"},
		"ttl":      "90s",
	})
	if err != nil {
		t.Fatalf("mint userA token: %v", err)
	}
	aClient := rootClient(t, addr)
	aClient.SetToken(tokSec.Auth.ClientToken)

	// A can read its own path.
	if _, err := aClient.Logical().ReadWithContext(ctx, "secret/data/users/userA/github"); err != nil {
		t.Fatalf("userA reading own path should succeed: %v", err)
	}
	// A cannot read user B's path: OpenBao returns 403.
	_, err = aClient.Logical().ReadWithContext(ctx, "secret/data/users/userB/github")
	if err == nil {
		t.Fatal("expected userA to be denied reading userB's path, got nil error")
	}
	var re *bao.ResponseError
	if !errors.As(err, &re) || re.StatusCode != 403 {
		t.Fatalf("expected 403 permission denied, got %v", err)
	}
}

// deterministicReader is a fixed entropy source for the volume-key test.
type deterministicReader struct{}

func (deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i)
	}
	return len(p), nil
}
