// Command nodeagent is the privileged host daemon that boots and supervises
// microVMs behind a Driver interface, exposing the agentapi wire contract to
// the control plane over an authenticated (bearer-token) HTTP/JSON API.
//
// It is a separate Go module from the control plane: it carries the
// Firecracker/netlink dependencies and a different (root-on-host) deploy story,
// and it never imports the control plane. The only shared code is the wire
// contract in github.com/tavon/proteos/nodeagent/api.
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

	"github.com/tavon/proteos/nodeagent/internal/config"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/driver/dev"
	"github.com/tavon/proteos/nodeagent/internal/httpapi"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

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

	store, err := state.NewStore(cfg.DataDir, cfg.Subnet)
	if err != nil {
		return err
	}

	drv, err := buildDriver(cfg, store)
	if err != nil {
		return err
	}

	// Re-attach to (or reap) machines that survived/died across an agent
	// restart, so on-disk state matches reality before we serve requests.
	if err := drv.Reattach(context.Background()); err != nil {
		slog.Warn("reattach encountered errors", "err", err)
	}

	srv := httpapi.New(cfg.Token, drv)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("node-agent listening", "addr", cfg.Addr, "driver", cfg.Driver, "data_dir", cfg.DataDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM. We deliberately leave running VMs
	// alone — they are tracked on disk and re-attached on the next start.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// buildDriver selects the driver implementation from config. The firecracker
// driver is linux-only and lives behind a build tag; on platforms/builds where
// it is absent, requesting it is a startup error rather than a silent fallback.
func buildDriver(cfg *config.Config, store *state.Store) (driver.Driver, error) {
	switch cfg.Driver {
	case "dev":
		return dev.New(store, cfg.BootDelay, cfg.StubPath, cfg.GuestAgentBin), nil
	case "firecracker":
		return newFirecrackerDriver(cfg, store)
	default:
		return nil, errors.New("unknown PROTEOS_AGENT_DRIVER: " + cfg.Driver)
	}
}
