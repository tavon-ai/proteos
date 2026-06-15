// Package secrets holds the provider definitions the control plane injects into
// this guest at runtime (Phase 5 decision #7). The control plane pushes the set
// over the private transport; the guest keeps it in memory and mirrors each
// provider's environment to a 0600 file under a tmpfs env dir, so login shells
// can source the keys (via a profile.d snippet) and agent sessions can spawn the
// provider's command with the right environment.
//
// Secrets live only in RAM and on tmpfs — never on the rootfs image or the
// persistent disk. Replace-all semantics keep the on-disk env dir in lockstep
// with the in-memory map: a push that drops a provider deletes its env file.
//
// Phase 6 adds setup commands: a provider may carry a setup_command run once per
// push to complete login-style auth (e.g. Codex's `codex login --with-api-key`).
// The command runs asynchronously as a root login shell after the env file is
// written, with its output captured to the guest-agent log; it must be
// idempotent (run-on-every-push avoids any "has it run yet" state machine). A
// setup failure marks the provider degraded, and launching a degraded provider
// closes the agent WS with CloseReasonSetupFailed instead of spawning a broken
// TUI. A later successful push (e.g. key rotation) clears the degraded flag.
package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// setupTimeout bounds a single setup_command run. A login step that hangs past
// this is treated as a failure (the provider is marked degraded).
const setupTimeout = 60 * time.Second

// providerState is one injected provider: its definition plus its setup status.
// degraded is set when the provider's setup_command failed on the current push;
// a provider without a setup_command is never degraded. ready is closed once the
// setup command (if any) has settled — for providers without one it is created
// already-closed, so AwaitReady never blocks on them.
type providerState struct {
	def      guestwire.ProviderDef
	degraded bool
	ready    chan struct{}
}

// Store is the in-memory provider registry plus its tmpfs env-file mirror. Safe
// for concurrent use.
type Store struct {
	envDir string

	mu        sync.RWMutex
	providers map[string]providerState
	// gen increments on every Replace. An async setup goroutine captures the gen
	// of the push that spawned it and only writes its result back if gen still
	// matches — so a stale setup from a superseded push cannot clobber the status
	// of the current one.
	gen uint64

	// setupWG tracks in-flight setup goroutines so AwaitSetup can block until
	// they finish (used by tests for determinism; harmless in production).
	setupWG sync.WaitGroup

	// runSetup runs one provider's setup command and reports success. It is a
	// field so tests can substitute a deterministic runner; production uses
	// runSetupCommand.
	runSetup func(key string, def guestwire.ProviderDef) bool
}

// New creates a Store backed by envDir (created 0700 if absent). In production
// envDir is /run/proteos/env (tmpfs); tests pass a temp dir.
func New(envDir string) (*Store, error) {
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return nil, fmt.Errorf("create env dir: %w", err)
	}
	s := &Store{
		envDir:    envDir,
		providers: make(map[string]providerState),
	}
	s.runSetup = s.runSetupCommand
	// Start clean: drop any stale env files left by a previous run (these live on
	// tmpfs, so normally there are none, but a dev re-run over a plain dir might).
	if err := s.pruneExcept(nil); err != nil {
		return nil, err
	}
	return s, nil
}

