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

	guestwire "github.com/tavon/proteos/guestagent/api"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
)

// GuestDialer opens the opaque byte tunnel to a machine's guest agent.
// *nodeclient.Client satisfies it.
type GuestDialer interface {
	DialGuest(ctx context.Context, machineID string) (net.Conn, error)
}

// Injector reads provider secrets and pushes them to guests.
type Injector struct {
	guests   GuestDialer
	registry *providers.Registry
	secrets  secrets.Store
	audit    *audit.Recorder
}

// New builds an Injector.
func New(guests GuestDialer, registry *providers.Registry, sec secrets.Store, rec *audit.Recorder) *Injector {
	return &Injector{guests: guests, registry: registry, secrets: sec, audit: rec}
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
	out := guestwire.SecretsRequest{Providers: map[string]guestwire.ProviderDef{}}
	for _, p := range provs {
		if !p.Enabled {
			continue
		}
		path := secrets.UserProviderPath(userID, p.Key)
		data, err := i.secrets.Get(path)
		if errors.Is(err, secrets.ErrNotFound) {
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
	tunnel, err := i.guests.DialGuest(ctx, machineID)
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
