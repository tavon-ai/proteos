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
	"errors"
	"fmt"
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
