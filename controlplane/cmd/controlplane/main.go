// Command controlplane is the ProteOS control-plane HTTP server: GitHub auth,
// sessions, and the (Phase 1 stubbed) machine API.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/config"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
)

func main() {
	migrateFlag := flag.Bool("migrate", false, "apply database migrations on startup, then serve")
	migrateOnlyFlag := flag.Bool("migrate-only", false, "apply database migrations and exit (CI / init container)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(*migrateFlag, *migrateOnlyFlag); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(migrate, migrateOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if migrate || migrateOnly {
		slog.Info("applying migrations")
		if err := store.Migrate(cfg.DatabaseURL); err != nil {
			return err
		}
		if migrateOnly {
			slog.Info("migrations applied; exiting (migrate-only)")
			return nil
		}
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := store.New(pool)

	sec, err := secrets.NewFileStore(cfg.SecretsFile)
	if err != nil {
		return err
	}

	sessions := session.NewManager(q, cfg.SessionTTL)

	// Phase 2: seed the single host this control plane manages, wire the
	// node-agent client, and build the machine lifecycle (service + poller +
	// SSE broker). The poller runs for the life of the process.
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{
		Name:     cfg.HostName,
		AgentUrl: cfg.NodeAgentURL,
	})
	if err != nil {
		return err
	}
	if cfg.AgentToken == "" {
		slog.Warn("PROTEOS_AGENT_TOKEN is empty; node-agent calls will be unauthenticated and will fail")
	}
	nodes := nodeclient.New(cfg.NodeAgentURL, cfg.AgentToken)
	broker := machine.NewBroker()
	machineSvc := machine.NewService(pool, nodes, broker, host.ID, machine.Spec{
		Vcpus:     cfg.MachineVcpus,
		MemMiB:    cfg.MachineMemMiB,
		KernelRef: cfg.KernelRef,
		RootfsRef: cfg.RootfsRef,
	})
	poller := machine.NewPoller(pool, nodes, broker)
	go poller.Run(ctx)

	var authHandler *auth.Handler
	if err := cfg.ValidateOAuth(); err != nil {
		// Without OAuth config the server still serves /healthz and the
		// authenticated API (which simply 401s) — useful for the 1.0 smoke
		// path and CI before secrets are wired. Warn loudly.
		slog.Warn("GitHub OAuth not configured; login routes disabled", "reason", err.Error())
	} else {
		ghClient := github.NewClient(github.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
		})
		authHandler = auth.NewHandler(auth.Config{
			BaseURL:             cfg.BaseURL,
			StateKey:            cfg.StateSigningKey,
			CookieSecure:        cfg.CookieSecure,
			SessionTTL:          cfg.SessionTTL,
			AllowedGitHubLogins: cfg.AllowedGitHubLogins,
		}, ghClient, sessions, q, sec)
	}

	srv := &httpapi.Server{
		Sessions: sessions,
		Auth:     authHandler,
		Machines: machineSvc,
		Broker:   broker,
		Queries:  q,
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("control plane listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
	return httpServer.ListenAndServe()
}
