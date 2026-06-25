// Package injector pushes a user's provider secrets into a running machine's
// guest agent over the existing node-agent byte tunnel (Phase 5 decision #7).
// It is a control-plane push: the node-agent stays an opaque pipe and no
// guest-side credential is needed. The push composes provider definitions from
// the registry (the launch command) and the user's stored secrets (the env), so
// the browser never chooses what runs or what env it gets.
//
// Injection fires (a) from the poller on every * → running transition — which
// after Phase 4 includes resumes, satisfying "every start and resume" by
// construction — and (b) idempotently before any agent launch. The guest stores
// the definitions on tmpfs; re-pushing on every resume keeps them fresh.
package injector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/profile"
	"github.com/tavon-ai/proteos/controlplane/internal/providers"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
)

// GuestDialer opens the opaque byte tunnel to a machine's guest agent at the
// given guest port. *nodeclient.Client satisfies it; secret injection rides the
// terminal port (agentapi.GuestTerminalPort).
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error)
}

// Injector reads provider secrets and pushes them to guests.
type Injector struct {
	guests   GuestDialer
	registry *providers.Registry
	secrets  secrets.Store
	audit    *audit.Recorder
	profile  *profile.Store
}

// New builds an Injector. prof may be nil (no portable-profile items are then
// composed; provider secrets behave exactly as before).
func New(guests GuestDialer, registry *providers.Registry, sec secrets.Store, rec *audit.Recorder, prof *profile.Store) *Injector {
	return &Injector{guests: guests, registry: registry, secrets: sec, audit: rec, profile: prof}
}

// pushTimeout bounds a single tunnel dial + PUT /secrets.
const pushTimeout = 15 * time.Second

// Inject composes the user's full provider set and pushes it to the machine's
// guest (replace-all). An empty set is still pushed, so a revoked key is cleared
// on the guest by the next injection.
func (i *Injector) Inject(ctx context.Context, userID, machineID string) error {
	req, err := i.compose(ctx, userID)
	if err != nil {
		return fmt.Errorf("compose secrets: %w", err)
	}
	return i.push(ctx, machineID, req)
}

// InjectAsync runs Inject in the background with bounded retry/backoff. It never
// blocks the caller (the poller's lifecycle must not depend on Bao or guest
// availability) and logs a final failure. Secret values are never logged.
func (i *Injector) InjectAsync(userID, machineID string) {
	go func() {
		backoff := time.Second
		const maxAttempts = 5
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
			err := i.Inject(ctx, userID, machineID)
			cancel()
			if err == nil {
				return
			}
			slog.Warn("injector: push failed", "machine", machineID, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				time.Sleep(backoff)
				if backoff < 16*time.Second {
					backoff *= 2
				}
			}
		}
		slog.Error("injector: giving up after retries", "machine", machineID)
	}()
}

// compose builds the SecretsRequest for a user: for each enabled provider the
// user has a key for, it maps the provider's secret_env (env var → secret field)
// to the stored values. Each secret read is audited (target = path, never value).
func (i *Injector) compose(ctx context.Context, userID string) (guestwire.SecretsRequest, error) {
	provs, err := i.registry.List(ctx)
	if err != nil {
		return guestwire.SecretsRequest{}, err
	}

	// Resolve the user's env-kind portable-profile items once and group the
	// provider-tied ones by provider. A provider credential (e.g. the Claude
	// subscription token → CLAUDE_CODE_OAUTH_TOKEN) must be merged into *that
	// provider's* Env, not a standalone entry: login shells source every
	// provider's env file, but an agent session overlays only the launched
	// provider's env, so a standalone entry would reach the terminal but not an
	// agent-launched `claude`.
	byProvider := map[string]map[string]string{}
	var files []guestwire.FileDef
	if i.profile != nil {
		envItems, err := i.profile.EnvValues(ctx, userID)
		if err != nil {
			return guestwire.SecretsRequest{}, fmt.Errorf("profile env: %w", err)
		}
		for _, it := range envItems {
			if it.Provider == "" {
				continue // generic (non-provider) env items are deferred (login-shell-only)
			}
			env := byProvider[it.Provider]
			if env == nil {
				env = map[string]string{}
				byProvider[it.Provider] = env
			}
			env[it.Target] = it.Value
		}
		// File-kind items (Phase 3): materialized under $HOME by the guest. Pushed
		// alongside providers in the same replace-all request.
		fileItems, err := i.profile.FileValues(ctx, userID)
		if err != nil {
			return guestwire.SecretsRequest{}, fmt.Errorf("profile files: %w", err)
		}
		for _, f := range fileItems {
			files = append(files, guestwire.FileDef{Path: f.Path, Mode: uint32(f.Mode), Content: f.Value})
		}
	}

	out := guestwire.SecretsRequest{Providers: map[string]guestwire.ProviderDef{}, Files: files}
	for _, p := range provs {
		if !p.Enabled {
			continue
		}
		path := secrets.UserProviderPath(userID, p.Key)
		data, err := i.secrets.Get(path)
		if errors.Is(err, secrets.ErrNotFound) {
			// No stored key. A subscription-capable provider (Claude Code) still
			// gets its launch command pushed, so the guest can spawn it and let the
			// subscription credential authenticate; any other provider is simply
			// skipped (it cannot run without its key). The env is keyless here — no
			// ANTHROPIC_API_KEY is emitted — but a provider-tied profile credential
			// (the OAuth token) is merged in so `claude` runs authenticated without
			// an interactive login.
			if p.AllowsSubscriptionAuth() {
				env := byProvider[p.Key]
				if env == nil {
					env = map[string]string{}
				}
				out.Providers[p.Key] = guestwire.ProviderDef{
					Command:      p.LaunchCommand,
					Env:          env,
					SetupCommand: p.SetupCommand,
				}
			}
			continue
		}
		if err != nil {
			return guestwire.SecretsRequest{}, fmt.Errorf("read %s: %w", p.Key, err)
		}
		i.audit.Record(ctx, audit.Entry{
			Actor:  audit.ActorSystemInjector,
			Action: audit.ActionSecretRead,
			Target: path,
		})
		env := make(map[string]string, len(p.SecretFields))
		for _, f := range p.SecretFields {
			if v, ok := data[f.Name]; ok {
				env[f.Env] = v
			}
		}
		out.Providers[p.Key] = guestwire.ProviderDef{
			Command:      p.LaunchCommand,
			Env:          env,
			SetupCommand: p.SetupCommand,
		}
	}
	return out, nil
}

// push dials the guest tunnel and PUTs /secrets over a one-shot HTTP client
// whose transport returns the tunnel for its single connection (the same trick
// the gateway uses for the guest WebSocket).
func (i *Injector) push(ctx context.Context, machineID string, req guestwire.SecretsRequest) error {
	tunnel, err := i.guests.DialGuest(ctx, machineID, agentapi.GuestTerminalPort)
	if err != nil {
		return fmt.Errorf("dial guest: %w", err)
	}
	defer tunnel.Close()

	used := false
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				if used {
					return nil, errors.New("injector: guest tunnel already consumed")
				}
				used = true
				return tunnel, nil
			},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://guest"+guestwire.RouteSecretsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("put secrets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("put secrets: guest returned %d", resp.StatusCode)
	}
	return nil
}
