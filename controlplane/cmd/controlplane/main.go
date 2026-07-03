// Command controlplane is the ProteOS control-plane HTTP server: GitHub auth,
// sessions, and the (Phase 1 stubbed) machine API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/config"
	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/injector"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/nodeclient"
	"github.com/tavon-ai/proteos/controlplane/internal/profile"
	"github.com/tavon-ai/proteos/controlplane/internal/providers"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
	"github.com/tavon-ai/proteos/controlplane/internal/token"
)

func main() {
	migrateFlag := flag.Bool("migrate", false, "apply database migrations on startup, then serve")
	migrateOnlyFlag := flag.Bool("migrate-only", false, "apply database migrations and exit (CI / init container)")
	migrateSecretsFlag := flag.String("migrate-secrets", "", "one-shot: copy a dev FileStore JSON dump into the configured backend, then exit")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))

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

// logLevel reads PROTEOS_LOG_LEVEL ("debug", "info", "warn", "error";
// case-insensitive) and returns the corresponding slog.Level. It defaults to
// info when the var is unset or unrecognized.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROTEOS_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
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
		Backend:       cfg.SecretsBackend,
		File:          cfg.SecretsFile,
		OpenBaoAddr:   cfg.OpenBaoAddr,
		OpenBaoMount:  cfg.OpenBaoMount,
		OpenBaoPrefix: cfg.OpenBaoPrefix,
		RoleID:        cfg.OpenBaoRoleID,
		SecretIDFile:  cfg.OpenBaoSecretIDFile,
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

	// Cancel the root context on SIGTERM or SIGINT. This propagates to all
	// background goroutines (poller, guestCtl) so they stop gracefully when the
	// signal fires.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

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

	// Fail fast if the secrets backend cannot write/read/delete a machine secret.
	// This catches the classic OpenBao misconfiguration — a cp-base policy whose
	// granted paths don't match the configured mount/prefix — at boot, with a
	// clear message, instead of as an opaque 500 on the first machine create.
	if err := secrets.SelfCheck(sec); err != nil {
		if secrets.IsPermissionDenied(err) {
			slog.Error("secrets self-check denied: the backend policy is missing machine write capability; "+
				"verify the OpenBao cp-base policy grants create/update on the configured mount+prefix",
				"err", err, "mount", cfg.OpenBaoMount, "prefix", cfg.OpenBaoPrefix)
			return fmt.Errorf("secrets backend misconfigured: %w", err)
		}
		return fmt.Errorf("secrets backend self-check failed: %w", err)
	}
	slog.Info("secrets self-check passed")

	sessions := session.NewManager(q, cfg.SessionTTL)
	// AC1: personal access tokens back bearer auth for the CLI and the
	// /api/tokens management routes (browser settings page mints them).
	patManager := token.NewManager(q)

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

	// AT2: the in-process fan-out for headless agent-task event streams. The
	// guest control channel publishes normalized agent.event frames here; the task
	// SSE endpoint subscribes per task. Bounded + ephemeral (DB holds the outcome).
	taskHub := taskevents.New(taskevents.DefaultBufferSize, taskevents.DefaultRetention)

	// Machine-template catalog (static config): load from PROTEOS_TEMPLATES_FILE
	// when set, else synthesize a single "base" template from the global image
	// refs + resource defaults. Either way the catalog is the source of truth for
	// create-time refs/resources; a bad catalog fails startup loudly.
	defaults := machine.Resources{Vcpus: cfg.MachineVcpus, MemMiB: cfg.MachineMemMiB, DiskMiB: cfg.MachineDiskMiB}
	var catalog machine.Catalog
	if cfg.TemplatesFile != "" {
		catalog, err = machine.LoadCatalogFile(cfg.TemplatesFile, cfg.KernelRef)
	} else {
		catalog, err = machine.SingleTemplateCatalog(cfg.RootfsRef, cfg.KernelRef, defaults)
	}
	if err != nil {
		return fmt.Errorf("machine templates: %w", err)
	}
	// Resource caps for create-time overrides; every template's own defaults must
	// fall within them, else the catalog is misconfigured — fail startup loudly.
	limits := machine.NewResourceLimits(cfg.MaxVcpus, cfg.MaxMemMiB, cfg.MaxDiskMiB)
	for _, t := range catalog.Templates() {
		if err := limits.Validate(t.Defaults); err != nil {
			return fmt.Errorf("template %q defaults out of caps: %w", t.ID, err)
		}
	}
	tmplSource := "synthesized-base"
	if cfg.TemplatesFile != "" {
		tmplSource = cfg.TemplatesFile
	}
	slog.Info("machine templates loaded", "count", len(catalog.Templates()), "source", tmplSource)

	machineSvc := machine.NewService(pool, nodes, broker, sec, host.ID, machine.Spec{
		Vcpus:      cfg.MachineVcpus,
		MemMiB:     cfg.MachineMemMiB,
		DiskMiB:    cfg.MachineDiskMiB,
		KernelRef:  cfg.KernelRef,
		RootfsRef:  cfg.RootfsRef,
		MaxPerUser: cfg.MachineMaxPerUser,
		Catalog:    catalog,
		Limits:     limits,
	})
	poller := machine.NewPoller(pool, nodes, broker)

	// Phase 5: the secret injector pushes provider keys into the guest. The
	// poller fires it on every * → running transition (start and resume); the
	// agent gateway route fires it again, idempotently, before a launch.
	providerRegistry := providers.NewRegistry(q)
	auditRec := audit.NewRecorder(q)
	// Portable user profile (Phase 1): user-scoped items the injector merges into
	// the guest alongside provider secrets (e.g. the Claude subscription token).
	profileStore := profile.NewStore(q, sec, auditRec)
	inject := injector.New(nodes, providerRegistry, sec, auditRec, profileStore)
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

	// Phase 8: the per-machine code-server editor origin (m-<uuid>.<domain>).
	// NewMachineWeb returns nil when PROTEOS_MACHINE_DOMAIN is unset, disabling
	// host-first routing and the web-session route. It shares the terminal's conn
	// registry so a logout closes live editor sockets too.
	sessRes, machRes := httpapi.MachineWebResolvers(sessions, machineSvc)
	machineWeb := gateway.NewMachineWeb(gateway.MachineWebConfig{
		Domain:         cfg.MachineDomain,
		SigningKey:     cfg.StateSigningKey,
		CookieSecure:   cfg.CookieSecure,
		FrameAncestors: cfg.AllowedWSOrigins,
		Guests:         nodes,
		Registry:       gwRegistry,
		Sessions:       sessRes,
		Machines:       machRes,
		PreviewPortMin: cfg.PreviewPortMin,
		PreviewPortMax: cfg.PreviewPortMax,
	})
	if machineWeb != nil {
		slog.Info("machine-web editor enabled", "domain", cfg.MachineDomain)
	}

	var authHandler *auth.Handler
	var ghClient *github.Client
	var tokenSrc *github.TokenSource
	var guestCtl *guestctl.Manager
	if err := cfg.ValidateOAuth(); err != nil {
		// Without OAuth config the server still serves /healthz and the
		// authenticated API (which simply 401s) — useful for the 1.0 smoke
		// path and CI before secrets are wired. Warn loudly.
		slog.Warn("GitHub OAuth not configured; login + git routes disabled", "reason", err.Error())
	} else {
		ghClient = github.NewClient(github.Config{
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

		// Phase 7: per-user token lifecycle + the persistent guest control channel
		// manager. The manager watches machine state (via the broker) and keeps one
		// control channel per running machine, serving on-demand git.credential
		// requests through its single authorization choke point.
		tokenSrc = github.NewTokenSource(ghClient, q, sec)
		guestCtl = guestctl.New(nodes, broker, q, tokenSrc, auditRec, taskHub, cfg.GitHost)
		go guestCtl.Run(ctx)
	}

	srv := &httpapi.Server{
		Sessions:   sessions,
		PATs:       patManager,
		Auth:       authHandler,
		Machines:   machineSvc,
		Broker:     broker,
		Queries:    q,
		Gateway:    gw,
		Guests:     nodes,
		MachineWeb: machineWeb,
		Providers:  providerRegistry,
		Secrets:    sec,
		Audit:      auditRec,
		Profile:    profileStore,
		Injector:   inject,
	}
	if guestCtl != nil {
		srv.GitHub = ghClient
		srv.Tokens = tokenSrc
		srv.GitChannel = guestCtl
		srv.GitHost = cfg.GitHost
		srv.GitHubAppSlug = cfg.GitHubAppSlug
		// Phase 9: the same control-channel manager backs projects.list + kv.*.
		srv.Projects = guestCtl
		// GR1: and the worktree-review git status/diff surface.
		srv.GitWorktree = guestCtl
		// AT1: and the headless agent-run dispatch surface.
		srv.TaskChannel = guestCtl
		// Phase 4: re-apply a portable git-identity change to running machines.
		srv.GitConfigurer = guestCtl
	}
	// AT2: the live agent-task event stream (independent of the OAuth/git stack).
	srv.TaskEvents = taskHub

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    8 << 10, // 8 KiB; default is 1 MiB, which allows oversized header attacks
	}
	slog.Info("control plane listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)

	// Start the HTTP server in a goroutine so we can orchestrate shutdown.
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// Block until a signal fires or the server exits with a fatal error.
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		// Signal received — begin graceful shutdown.
	}

	// Release the signal handler so a second signal kills the process immediately.
	stop()

	slog.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)

	// Notify active SSE clients so they can display a reconnect banner before
	// the connection is closed. This must happen before Shutdown() so that the
	// SSE handlers have a chance to write their final event.
	broker.Shutdown()
	taskHub.Shutdown()

	// Drain in-flight HTTP requests within the configured timeout. New requests
	// are rejected immediately; existing connections are closed once idle.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown", "err", err)
		return err
	}

	slog.Info("control plane stopped cleanly")
	return nil
}
