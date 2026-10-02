package postgres_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
)

// These tests need a real PostgreSQL server, because the things they verify —
// transaction handling, advisory locks, extension availability — cannot be
// faked. They are skipped when TEST_DATABASE_URL is unset so that `go test ./...`
// stays green on a machine with no database.
//
// Run them with a throwaway server:
//
//	docker run -d --rm --name cw-pg-test -p 55432:5432 \
//	  -e POSTGRES_PASSWORD=postgres -e POSTGRES_USER=postgres \
//	  -e POSTGRES_DB=classwatch_test postgres:18-alpine
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/classwatch_test?sslmode=disable' \
//	  go test ./internal/infrastructure/postgres/ -v

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	// Honour the repository .env during development: a developer who ran
	// `make init` should not have to export TEST_DATABASE_URL by hand.
	if _, err := config.LoadDotEnv(); err != nil {
		t.Fatalf("load .env: %v", err)
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	return dsn
}

func testConfig(dsn string) *config.Config {
	return &config.Config{
		AppEnv:              config.EnvTest,
		DatabaseURL:         dsn,
		DBMaxConns:          5,
		DBMinConns:          1,
		DBMaxConnLifetime:   time.Minute,
		DBHealthCheckPeriod: 30 * time.Second,
	}
}

// connect opens a pool for the test and closes it on cleanup.
func connect(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := testDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := postgres.Connect(ctx, testConfig(dsn))
	if err != nil {
		t.Fatalf("postgres.Connect() failed: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping() failed: %v", err)
	}
	return db
}

// TestMigrationUpIsIdempotentAndRecorded is the Phase 0 acceptance test for the
// migration pipeline: it must apply exactly once, record what it did, and be safe
// to run again from a boot script or a deploy job.
func TestMigrationUpIsIdempotentAndRecorded(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	pool := db.Pool()

	// Start from a known state. Dropping the bookkeeping table is safe here
	// because the Phase 0 migration is pure idempotent DDL (CREATE EXTENSION IF
	// NOT EXISTS): re-running it cannot destroy data. Later phases must NOT copy
	// this shortcut — their migrations will hold real tables.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
		t.Fatalf("reset schema_migrations: %v", err)
	}

	first, err := migrate.Up(ctx, pool)
	if err != nil {
		t.Fatalf("first migrate.Up() failed: %v", err)
	}
	if len(first) != 1 || first[0].Version != 1 {
		t.Fatalf("first Up() applied %+v, want exactly version 1", first)
	}
	if first[0].Filename != "0001_bootstrap.sql" {
		t.Errorf("applied file = %q, want 0001_bootstrap.sql", first[0].Filename)
	}

	// Second run must be a no-op: this is the property that lets the API boot with
	// DB_AUTO_MIGRATE=true without racing itself.
	second, err := migrate.Up(ctx, pool)
	if err != nil {
		t.Fatalf("second migrate.Up() failed: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second Up() applied %+v, want nothing (migrations are not idempotent)", second)
	}

	var count int
	var name string
	var applied bool
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(min(name), ''), coalesce(bool_and(applied_at <= now()), false)
		   FROM schema_migrations WHERE version = 1`).Scan(&count, &name, &applied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations rows for version 1 = %d, want 1", count)
	}
	if name != "bootstrap" {
		t.Errorf("recorded name = %q, want %q", name, "bootstrap")
	}
	if !applied {
		t.Error("applied_at is not set in the past")
	}

	// The bootstrap migration's whole purpose is these two extensions.
	for _, ext := range []string{"pgcrypto", "citext"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext).Scan(&exists); err != nil {
			t.Fatalf("query pg_extension: %v", err)
		}
		if !exists {
			t.Errorf("extension %q is not installed; 0001_bootstrap.sql did not run", ext)
		}
	}

	// Status must report the applied migration and must not invent an "unknown"
	// row for a file the binary does not know about.
	entries, err := migrate.Status(ctx, pool)
	if err != nil {
		t.Fatalf("migrate.Status() failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Status() returned %d entries, want 1: %+v", len(entries), entries)
	}
	if !entries[0].Applied || entries[0].Unknown || entries[0].AppliedAt == nil {
		t.Errorf("Status()[0] = %+v, want applied and known with a timestamp", entries[0])
	}
}

// TestMigrationUpIsConcurrencySafe proves the advisory lock does its job: several
// API replicas booting at the same moment must not double-apply a migration or
// fail each other with a duplicate-key error.
func TestMigrationUpIsConcurrencySafe(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	pool := db.Pool()

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
		t.Fatalf("reset schema_migrations: %v", err)
	}

	const runners = 3
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		applied  int
		failures []error
	)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := migrate.Up(ctx, pool)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			applied += len(done)
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("concurrent Up() failed: %v", failures)
	}
	if applied != 1 {
		t.Errorf("concurrent Up() applied %d migrations in total, want exactly 1", applied)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations has %d rows, want 1", rows)
	}
}

// TestConnectRejectsUnreachableServer checks the failure path main relies on:
// an unreachable database must produce an error at connect time, not a pool that
// fails later on the first request.
func TestConnectRejectsUnreachableServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	// Port 1 is reserved and never listening.
	cfg := testConfig("postgres://postgres:postgres@127.0.0.1:1/classwatch_test?sslmode=disable")
	cfg.DBMaxConns = 1
	cfg.DBMinConns = 0

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := postgres.Connect(ctx, cfg)
	if err == nil {
		db.Close()
		t.Fatal("Connect() succeeded against an unreachable server, want error")
	}
}

// TestConnectErrorDoesNotLeakPassword is a security regression test: connect
// failures are logged and pasted into tickets, so the DSN password must never
// appear in them.
func TestConnectErrorDoesNotLeakPassword(t *testing.T) {
	const password = "sup3r-s3cret-db-password"

	cfg := testConfig("postgres://postgres:" + password + "@127.0.0.1:1/classwatch_test?sslmode=disable")
	cfg.DBMinConns = 0
	cfg.DBMaxConns = 1

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := postgres.Connect(ctx, cfg)
	if err == nil {
		db.Close()
		t.Fatal("Connect() succeeded against an unreachable server, want error")
	}
	if got := err.Error(); strings.Contains(got, password) {
		t.Errorf("database password leaked into connect error: %s", got)
	}
}
