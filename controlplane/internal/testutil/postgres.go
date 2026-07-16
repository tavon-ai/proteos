// Package testutil provides shared test fixtures. The Postgres helper returns a
// migrated, isolated database: it uses TEST_DATABASE_URL when set (CI runs a
// Postgres service container) and otherwise spins one up via Testcontainers
// (local dev). Either way the schema comes from the real migrations — no mocks.
//
// Isolation is per-test databases, not truncation: once per process a template
// database is created and migrated, and every DatabaseURL/Postgres call clones
// it with CREATE DATABASE ... TEMPLATE (fast — a file-level copy) and drops the
// clone in t.Cleanup. Clones are fully independent, so tests may call
// t.Parallel() freely; nothing is shared between them. Names are pid-scoped
// (proteos_tmpl_<pid>, proteos_test_<pid>_<n>) so concurrent test binaries
// sharing one server (go test ./... against TEST_DATABASE_URL) never collide.
// The per-process template is not dropped (there is no process-exit hook); on
// the ephemeral CI service container and the Testcontainers singleton that is
// free, and DROP DATABASE IF EXISTS before CREATE handles pid reuse on a
// long-lived dev server.
//
// The Testcontainers Postgres is started ONCE per test binary (a package-level
// singleton) rather than once per test: booting and terminating a container per
// test dominated local test wall-clock. The container is reaped by
// Testcontainers' Ryuk sidecar when the test process exits.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

var (
	// serverOnce guards resolving the admin URL: TEST_DATABASE_URL if set,
	// otherwise the singleton Testcontainers Postgres.
	serverOnce sync.Once
	serverURL  string
	serverErr  error

	// templateOnce guards creating + migrating the per-process template database.
	// The schema is identical for every test, so each clone inherits it for the
	// cost of a file copy instead of a full migration run.
	templateOnce sync.Once
	templateErr  error

	// createMu serializes CREATE DATABASE ... TEMPLATE calls: Postgres refuses to
	// clone a template that another backend is concurrently cloning from.
	createMu sync.Mutex

	// dbCounter distinguishes the per-test clones within this process.
	dbCounter atomic.Int64
)

func templateName() string { return fmt.Sprintf("proteos_tmpl_%d", os.Getpid()) }

// adminURL returns a URL for administrative statements (CREATE/DROP DATABASE),
// pointing at the server's base database.
func adminURL(t *testing.T) string {
	t.Helper()
	serverOnce.Do(func() {
		serverURL = os.Getenv("TEST_DATABASE_URL")
		if serverURL == "" {
			serverURL, serverErr = startContainer(context.Background())
		}
	})
	if serverErr != nil {
		t.Fatalf("start postgres container: %v", serverErr)
	}
	return serverURL
}

// withDatabase returns base with its database (path) component replaced.
func withDatabase(t *testing.T, base, dbname string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse database url %q: %v", base, err)
	}
	u.Path = "/" + dbname
	return u.String()
}

// admExec runs a single administrative statement against the base database.
func admExec(ctx context.Context, adminURL, stmt string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	return nil
}

// DatabaseURL returns the URL of a freshly cloned, fully migrated database that
// is private to the calling test and dropped in t.Cleanup. Safe under
// t.Parallel().
func DatabaseURL(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin := adminURL(t)

	templateOnce.Do(func() {
		tmpl := templateName()
		// A previous process with this pid may have crashed mid-migration on a
		// long-lived server; start from a clean slate.
		if templateErr = admExec(ctx, admin, "DROP DATABASE IF EXISTS "+tmpl); templateErr != nil {
			return
		}
		if templateErr = admExec(ctx, admin, "CREATE DATABASE "+tmpl); templateErr != nil {
			return
		}
		templateErr = store.Migrate(withDatabase(t, admin, tmpl))
	})
	if templateErr != nil {
		t.Fatalf("create template database: %v", templateErr)
	}

	name := fmt.Sprintf("proteos_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	createMu.Lock()
	err := admExec(ctx, admin, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateName()))
	createMu.Unlock()
	if err != nil {
		t.Fatalf("clone test database: %v", err)
	}
	t.Cleanup(func() {
		// FORCE kicks out stragglers (e.g. a spawned control-plane process that
		// is torn down by a later cleanup); best-effort by design.
		_ = admExec(context.Background(), admin, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return withDatabase(t, admin, name)
}

// Postgres returns a migrated pool and Queries against a database private to
// the calling test (see DatabaseURL). Safe under t.Parallel().
func Postgres(t *testing.T) (*pgxpool.Pool, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	dbURL := DatabaseURL(t)

	// Cap per-test pools well below Postgres' max_connections (default 100):
	// under t.Parallel() many pools are open at once, and pgxpool's default max
	// (GOMAXPROCS) multiplied by parallel tests can exhaust the server.
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, store.New(pool)
}

// startContainer boots a Postgres container. It is called at most once per test
// binary (see serverOnce); the container is reaped by Testcontainers' Ryuk
// sidecar when the test process exits, so there is no explicit Terminate.
func startContainer(ctx context.Context) (string, error) {
	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("proteos"),
		tcpostgres.WithUsername("proteos"),
		tcpostgres.WithPassword("proteos"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", err
	}
	return container.ConnectionString(ctx, "sslmode=disable")
}
