// Package providers reads the AI-provider registry (the providers table) and
// composes the per-user "key_set" view used by the secrets API and the injector.
// The registry is the source of truth for which agent CLIs exist, the command
// that launches each, and how env vars map to fields of the user's provider
// secret — the browser never chooses any of this (master-plan trust model).
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// ErrUnknown is returned by Get when no provider has the given key.
var ErrUnknown = errors.New("providers: unknown provider")

// Provider is a registry row with secret_env decoded.
type Provider struct {
	Key           string
	DisplayName   string
	LaunchCommand string
	// SecretEnv maps an environment variable name to the field in the user's
	// provider secret holding its value (claude: ANTHROPIC_API_KEY → api_key).
	SecretEnv map[string]string
	Enabled   bool
}

// Registry reads providers from Postgres.
type Registry struct {
	q *store.Queries
}

// NewRegistry returns a Registry backed by q.
func NewRegistry(q *store.Queries) *Registry { return &Registry{q: q} }

// List returns every registered provider, ordered by key.
func (r *Registry) List(ctx context.Context) ([]Provider, error) {
	rows, err := r.q.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(rows))
	for _, row := range rows {
		p, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns one provider by key, or ErrUnknown.
func (r *Registry) Get(ctx context.Context, key string) (Provider, error) {
	row, err := r.q.GetProvider(ctx, key)
	if err != nil {
		return Provider{}, ErrUnknown
	}
	return fromRow(row)
}

func fromRow(row store.Provider) (Provider, error) {
	secretEnv := map[string]string{}
	if len(row.SecretEnv) > 0 {
		if err := json.Unmarshal(row.SecretEnv, &secretEnv); err != nil {
			return Provider{}, fmt.Errorf("decode secret_env for %q: %w", row.Key, err)
		}
	}
	return Provider{
		Key:           row.Key,
		DisplayName:   row.DisplayName,
		LaunchCommand: row.LaunchCommand,
		SecretEnv:     secretEnv,
		Enabled:       row.Enabled,
	}, nil
}
