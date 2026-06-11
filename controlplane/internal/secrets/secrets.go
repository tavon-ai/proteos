// Package secrets defines the Store interface used to hold sensitive material
// (GitHub tokens, provider API keys, machine identities) out of Postgres.
//
// Phase 1 ships only the dev file-backed implementation; OpenBao implements the
// same interface in Phase 5 as a drop-in. Path conventions follow the master
// plan so refs are stable across implementations:
//
//	secret/users/<user_id>/github
//	secret/users/<user_id>/providers/<key>
//	secret/machines/<machine_id>/identity
package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrNotFound is returned by Get/Delete when no secret exists at the path.
var ErrNotFound = errors.New("secrets: not found")

// Store is the key/value secret backend. Implementations must be safe for
// concurrent use.
type Store interface {
	// Put writes (overwriting) the data map at path.
	Put(path string, data map[string]string) error
	// Get reads the data map at path, or ErrNotFound.
	Get(path string) (map[string]string, error)
	// Delete removes the secret at path. Deleting a missing path is not an error.
	Delete(path string) error
}

// UserGitHubPath returns the canonical secret path for a user's GitHub tokens.
func UserGitHubPath(userID string) string {
	return fmt.Sprintf("secret/users/%s/github", userID)
}

// MachineVolumeKeyPath returns the canonical secret path for a machine's LUKS
// volume key (Phase 4 decision #2). The shape matches the OpenBao convention so
// the Phase 5 backend swap touches the store, not the callers.
func MachineVolumeKeyPath(machineID string) string {
	return fmt.Sprintf("secret/machines/%s/volume-key", machineID)
}

// volumeKeyField is the field name under MachineVolumeKeyPath holding the
// base64-encoded 32-byte key.
const volumeKeyField = "key_b64"

// MintMachineVolumeKey generates a fresh 32-byte volume key, stores it (base64)
// in the secret store at MachineVolumeKeyPath, and returns it base64-encoded.
// rnd is the randomness source (crypto/rand.Reader in production).
func MintMachineVolumeKey(s Store, rnd io.Reader, machineID string) (keyB64 string, err error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rnd, key); err != nil {
		return "", fmt.Errorf("generate volume key: %w", err)
	}
	keyB64 = base64.StdEncoding.EncodeToString(key)
	if err := s.Put(MachineVolumeKeyPath(machineID), map[string]string{volumeKeyField: keyB64}); err != nil {
		return "", fmt.Errorf("store volume key: %w", err)
	}
	return keyB64, nil
}

// GetMachineVolumeKey reads a machine's volume key (base64). ErrNotFound if the
// machine has no key minted.
func GetMachineVolumeKey(s Store, machineID string) (keyB64 string, err error) {
	data, err := s.Get(MachineVolumeKeyPath(machineID))
	if err != nil {
		return "", err
	}
	k, ok := data[volumeKeyField]
	if !ok {
		return "", ErrNotFound
	}
	return k, nil
}