// Replace installs providers as the complete injected set, replacing any prior
// set: it writes a 0600 env file per provider and deletes files for providers no
// longer present, then swaps the in-memory map. Providers with a setup_command
// are (re)run asynchronously and start un-degraded; a failing run flips the flag.
// Idempotent for equal input (setup commands must themselves be idempotent).
func (s *Store) Replace(providers map[string]guestwire.ProviderDef) error {
	s.mu.Lock()
	for key, def := range providers {
		if err := s.writeEnvFile(key, def.Env); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if err := s.pruneExcept(providers); err != nil {
		s.mu.Unlock()
		return err
	}

	s.gen++
	gen := s.gen
	next := make(map[string]providerState, len(providers))
	var withSetup []string
	for key, def := range providers {
		st := providerState{def: def, ready: make(chan struct{})}
		if strings.TrimSpace(def.SetupCommand) != "" {
			withSetup = append(withSetup, key)
		} else {
			close(st.ready) // no setup ⇒ ready immediately
		}
		next[key] = st
	}
	s.providers = next
	s.mu.Unlock()

	// Run setup commands after the env files exist and the map is published, so a
	// concurrent EnvList/Get sees the new definition. Each runs in its own
	// goroutine (a slow login must not block PUT /secrets) and writes its result
	// back only if its push is still current, then signals readiness so the launch
	// path can wait for the outcome (AwaitReady) instead of racing it.
	for _, key := range withSetup {
		def := providers[key]
		ready := next[key].ready
		s.setupWG.Add(1)
		go func(key string, def guestwire.ProviderDef, gen uint64, ready chan struct{}) {
			defer s.setupWG.Done()
			ok := s.runSetup(key, def)
			s.mu.Lock()
			if s.gen == gen {
				if st, present := s.providers[key]; present {
					st.degraded = !ok
					s.providers[key] = st
				}
			}
			s.mu.Unlock()
			close(ready)
		}(key, def, gen, ready)
	}
	return nil
}

// AwaitReady blocks until the provider's setup_command has settled (or it has
// none), bounded by ctx. The launch path calls it before checking Degraded so a
// freshly pushed provider's setup outcome is known before deciding whether to
// spawn — turning the async setup into a deterministic gate without making
// PUT /secrets block. A missing provider returns immediately.
func (s *Store) AwaitReady(ctx context.Context, key string) {
	s.mu.RLock()
	st, ok := s.providers[key]
	s.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case <-st.ready:
	case <-ctx.Done():
	}
}

// runSetupCommand runs def.SetupCommand as a root login shell with the
// provider's secret env overlaid on the process environment, capturing combined
// output to the guest-agent log. Returns true on a clean (exit 0) run. Secret
// values are not logged here — only the command's own output, which providers'
// login steps must not echo.
func (s *Store) runSetupCommand(key string, def guestwire.ProviderDef) bool {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", def.SetupCommand)
	cmd.Env = os.Environ()
	for _, kv := range sortedEnv(def.Env) {
		cmd.Env = append(cmd.Env, kv)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("provider setup failed", "provider", key, "err", err, "output", string(out))
		return false
	}
	slog.Info("provider setup ok", "provider", key, "output", string(out))
	return true
}

// AwaitSetup blocks until all in-flight setup commands have finished. It exists
// for deterministic tests (and is a harmless no-op when nothing is running).
func (s *Store) AwaitSetup() { s.setupWG.Wait() }

// Get returns the injected definition for a provider key.
func (s *Store) Get(key string) (guestwire.ProviderDef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.providers[key]
	return st.def, ok
}

// Degraded reports whether the provider is injected but unlaunchable because its
// setup_command failed on the current push. A missing provider returns false
// (the caller distinguishes "not injected" via Get).
func (s *Store) Degraded(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providers[key].degraded
}

// EnvList returns a provider's environment as KEY=VALUE pairs for an exec
// overlay (sorted for determinism), and whether the provider is injected.
func (s *Store) EnvList(key string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.providers[key]
	if !ok {
		return nil, false
	}
	return sortedEnv(st.def.Env), true
}

// sortedEnv renders an env map as sorted KEY=VALUE pairs.
func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// envFilePath is the tmpfs file mirroring one provider's environment.
func (s *Store) envFilePath(key string) string {
	return filepath.Join(s.envDir, key+".env")
}

// writeEnvFile writes env as a shell-sourceable 0600 file (export K='v', with
// single quotes escaped) so a login shell can source it. Written atomically.
func (s *Store) writeEnvFile(key string, env map[string]string) error {
	var b strings.Builder
	for _, kv := range sortedEnv(env) {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("='")
		b.WriteString(strings.ReplaceAll(v, "'", `'\''`))
		b.WriteString("'\n")
	}

	path := s.envFilePath(key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename env file: %w", err)
	}
	return nil
}

// pruneExcept removes every *.env file in the env dir whose provider key is not
// in keep. A nil keep removes them all.
func (s *Store) pruneExcept(keep map[string]guestwire.ProviderDef) error {
	entries, err := os.ReadDir(s.envDir)
	if err != nil {
		return fmt.Errorf("read env dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".env") {
			continue
		}
		key := strings.TrimSuffix(name, ".env")
		if _, ok := keep[key]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(s.envDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale env file: %w", err)
		}
	}
	return nil
}
