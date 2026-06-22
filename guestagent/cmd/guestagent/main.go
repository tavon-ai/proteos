// Command guestagent is the PTY broker that runs inside a ProteOS microVM (and,
// in dev, as a child of the node-agent). It listens on a private transport
// (vsock in production, a unix socket in dev) and serves the terminal
// WebSocket protocol: each attach gets an interactive shell whose session
// outlives the connection. It imports nothing from the other modules — it ships
// as a static Linux binary baked into the rootfs (Phase 3 decision #2).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/config"
	"github.com/tavon/proteos/guestagent/internal/ctlchan"
	"github.com/tavon/proteos/guestagent/internal/listen"
	"github.com/tavon/proteos/guestagent/internal/localsock"
	"github.com/tavon/proteos/guestagent/internal/persist"
	"github.com/tavon/proteos/guestagent/internal/previewfwd"
	"github.com/tavon/proteos/guestagent/internal/runas"
	"github.com/tavon/proteos/guestagent/internal/secrets"
	"github.com/tavon/proteos/guestagent/internal/server"
	"github.com/tavon/proteos/guestagent/internal/term"
	"github.com/tavon/proteos/guestagent/internal/webfwd"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Subcommand dispatch: the same static binary is the credential helper git
	// invokes (Phase 7 decision #5). `git-credential` speaks the credential
	// protocol on stdio and exits; everything else runs the long-lived agent.
	if len(os.Args) > 1 && os.Args[1] == "git-credential" {
		if err := gitCredential(os.Args[2:]); err != nil {
			os.Exit(1)
		}
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ln, err := listen.Listen(cfg.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()

	// Resolve the unprivileged user that PTY sessions run as (Phase 8). The agent
	// stays root; sessions drop to this user. Falls back to root if it is absent
	// (older rootfs / dev on a Mac), preserving the legacy all-root behavior.
	id := runas.Resolve(cfg.RunAsUser)
	slog.Info("guest: session run-as identity", "user", id.Name, "uid", id.UID, "home", id.Home, "root", id.IsRoot)

	// Persistence runs first, before any shell spawns, so $HOME and the
	// workspace are on the disk (decision #7). A degraded handle (no disk) still
	// serves terminals — ephemerally.
	p, err := persist.Setup(persist.Config{
		Dir:       cfg.PersistDir,
		Device:    cfg.PersistDevice,
		Version:   version,
		RunAsHome: id.Home,
		RunAsUID:  id.UID,
		RunAsGID:  id.GID,
		RunAsUser: id.Name,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	// Sessions drop to the unprivileged user and start in its $HOME (which it can
	// write); the credential is nil for the root identity.
	sessDir := ""
	if !id.IsRoot {
		sessDir = id.Home
	}
	mgr := term.NewManager(term.Config{
		Shell:         cfg.Shell,
		ScrollbackKiB: cfg.ScrollbackKiB,
		Env:           p.ShellEnv(),
		Credential:    id.Credential(),
		Dir:           sessDir,
	})
	defer mgr.Shutdown()

	sec, err := secrets.New(cfg.EnvDir, id)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Phase 7: the control channel (CP-dialed GET /control) and the local
	// credential-helper socket. The manager holds the single live channel and
	// resolves git credentials for the helper over it.
	control := ctlchan.New(p.ShellEnv(), id, p, sec)
	helperSock := localsock.New(guestwire.AgentSockPath, control, id)
	go func() {
		if err := helperSock.Serve(ctx); err != nil {
			slog.Error("credential helper socket", "err", err)
		}
	}()

	// Phase 8: the code-server web forward (decision #4). When a web listener is
	// configured it serves on a second private port (vsock:1025 / a dev unix
	// socket); the node-agent tunnel reaches it on agentapi.GuestWebPort. With a
	// code-server binary configured it lazily starts + supervises code-server,
	// else it forwards to an already-running backend (dev/e2e stub).
	if cfg.WebListen != "" {
		webLn, err := listen.Listen(cfg.WebListen)
		if err != nil {
			return err
		}
		defer webLn.Close()

		args := webfwd.DefaultCodeServerArgs(cfg.WebBackend, id.Home, "/workspace")
		if cfg.CodeServerArgs != "" {
			args = strings.Fields(cfg.CodeServerArgs)
		}
		// Seed default User/settings.json on the persist disk before the first
		// lazy start. Non-fatal: a missing settings file only costs the defaults.
		if cfg.CodeServerBin != "" {
			if err := webfwd.SeedUserSettings(id.Home, id.UID, id.GID); err != nil {
				slog.Warn("webfwd: seed code-server settings", "err", err)
			}
		}
		sup := webfwd.NewSupervisor(webfwd.SupervisorConfig{
			Bin:  cfg.CodeServerBin,
			Args: args,
			Env:  p.ShellEnv(),
			Dir:  "/workspace",
			Cred: id.Credential(),
			Addr: cfg.WebBackend,
		})
		fwd := webfwd.New(webLn, cfg.WebBackend, sup)
		go func() {
			slog.Info("guest web forward listening", "listen", cfg.WebListen, "backend", cfg.WebBackend, "supervised", cfg.CodeServerBin != "")
			if err := fwd.Serve(ctx); err != nil {
				slog.Error("web forward serve", "err", err)
			}
		}()
		defer sup.Shutdown()
	}

	// PP1: the generic port-preview forward. When a preview listener is configured
	// it serves on a third private port (vsock:1026 / a dev unix socket) that the
	// node-agent tunnel reaches on agentapi.GuestPreviewPort. It bridges each
	// connection to the loopback port the node-agent names in a one-line preamble;
	// there is no backend config and no supervisor (the user's own process is the
	// backend), so a missing listener just disables the feature.
	if cfg.PreviewListen != "" {
		previewLn, err := listen.Listen(cfg.PreviewListen)
		if err != nil {
			return err
		}
		defer previewLn.Close()

		fwd := previewfwd.New(previewLn)
		go func() {
			slog.Info("guest preview forward listening", "listen", cfg.PreviewListen)
			if err := fwd.Serve(ctx); err != nil {
				slog.Error("preview forward serve", "err", err)
			}
		}()
	}

	srv := server.New(mgr, p, sec, control)
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("guest agent listening", "version", version, "listen", cfg.Listen, "shell", cfg.Shell)
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http serve", "err", err)
		}
	}()
	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
