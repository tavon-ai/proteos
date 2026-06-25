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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/runas"
)

// defaultFileMode is applied to a file-kind profile item whose FileDef.Mode is 0.
const defaultFileMode = 0o600

// stateDirName is the per-home directory holding the guest's profile-file
// manifest (the list of files this guest materialized, for replace-all drop
// across reboots). It sits under the session user's $HOME on the persist disk.
const stateDirName = ".proteos"

// fileManifestName is the manifest file (a JSON array of $HOME-relative paths).
const fileManifestName = "profile-files.json"

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
	owner  runas.Identity // session user that login shells source env files as

	mu        sync.RWMutex
	providers map[string]providerState
	// gen increments on every Replace. An async setup goroutine captures the gen
	// of the push that spawned it and only writes its result back if gen still
	// matches — so a stale setup from a superseded push cannot clobber the status
	// of the current one.
	gen uint64

	// managedFiles is the set of $HOME-relative paths this guest has materialized
	// for file-kind profile items. It is loaded from the on-disk manifest at New
	// (so a file dropped while the machine was off is still removed on the next
	// push) and kept in lockstep with the manifest on every Replace.
	managedFiles map[string]struct{}

	// setupWG tracks in-flight setup goroutines so AwaitSetup can block until
	// they finish (used by tests for determinism; harmless in production).
	setupWG sync.WaitGroup

	// runSetup runs one provider's setup command and reports success. It is a
	// field so tests can substitute a deterministic runner; production uses
	// runSetupCommand.
	runSetup func(key string, def guestwire.ProviderDef) bool
}

// New creates a Store backed by envDir (created 0700 if absent). In production
// envDir is /run/proteos/env (tmpfs); tests pass a temp dir. owner is the
// unprivileged session user whose login shells source the env files; the dir
// and each env file are chowned to it so it can read them (root, which runs the
// agent, still bypasses these perms). For the root identity this is a no-op.
func New(envDir string, owner runas.Identity) (*Store, error) {
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return nil, fmt.Errorf("create env dir: %w", err)
	}
	if err := owner.Chown(envDir); err != nil {
		slog.Warn("secrets: chown env dir failed", "dir", envDir, "err", err)
	}
	s := &Store{
		envDir:       envDir,
		owner:        owner,
		providers:    make(map[string]providerState),
		managedFiles: make(map[string]struct{}),
	}
	s.runSetup = s.runSetupCommand
	// Start clean: drop any stale env files left by a previous run (these live on
	// tmpfs, so normally there are none, but a dev re-run over a plain dir might).
	if err := s.pruneExcept(nil); err != nil {
		return nil, err
	}
	// Load the file manifest (file-kind items persist on the disk, unlike env
	// files): this lets a later push that omits a file still remove it, even if
	// the item was deleted while the machine was off.
	s.managedFiles = s.loadManifest()
	return s, nil
}

