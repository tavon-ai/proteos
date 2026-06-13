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
package secrets

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// Store is the in-memory provider registry plus its tmpfs env-file mirror. Safe
// for concurrent use.
type Store struct {
	envDir string

	mu        sync.RWMutex
	providers map[string]guestwire.ProviderDef
}

// New creates a Store backed by envDir (created 0700 if absent). In production
// envDir is /run/proteos/env (tmpfs); tests pass a temp dir.
func New(envDir string) (*Store, error) {
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return nil, fmt.Errorf("create env dir: %w", err)
	}
	s := &Store{
		envDir:    envDir,
		providers: make(map[string]guestwire.ProviderDef),
	}
	// Start clean: drop any stale env files left by a previous run (these live on
	// tmpfs, so normally there are none, but a dev re-run over a plain dir might).
	if err := s.pruneExcept(nil); err != nil {
		return nil, err
	}
	return s, nil
}

// Replace installs providers as the complete injected set, replacing any prior
// set: it writes a 0600 env file per provider and deletes files for providers no
// longer present, then swaps the in-memory map. Idempotent for equal input.
func (s *Store) Replace(providers map[string]guestwire.ProviderDef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, def := range providers {
		if err := s.writeEnvFile(key, def.Env); err != nil {
			return err
		}
	}
	if err := s.pruneExcept(providers); err != nil {
		return err
	}

	next := make(map[string]guestwire.ProviderDef, len(providers))
	maps.Copy(next, providers)
	s.providers = next
	return nil
}

// Get returns the injected definition for a provider key.
func (s *Store) Get(key string) (guestwire.ProviderDef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.providers[key]
	return def, ok
}

// EnvList returns a provider's environment as KEY=VALUE pairs for an exec
// overlay (sorted for determinism), and whether the provider is injected.
func (s *Store) EnvList(key string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.providers[key]
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(def.Env))
	for k, v := range def.Env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, true
}

// envFilePath is the tmpfs file mirroring one provider's environment.
func (s *Store) envFilePath(key string) string {
	return filepath.Join(s.envDir, key+".env")
}

// writeEnvFile writes env as a shell-sourceable 0600 file (export K='v', with
// single quotes escaped) so a login shell can source it. Written atomically.
func (s *Store) writeEnvFile(key string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("='")
		b.WriteString(strings.ReplaceAll(env[k], "'", `'\''`))
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
