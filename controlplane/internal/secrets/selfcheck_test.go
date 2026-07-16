package secrets_test

import (
	"errors"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
)

// probePath mirrors the (unexported) path SelfCheck writes, so tests can assert
// the probe is cleaned up afterward. Keep in sync with secrets.selfCheckPath.
const probePath = "secret/machines/_selfcheck/probe"

// TestSelfCheckHappyPath: a fully-writable store passes and leaves no probe
// behind. Uses MemStore so it needs no container.
func TestSelfCheckHappyPath(t *testing.T) {
	s := secrets.NewMemStore()
	if err := secrets.SelfCheck(s); err != nil {
		t.Fatalf("SelfCheck on a writable store should pass: %v", err)
	}
	if _, err := s.Get(probePath); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("probe should be deleted after SelfCheck, got %v", err)
	}
}

// TestSelfCheckSurfacesWriteError: a store that fails to write surfaces the
// error (and it is not a permission denial, since MemStore has no policies).
func TestSelfCheckSurfacesWriteError(t *testing.T) {
	s := failingStore{err: errors.New("backend down")}
	err := secrets.SelfCheck(s)
	if err == nil {
		t.Fatal("expected SelfCheck to fail when the store rejects writes")
	}
	if secrets.IsPermissionDenied(err) {
		t.Fatalf("a non-403 error must not be classified as permission denied: %v", err)
	}
}

// failingStore is a Store whose Put always errors, to drive SelfCheck's
// write-failure path without a real backend.
type failingStore struct{ err error }

func (f failingStore) Put(string, map[string]string) error   { return f.err }
func (f failingStore) Get(string) (map[string]string, error) { return nil, secrets.ErrNotFound }
func (f failingStore) Delete(string) error                   { return nil }
