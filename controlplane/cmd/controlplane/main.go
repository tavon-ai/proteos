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

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/config"
	"github.com/tavon/proteos/controlplane/internal/gateway"
	"github.com/tavon/proteos/controlplane/internal/github"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/injector"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
)

func main() {
	migrateFlag := flag.Bool("migrate", false, "apply database migrations on startup, then serve")
	migrateOnlyFlag := flag.Bool("migrate-only", false, "apply database migrations and exit (CI / init container)")
	migrateSecretsFlag := flag.String("migrate-secrets", "", "one-shot: copy a dev FileStore JSON dump into the configured backend, then exit")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if *migrateSecretsFlag != "" {
		if err := migrateSecrets(*migrateSecretsFlag); err != nil {
			slog.Error("migrate-secrets failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*migrateFlag, *migrateOnlyFlag); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// migrateSecrets copies a dev FileStore JSON dump into the configured backend
// (typically openbao) and exits. The backend comes from the usual env config.
func migrateSecrets(dumpPath string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dst, err := openSecrets(cfg)
	if err != nil {
		return err
	}
	n, err := secrets.MigrateFromFile(dumpPath, dst)
	if err != nil {
		return err
	}
	slog.Info("migrated secrets", "paths", n, "backend", cfg.SecretsBackend)
	return nil
}

// openSecrets builds the Store selected by config.
func openSecrets(cfg *config.Config) (secrets.Store, error) {
	return secrets.Open(secrets.BackendConfig{
		Backend:      cfg.SecretsBackend,
		File:         cfg.SecretsFile,
		OpenBaoAddr:  cfg.OpenBaoAddr,
		OpenBaoMount: cfg.OpenBaoMount,
		RoleID:       cfg.OpenBaoRoleID,
		SecretIDFile: cfg.OpenBaoSecretIDFile,
	})
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

	sec, err := openSecrets(cfg)
	if err != nil {
		return err
	}
	slog.Info("secrets backend", "backend", cfg.SecretsBackend)

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
	nodes, err := nodeclient.NewPinned(cfg.NodeAgentURL, cfg.AgentToken, cfg.NodeCAFile)
	if err != nil {
		return err
	}
	broker := machine.NewBroker()
	machineSvc := machine.NewService(pool, nodes, broker, sec, host.ID, machine.Spec{
		Vcpus:     cfg.MachineVcpus,
		MemMiB:    cfg.MachineMemMiB,
		DiskMiB:   cfg.MachineDiskMiB,
		KernelRef: cfg.KernelRef,
		RootfsRef: cfg.RootfsRef,
	})
	poller := machine.NewPoller(pool, nodes, broker)

	// Phase 5: the secret injector pushes provider keys into the guest. The
	// poller fires it on every * → running transition (start and resume); the
	// agent gateway route fires it again, idempotently, before a launch.
	providerRegistry := providers.NewRegistry(q)
	auditRec := audit.NewRecorder(q)
	inject := injector.New(nodes, providerRegistry, sec, auditRec)
	poller.SetOnRunning(inject.InjectAsync)

	// Phase 6: align the registry's enabled flags with the providers actually
	// baked into the rootfs (PROTEOS_PROVIDERS_ENABLED), so the UI never offers a
	// provider whose CLI is missing from the image. Absent ⇒ leave the seeds.
	if cfg.ProvidersEnabledSet {
		unknown, err := providerRegistry.SetEnabled(ctx, cfg.ProvidersEnabled)
		if err != nil {
			slog.Error("reconcile provider enablement", "err", err)
			os.Exit(1)
		}
		if len(unknown) > 0 {
			slog.Warn("PROTEOS_PROVIDERS_ENABLED lists unknown providers (ignored)", "keys", unknown)
		}
		slog.Info("provider enablement reconciled", "enabled", cfg.ProvidersEnabled)
	}

	go poller.Run(ctx)

	// Phase 3: the terminal gateway proxies the browser WS to each machine's
	// guest agent through the node-agent tunnel. The conn registry is wired as
	// the session revocation listener so logout/revoke closes live terminals.
	gwRegistry := gateway.NewRegistry()
	sessions.SetRevocationListener(gwRegistry)
	gw := gateway.NewProxy(cfg.AllowedWSOrigins, nodes, gwRegistry)

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
		Sessions:  sessions,
		Auth:      authHandler,
		Machines:  machineSvc,
		Broker:    broker,
		Queries:   q,
		Gateway:   gw,
		Providers: providerRegistry,
		Secrets:   sec,
		Audit:     auditRec,
		Injector:  inject,
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("control plane listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
	return httpServer.ListenAndServe()
}
