// Package providers reads the AI-provider registry (the providers table) and
// composes the per-user "key_set" view used by the secrets API and the injector.
// The registry is the source of truth for which agent CLIs exist, the command
// that launches each, the secret fields each needs, and an optional login-style
// setup command — the browser never chooses any of this (master-plan trust
// model). Phase 6 generalizes Phase 5's single-key shape into an ordered list of
// declared secret fields, so a new provider is a registry row, not new code.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// ErrUnknown is returned by Get when no provider has the given key.
var ErrUnknown = errors.New("providers: unknown provider")

// Validation errors returned by Provider.Validate. Handlers map all of them to
// 422 but the distinct sentinels make tests and logs precise.
var (
	// ErrUnknownField: the request named a field the provider does not declare.
	ErrUnknownField = errors.New("providers: unknown secret field")
	// ErrMissingField: a declared field was absent or empty in the request.
	ErrMissingField = errors.New("providers: missing required secret field")
	// ErrFieldTooLong: a field value exceeds MaxFieldValueLen.
	ErrFieldTooLong = errors.New("providers: secret field value too long")
)

// MaxFieldValueLen bounds an accepted secret field value (defensive; real keys
// are ~100 bytes).
const MaxFieldValueLen = 8192

// SecretField is one declared input a provider needs. Name is the field stored
// under the user's provider secret; Label is the human prompt the settings UI
// renders; Env is the environment variable the injector composes from the
// field's value. None of these are secret — only the value the user supplies is.
type SecretField struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Env   string `json:"env"`
}

// Provider is a registry row with secret_fields decoded.
type Provider struct {
	Key           string
	DisplayName   string
	LaunchCommand string
	// SetupCommand is an optional shell command the guest runs once per push to
	// complete login-style auth (empty when the provider's auth is pure-env).
	SetupCommand string
	// SecretFields is the ordered list of inputs this provider needs.
	SecretFields []SecretField
	Enabled      bool
}

// AllowsSubscriptionAuth reports whether the provider can run without a stored
// API key, falling back to subscription credentials baked into the machine image
// (e.g. Claude Code logged in via a Claude subscription in the guest's HOME).
// Only Claude Code supports this today; every other provider needs an env key.
// When true, the task/agent surfaces skip the no_provider_key gate and the
// injector still pushes the provider's launch command with an empty env so the
// guest can spawn it and let the CLI use its own stored login.
func (p Provider) AllowsSubscriptionAuth() bool {
	return p.LaunchCommand == "claude"
}

// Validate checks a fields map (field name → value) against the provider's
// declared fields: every supplied field must be declared, every declared field
// must be present and non-empty, and no value may exceed MaxFieldValueLen.
// Values are trimmed before the emptiness/length checks.
func (p Provider) Validate(fields map[string]string) error {
	declared := make(map[string]struct{}, len(p.SecretFields))
	for _, f := range p.SecretFields {
		declared[f.Name] = struct{}{}
	}
	for name := range fields {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownField, name)
		}
	}
	for _, f := range p.SecretFields {
		v := strings.TrimSpace(fields[f.Name])
		if v == "" {
			return fmt.Errorf("%w: %q", ErrMissingField, f.Name)
		}
		if len(v) > MaxFieldValueLen {
			return fmt.Errorf("%w: %q", ErrFieldTooLong, f.Name)
		}
	}
	return nil
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

// SetEnabled enables exactly the given provider keys and disables every other
// registered provider (Phase 6), aligning the registry with the CLIs actually
// baked into the rootfs so the UI never offers an unavailable provider. It
// returns the keys that were requested but are not registered (typos / providers
// dropped from the seeds), so the caller can warn — they are simply ignored.
func (r *Registry) SetEnabled(ctx context.Context, keys []string) ([]string, error) {
	known := map[string]struct{}{}
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		known[p.Key] = struct{}{}
	}
	var unknown []string
	for _, k := range keys {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if err := r.q.SetProvidersEnabled(ctx, keys); err != nil {
		return unknown, err
	}
	return unknown, nil
}

func fromRow(row store.Provider) (Provider, error) {
	var fields []SecretField
	if len(row.SecretFields) > 0 {
		if err := json.Unmarshal(row.SecretFields, &fields); err != nil {
			return Provider{}, fmt.Errorf("decode secret_fields for %q: %w", row.Key, err)
		}
	}
	setup := ""
	if row.SetupCommand != nil {
		setup = *row.SetupCommand
	}
	return Provider{
		Key:           row.Key,
		DisplayName:   row.DisplayName,
		LaunchCommand: row.LaunchCommand,
		SetupCommand:  setup,
		SecretFields:  fields,
		Enabled:       row.Enabled,
	}, nil
}