// Replace installs the pushed set as the complete injected state, replacing any
// prior set: it writes a 0600 env file per provider (deleting files for providers
// no longer present), materializes file-kind profile items under $HOME (removing
// any it previously wrote that are now absent), then swaps the in-memory map.
// Providers with a setup_command are (re)run asynchronously and start
// un-degraded; a failing run flips the flag. Idempotent for equal input (setup
// commands must themselves be idempotent).
func (s *Store) Replace(req guestwire.SecretsRequest) error {
	providers := req.Providers
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
	// Materialize file-kind items and drop any previously-written file now absent
	// (replace-all). Done under the same lock as the env mirror.
	if err := s.writeFiles(req.Files); err != nil {
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
	if err := s.owner.Chown(path); err != nil {
		slog.Warn("secrets: chown env file failed", "path", path, "err", err)
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

// writeFiles materializes each file-kind item under the session user's $HOME with
// the requested mode and ownership, then removes any file this guest previously
// wrote that is no longer present (replace-all), and persists the new manifest.
// Caller holds s.mu. The session user owns the files (and any parent dirs the
// guest creates); the agent runs as root so it can always write them.
func (s *Store) writeFiles(files []guestwire.FileDef) error {
	// Fast path: a user with no file-kind items (the common case) never causes the
	// guest to touch $HOME or create the state dir/manifest.
	if len(files) == 0 && len(s.managedFiles) == 0 {
		return nil
	}
	next := make(map[string]struct{}, len(files))
	for _, f := range files {
		rel, full, err := s.resolveHomePath(f.Path)
		if err != nil {
			return err
		}
		mode := os.FileMode(f.Mode) & os.ModePerm
		if mode == 0 {
			mode = defaultFileMode
		}
		if err := s.writeOwnedFile(full, []byte(f.Content), mode); err != nil {
			return err
		}
		next[rel] = struct{}{}
	}
	// Drop files we wrote on a previous push that are absent now.
	for rel := range s.managedFiles {
		if _, keep := next[rel]; keep {
			continue
		}
		if _, _, err := s.validateRel(rel); err != nil {
			continue // never trust a manifest entry that no longer cleans safely
		}
		if err := os.Remove(filepath.Join(s.owner.Home, rel)); err != nil && !os.IsNotExist(err) {
			slog.Warn("secrets: remove dropped profile file failed", "err", err)
		}
	}
	s.managedFiles = next
	if err := s.saveManifest(next); err != nil {
		return err
	}
	return nil
}

// writeOwnedFile writes content to full atomically with the given mode, creating
// any missing parent dirs (0700, owned by the session user), and chowns the file
// to the session user. The value is never logged.
func (s *Store) writeOwnedFile(full string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(full)
	if err := s.mkdirOwned(dir); err != nil {
		return err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return fmt.Errorf("write profile file: %w", err)
	}
	// WriteFile honors umask; force the exact mode.
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod profile file: %w", err)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename profile file: %w", err)
	}
	if err := s.owner.Chown(full); err != nil {
		slog.Warn("secrets: chown profile file failed", "err", err)
	}
	return nil
}

// mkdirOwned ensures dir exists (0700) and that dir and any ancestors it had to
// create are owned by the session user, stopping at $HOME (which already exists).
func (s *Store) mkdirOwned(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	// Chown the chain from $HOME down to dir, so a freshly created ~/.ssh is owned
	// by the user (root-owned dirs in a user's home break tools like ssh).
	home := filepath.Clean(s.owner.Home)
	for d := filepath.Clean(dir); strings.HasPrefix(d, home) && d != home; d = filepath.Dir(d) {
		if err := s.owner.Chown(d); err != nil {
			slog.Warn("secrets: chown profile dir failed", "dir", d, "err", err)
		}
	}
	return nil
}

// resolveHomePath validates a $HOME-relative path and returns its cleaned
// relative form and absolute path under $HOME. It rejects empty/absolute paths
// and any path that escapes $HOME via "..".
func (s *Store) resolveHomePath(rel string) (cleanRel, full string, err error) {
	return s.validateRel(rel)
}

func (s *Store) validateRel(rel string) (cleanRel, full string, err error) {
	if strings.TrimSpace(rel) == "" {
		return "", "", fmt.Errorf("profile file: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("profile file: path %q must be $HOME-relative", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("profile file: path %q escapes $HOME", rel)
	}
	home := filepath.Clean(s.owner.Home)
	full = filepath.Join(home, clean)
	// Defense in depth: the joined path must stay within $HOME.
	if full != home && !strings.HasPrefix(full, home+string(filepath.Separator)) {
		return "", "", fmt.Errorf("profile file: path %q escapes $HOME", rel)
	}
	return clean, full, nil
}

// manifestPath is the on-disk manifest of materialized profile files.
func (s *Store) manifestPath() string {
	return filepath.Join(s.owner.Home, stateDirName, fileManifestName)
}

// loadManifest reads the set of previously-materialized $HOME-relative paths. A
// missing/unreadable manifest yields an empty set (the safe default).
func (s *Store) loadManifest() map[string]struct{} {
	out := make(map[string]struct{})
	b, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return out
	}
	var paths []string
	if err := json.Unmarshal(b, &paths); err != nil {
		slog.Warn("secrets: unreadable profile-file manifest; ignoring", "err", err)
		return out
	}
	for _, p := range paths {
		out[p] = struct{}{}
	}
	return out
}

// saveManifest persists the current set of materialized paths (sorted for a
// stable file), creating the state dir (owned by the session user) as needed.
func (s *Store) saveManifest(set map[string]struct{}) error {
	dir := filepath.Join(s.owner.Home, stateDirName)
	if err := s.mkdirOwned(dir); err != nil {
		return err
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	b, err := json.Marshal(paths)
	if err != nil {
		return fmt.Errorf("marshal profile-file manifest: %w", err)
	}
	path := s.manifestPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write profile-file manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename profile-file manifest: %w", err)
	}
	if err := s.owner.Chown(path); err != nil {
		slog.Warn("secrets: chown profile-file manifest failed", "err", err)
	}
	return nil
}
