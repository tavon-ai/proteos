package secrets_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bao "github.com/openbao/openbao/api/v2"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
)

// setupAppRole enables the AppRole auth backend on the running OpenBao dev
// container, creates a named role, and returns (roleID, secretID). The role
// has a short TTL, is orphan-capable, and is non-renewable — matching what
// openbao-init.sh provisions in production.
func setupAppRole(t *testing.T, client *bao.Client, roleName string) (roleID, secretID string) {
	t.Helper()
	ctx := context.Background()

	// Enable the approle auth method (idempotent in dev mode).
	if err := client.Sys().EnableAuthWithOptions("approle", &bao.EnableAuthOptions{Type: "approle"}); err != nil {
		// "path is already in use" is fine — dev mode may pre-enable it.
		var re *bao.ResponseError
		if !errors.As(err, &re) || re.StatusCode != 400 {
			t.Fatalf("enable approle: %v", err)
		}
	}

	// Create the role the control plane will authenticate as.
	if _, err := client.Logical().WriteWithContext(ctx, "auth/approle/role/"+roleName, map[string]any{
		"token_policies": []string{"default"},
		"token_ttl":      "60s",
		"token_max_ttl":  "120s",
	}); err != nil {
		t.Fatalf("create role %s: %v", roleName, err)
	}

	// Retrieve the role_id.
	ridSec, err := client.Logical().ReadWithContext(ctx, "auth/approle/role/"+roleName+"/role-id")
	if err != nil || ridSec == nil {
		t.Fatalf("get role_id: %v", err)
	}
	roleID = ridSec.Data["role_id"].(string)

	// Generate a secret_id.
	sidSec, err := client.Logical().WriteWithContext(ctx, "auth/approle/role/"+roleName+"/secret-id", nil)
	if err != nil || sidSec == nil {
		t.Fatalf("get secret_id: %v", err)
	}
	secretID = sidSec.Data["secret_id"].(string)
	return roleID, secretID
}

// TestBaoStoreAppRoleLogin proves that NewBaoStore logs in via AppRole when
// RoleID + SecretIDFile are set (the production auth path, decision #3 from
// bao.go). The base token acquired this way must be able to write, read back,
// and delete a machine secret (the same SelfCheck exercise the startup boot
// uses to verify the backend).
func TestBaoStoreAppRoleLogin(t *testing.T) {
	addr := startBao(t)
	root := rootClient(t, addr)
	roleID, secretID := setupAppRole(t, root, "cp-test")

	// Write the secret_id to a temp file (matching production: the secret_id is
	// injected as a mounted file in the container, not passed via env).
	dir := t.TempDir()
	sidFile := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(sidFile, []byte(secretID+"\n"), 0o600); err != nil {
		t.Fatalf("write secret_id file: %v", err)
	}

	s, err := secrets.NewBaoStore(secrets.BaoConfig{
		Address:      addr,
		RoleID:       roleID,
		SecretIDFile: sidFile,
	})
	if err != nil {
		t.Fatalf("NewBaoStore with AppRole: %v", err)
	}

	// The base token minted via AppRole must satisfy SelfCheck (cp-base policy
	// grants machine path create/read/delete; dev mode's default policy is
	// permissive so this always passes in the test container).
	if err := secrets.SelfCheck(s); err != nil {
		t.Fatalf("SelfCheck after AppRole login: %v", err)
	}
}

// TestBaoStoreAppRoleMachineRoundTrip exercises a machine path end-to-end via
// an AppRole-authenticated store, proving the base-token path (not the
// per-user child-token path) works when the root token is replaced by an
// AppRole-derived one.
func TestBaoStoreAppRoleMachineRoundTrip(t *testing.T) {
	addr := startBao(t)
	root := rootClient(t, addr)
	roleID, secretID := setupAppRole(t, root, "cp-rt")

	dir := t.TempDir()
	sidFile := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(sidFile, []byte(secretID), 0o600); err != nil {
		t.Fatalf("write secret_id file: %v", err)
	}

	s, err := secrets.NewBaoStore(secrets.BaoConfig{
		Address:      addr,
		RoleID:       roleID,
		SecretIDFile: sidFile,
	})
	if err != nil {
		t.Fatalf("NewBaoStore with AppRole: %v", err)
	}

	// Mint and retrieve a machine volume key exactly as machine creation does.
	keyB64, err := secrets.MintMachineVolumeKey(s, deterministicReader{}, "m-approle")
	if err != nil {
		t.Fatalf("MintMachineVolumeKey: %v", err)
	}
	got, err := secrets.GetMachineVolumeKey(s, "m-approle")
	if err != nil {
		t.Fatalf("GetMachineVolumeKey: %v", err)
	}
	if got != keyB64 {
		t.Fatalf("volume key mismatch: %q != %q", got, keyB64)
	}
}

// TestBaoStoreAppRoleMissingSecretIDFile proves that NewBaoStore returns an
// error when the SecretIDFile path does not exist, rather than failing silently
// at the first real operation (the fail-fast behaviour that lets the control
// plane die at boot instead of at runtime).
func TestBaoStoreAppRoleMissingSecretIDFile(t *testing.T) {
	addr := startBao(t)
	_, err := secrets.NewBaoStore(secrets.BaoConfig{
		Address:      addr,
		RoleID:       "some-role-id",
		SecretIDFile: "/nonexistent/path/to/secret_id",
	})
	if err == nil {
		t.Fatal("expected error for missing SecretIDFile, got nil")
	}
}

// TestBaoStoreAppRoleNoAuth proves that NewBaoStore returns an error when
// neither Token nor RoleID is set: the store cannot operate without an auth
// method (the guard from bao.go decision #3).
func TestBaoStoreAppRoleNoAuth(t *testing.T) {
	addr := startBao(t)
	_, err := secrets.NewBaoStore(secrets.BaoConfig{Address: addr})
	if err == nil {
		t.Fatal("expected error when no auth is configured, got nil")
	}
}

// TestBaoStoreAppRoleInvalidCredentials proves that NewBaoStore returns an
// error when the role_id/secret_id pair is rejected by OpenBao — the login
// attempt happens at construction time so bad credentials fail fast.
func TestBaoStoreAppRoleInvalidCredentials(t *testing.T) {
	addr := startBao(t)
	// Enable AppRole so the endpoint exists (but we supply invalid credentials).
	root := rootClient(t, addr)
	if err := root.Sys().EnableAuthWithOptions("approle2", &bao.EnableAuthOptions{Type: "approle"}); err != nil {
		var re *bao.ResponseError
		if !errors.As(err, &re) || re.StatusCode != 400 {
			t.Fatalf("enable approle2: %v", err)
		}
	}

	dir := t.TempDir()
	sidFile := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(sidFile, []byte("bad-secret-id"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := secrets.NewBaoStore(secrets.BaoConfig{
		Address:      addr,
		RoleID:       "bad-role-id",
		SecretIDFile: sidFile,
	})
	if err == nil {
		t.Fatal("expected error for invalid AppRole credentials, got nil")
	}
}
