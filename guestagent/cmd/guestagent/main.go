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
	"syscall"
	"time"

	"github.com/tavon/proteos/guestagent/internal/config"
	"github.com/tavon/proteos/guestagent/internal/listen"
	"github.com/tavon/proteos/guestagent/internal/persist"
	"github.com/tavon/proteos/guestagent/internal/server"
	"github.com/tavon/proteos/guestagent/internal/term"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
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

	// Persistence runs first, before any shell spawns, so $HOME and the
	// workspace are on the disk (decision #7). A degraded handle (no disk) still
	// serves terminals — ephemerally.
	p, err := persist.Setup(persist.Config{
		Dir:     cfg.PersistDir,
		Device:  cfg.PersistDevice,
		Version: version,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	mgr := term.NewManager(term.Config{
		Shell:         cfg.Shell,
		ScrollbackKiB: cfg.ScrollbackKiB,
		Env:           p.ShellEnv(),
	})
	defer mgr.Shutdown()

	srv := server.New(mgr, p)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
