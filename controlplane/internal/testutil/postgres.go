// Package testutil provides shared test fixtures. The Postgres helper returns a
// migrated, isolated database: it uses TEST_DATABASE_URL when set (CI runs a
// Postgres service container) and otherwise spins one up via Testcontainers
// (local dev). Either way the schema comes from the real migrations — no mocks.
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// DatabaseURL returns a migrated database URL — TEST_DATABASE_URL if set,
// otherwise a fresh Testcontainers Postgres terminated on test cleanup.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = startContainer(t, context.Background())
	}
	if err := store.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return url
}

// Postgres returns a migrated pool and Queries against a clean database. All
// tables are truncated before the test runs so shared (CI) databases stay
// isolated across tests.
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
	// tests on the shared CI Postgres (audit_log deliberately has no user FK so
	// audit outlives its subjects — Phase 5 decision #6). RESTART IDENTITY resets
	// owned sequences (incl. machine_events.id and audit_log.id) so the shared CI
	// Postgres behaves like a freshly-migrated DB for every test — without it,
	// bigserial ids accumulate across tests on the same DB. The seeded providers
	// table is left intact (no test mutates it).
	if _, err := pool.Exec(ctx, "TRUNCATE users, hosts, audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, store.New(pool)
}

func startContainer(t *testing.T, ctx context.Context) string {
	t.Helper()
	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("proteos"),
		tcpostgres.WithUsername("proteos"),
		tcpostgres.WithPassword("proteos"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}
