package secrets_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
)

func TestFileStorePutGetDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := secrets.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}

	ref := secrets.UserGitHubPath("user-123")
	if err := s.Put(ref, map[string]string{"access_token": "gho_abc", "refresh_token": "ghr_xyz"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "gho_abc" || got["refresh_token"] != "ghr_xyz" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := s.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ref); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFileStoreMissingReturnsNotFound(t *testing.T) {
	s, err := secrets.NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("secret/nope"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStoreFileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := secrets.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("secret/x", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600, got %o", perm)
	}
}

func TestUserGitHubPathConvention(t *testing.T) {
	// Path must match the master-plan convention so the OpenBao impl is a
	// drop-in replacement in Phase 5.
	if got := secrets.UserGitHubPath("abc"); got != "secret/users/abc/github" {
		t.Fatalf("unexpected path: %q", got)
	}
}
