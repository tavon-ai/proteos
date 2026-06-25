// Package testutil provides shared test fixtures. The Postgres helper returns a
// migrated, isolated database: it uses TEST_DATABASE_URL when set (CI runs a
// Postgres service container) and otherwise spins one up via Testcontainers
// (local dev). Either way the schema comes from the real migrations — no mocks.
//
// The Testcontainers Postgres is started ONCE per test binary (a package-level
// singleton) rather than once per test: booting and terminating a container per
// test dominated local test wall-clock. Isolation is unchanged — every Postgres
// call truncates the mutable tables before handing back the pool, exactly as the
// shared CI database (TEST_DATABASE_URL) already does. Tests in a package run
// serially (none call t.Parallel), so a shared database is safe. The container
// is reaped by Testcontainers' Ryuk sidecar when the test process exits, which
// is both correct and far cheaper than an explicit per-test Terminate.
package testutil

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

var (
	// containerOnce guards starting the singleton Testcontainers Postgres.
	containerOnce sync.Once
	containerURL  string
	containerErr  error

	// migrateOnce guards running migrations a single time per process. The schema
	// is identical for every test, so re-running all migrations on each Postgres
	// call is pure overhead.
	migrateOnce sync.Once
	migrateErr  error
)

// DatabaseURL returns a migrated database URL — TEST_DATABASE_URL if set,
// otherwise a per-binary singleton Testcontainers Postgres. Migrations run once
// per process; subsequent calls reuse the already-migrated schema.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		containerOnce.Do(func() {
			containerURL, containerErr = startContainer(context.Background())
		})
		if containerErr != nil {
			t.Fatalf("start postgres container: %v", containerErr)
		}
		url = containerURL
	}

	migrateOnce.Do(func() { migrateErr = store.Migrate(url) })
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	return url
}

// Postgres returns a migrated pool and Queries against a clean database. All
// tables are truncated before the test runs so the shared database (CI or the
// per-binary singleton container) stays isolated across tests.
func Postgres(t *testing.T) (*pgxpool.Pool, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	url := DatabaseURL(t)

	pool, err := store.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Isolate: wipe rows. TRUNCATE users CASCADE clears sessions, github_links,
	// and machines (+ machine_events) via FKs — but hosts and audit_log have no
	// FK to users, so they must be truncated explicitly or their rows leak across
	// tests on the shared Postgres (audit_log deliberately has no user FK so
	// audit outlives its subjects — Phase 5 decision #6). RESTART IDENTITY resets
	// owned sequences (incl. machine_events.id and audit_log.id) so the shared
	// Postgres behaves like a freshly-migrated DB for every test — without it,
	// bigserial ids accumulate across tests on the same DB. The seeded providers
	// table is left intact (no test mutates it).
	if _, err := pool.Exec(ctx, "TRUNCATE users, hosts, audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, store.New(pool)
}

// startContainer boots a Postgres container. It is called at most once per test
// binary (see containerOnce); the container is reaped by Testcontainers' Ryuk
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
