// Package webfwd is the guest agent's Phase 8 code-server forward. It listens on
// a private transport (vsock:1025 in production, a unix socket in dev) that the
// node-agent tunnel reaches on agentapi.GuestWebPort, lazily starts and
// supervises code-server (decision #5), and raw-copies bytes between each
// accepted connection and code-server's loopback address (127.0.0.1:13337).
//
// It never parses what flows through — code-server speaks plain HTTP+WebSocket
// and must stay path-untouched (no prefix rewriting, historically the fragile
// part of proxying it). The control-plane gateway is the authenticator; this
// forward, like the terminal listener, is reachable only over the per-VM private
// transport (Phase 3 decision #10), so it does no auth of its own.
package webfwd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
)

// Forwarder accepts connections on a listener and bridges each to the backend
// (code-server), lazily ensuring the backend is up via the Supervisor first.
type Forwarder struct {
	ln      net.Listener
	backend string
	sup     *Supervisor // nil ⇒ assume backend already up (dev/e2e stub)
	dial    func(ctx context.Context, addr string) (net.Conn, error)
}

// New builds a Forwarder over ln, dialing backend for each connection. sup may
// be nil to skip supervision (the unsupervised dev/e2e path).
func New(ln net.Listener, backend string, sup *Supervisor) *Forwarder {
	return &Forwarder{ln: ln, backend: backend, sup: sup, dial: defaultDial}
}

// Serve runs the accept loop until ctx is cancelled or the listener errors. It
// closes the listener when ctx ends so a blocked Accept unblocks.
func (f *Forwarder) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = f.ln.Close()
	}()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go f.handle(ctx, conn)
	}
}

// handle ensures code-server is healthy, then bridges client↔backend. A failed
// Ensure or backend dial simply drops the client connection — the gateway maps
// the closed tunnel to a 502, exactly as for an unreachable guest.
func (f *Forwarder) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	if err := f.sup.Ensure(ctx); err != nil {
		slog.Warn("webfwd: code-server unavailable", "err", err)
		return
	}
	backend, err := f.dial(ctx, f.backend)
	if err != nil {
		slog.Warn("webfwd: dial code-server", "addr", f.backend, "err", err)
		return
	}
	defer backend.Close()

	bridge(client, backend)
}

// bridge copies bytes both ways until either side closes, then tears the other
// down. Same dumb-pipe shape as the node-agent's guest bridge.
func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// ErrDisabled is returned by Setup when no web listener is configured.
var ErrDisabled = errors.New("webfwd: disabled (no listen spec)")

// DefaultCodeServerArgs builds the code-server argument vector per decision #5:
// loopback-only bind, no auth (the gateway authenticates — same trust argument
// as the guest agent itself), telemetry/update-check off, and user-data /
// extensions under $HOME so they live on the Phase 4 persist disk. The default
// folder is the workspace. backend is the host:port code-server binds.
//
// Workspace-trust and the getting-started override are disabled so a fresh VM
// opens straight into /workspace as a trusted folder, with no trust modal or
// welcome page interrupting first use (the VM is the user's own private box).
func DefaultCodeServerArgs(backend, home, workspace string) []string {
	userData := codeServerUserDataDir(home)
	return []string{
		"--auth", "none",
		"--bind-addr", backend,
		"--disable-telemetry",
		"--disable-update-check",
		"--disable-workspace-trust",
		"--disable-getting-started-override",
		"--user-data-dir", userData,
		"--extensions-dir", userData + "/extensions",
		workspace,
	}
}

// codeServerUserDataDir is the --user-data-dir under home; settings and
// extensions hang off it, and it lives on the Phase 4 persist disk.
func codeServerUserDataDir(home string) string {
	return home + "/.local/share/code-server"
}

// userSettingsJSON is the default code-server User/settings.json seeded onto a
// fresh persist disk: an absolute git path (PATH is minimal under the lazy
// supervisor) and the Dark+ theme. JSONC, so a trailing comma would be legal,
// but we keep it strict JSON.
const userSettingsJSON = `{
  "git.path": "/usr/bin/git",
  "workbench.colorTheme": "Dark+"
}
`

// SeedUserSettings writes code-server's User/settings.json with platform
// defaults if it is absent, creating the user-data tree under home and chowning
// each level to uid/gid so the dropped-privilege code-server (decision #5) can
// read and later rewrite it. It never clobbers an existing file, so user edits
// made through the editor survive reboots.
func SeedUserSettings(home string, uid, gid int) error {
	userDir := filepath.Join(codeServerUserDataDir(home), "User")
	settings := filepath.Join(userDir, "settings.json")
	if _, err := os.Stat(settings); err == nil {
		return nil // already present — leave user edits untouched
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(settings, []byte(userSettingsJSON), 0o644); err != nil {
		return err
	}
	// Own the freshly created tree by the run-as user; chowning paths that
	// already belong to it is a harmless no-op.
	for _, p := range []string{
		filepath.Join(home, ".local"),
		filepath.Join(home, ".local", "share"),
		codeServerUserDataDir(home),
		userDir,
		settings,
	} {
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}
