package postgres_test

import (
	"context"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/migrations"
)

// These tests need a real PostgreSQL server, because the things they verify —
// transaction handling, advisory locks, extension availability, the exact DDL of
// a migration — cannot be faked. They are skipped when TEST_DATABASE_URL is
// unset so that `go test ./...` stays green on a machine with no database.
//
// Run them with a throwaway server:
//
//	docker run -d --rm --name cw-pg-test -p 55432:5432 \
//	  -e POSTGRES_PASSWORD=postgres -e POSTGRES_USER=postgres \
//	  -e POSTGRES_DB=classwatch_test postgres:18-alpine
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/classwatch_test?sslmode=disable' \
//	  go test ./internal/infrastructure/postgres/ -v
//
// The migration tests run against a THROWAWAY DATABASE created for the test
// (dbtest.FreshPool/EmptyPool) rather than against the shared one: "the first run
// applies everything" and "concurrent runners apply it once" can only be observed
// on an empty database, and dropping the shared schema instead would break every
// other package that runs in parallel with this one.

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

// embeddedMigrationCount is how many .sql files the binary carries. Every
// assertion below is written in terms of it so adding a migration cannot make a
// test silently wrong.
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			count++
		}
	}
	if count == 0 {
		t.Fatal("no embedded migrations found")
	}
	return count
}

// TestMigrationUpAppliesEverythingAndIsIdempotent is the acceptance test for the
// migration pipeline: on an empty database every embedded migration must be
// applied, recorded, and be safe to run again from a boot script or a deploy job.
func TestMigrationUpAppliesEverythingAndIsIdempotent(t *testing.T) {
	// FreshPool has already run migrate.Up once, from scratch.
	pool := dbtest.FreshPool(t)
	ctx := context.Background()
	want := embeddedMigrationCount(t)

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != want {
		t.Fatalf("schema_migrations has %d rows, want %d (one per embedded file)", count, want)
	}

	// Versions must be recorded in ascending order with the file's own name: the
	// name is what an operator reads to answer "what has been applied?".
	rows, err := pool.Query(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var versions []int64
	names := map[int64]string{}
	for rows.Next() {
		var version int64
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		versions = append(versions, version)
		names[version] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	for i, version := range versions {
		if version != int64(i+1) {
			t.Errorf("version at position %d is %d; migrations must be numbered 1..N without gaps", i, version)
		}
	}
	// Phase 0 and Phase 1 artefacts, named explicitly so a rename is a test
	// failure rather than a surprise in production.
	if names[1] != "bootstrap" {
		t.Errorf("version 1 name = %q, want bootstrap", names[1])
	}
	if names[2] != "users" {
		t.Errorf("version 2 name = %q, want users", names[2])
	}
	if names[3] != "sessions" {
		t.Errorf("version 3 name = %q, want sessions", names[3])
	}
	// Phase 3 artefacts (§69). The names are pinned for the same reason: a
	// classroom migration silently renamed would leave a database that no longer
	// matches the constraint names the repository translates into business errors.
	if names[4] != "classrooms" {
		t.Errorf("version 4 name = %q, want classrooms", names[4])
	}
	if names[5] != "classroom_students" {
		t.Errorf("version 5 name = %q, want classroom_students", names[5])
	}
	if names[6] != "classroom_runs" {
		t.Errorf("version 6 name = %q, want classroom_runs", names[6])
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

	// Phase 1 and Phase 3 objects. Asserting on the catalog rather than on "the
	// query works" keeps the failure message specific: a missing CHECK constraint is
	// otherwise only noticed when bad data gets in.
	for _, table := range []string{"users", "sessions", "classrooms", "classroom_students", "classroom_runs"} {
		if !objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table) {
			t.Errorf("table %q does not exist", table)
		}
	}
	for _, constraint := range []string{
		"users_password_by_role", "users_account_format", "users_role_valid",
		"users_status_valid", "users_display_name_not_blank", "sessions_expires_after_issued",
		// Phase 3 (§10/§8): the two biconditionals, the owner FK that 0006 adds, and
		// the run's own OPEN/closed_at consistency rule.
		"classrooms_name_not_blank", "classrooms_status_valid", "classrooms_run_consistency",
		"classrooms_current_run_fk", "classroom_runs_status_valid", "classroom_runs_closed_at",
	} {
		if !objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, constraint) {
			t.Errorf("constraint %q does not exist", constraint)
		}
	}
	for _, index := range []string{
		"users_role_status_idx", "sessions_user_idx", "sessions_expires_idx",
		"classrooms_owner_idx", "classroom_students_student_idx",
		// The partial unique index is the database-level half of "one open run per
		// classroom"; its WHERE clause is asserted below, because an index with the
		// right name and no predicate would let two live runs coexist.
		"classroom_runs_one_open_idx",
	} {
		if !objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, index) {
			t.Errorf("index %q does not exist", index)
		}
	}

	// The partial predicate itself, and the owner-role trigger. Both are the kind of
	// object whose absence is invisible until the day it matters.
	if !objectExists(t, pool, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			 WHERE indexname = $1
			   AND indexdef LIKE '%WHERE (status = ''OPEN''::text)%')`, "classroom_runs_one_open_idx") {
		t.Error("classroom_runs_one_open_idx is not a partial index on status = 'OPEN'")
	}
	if !objectExists(t, pool, `
		SELECT EXISTS (
			SELECT 1 FROM pg_trigger
			 WHERE tgname = $1 AND NOT tgisinternal)`, "classrooms_owner_is_teacher_trigger") {
		t.Error("the classrooms owner-role trigger does not exist (§10)")
	}

	// Status must report every migration as applied and none as unknown.
	entries, err := migrate.Status(ctx, pool)
	if err != nil {
		t.Fatalf("migrate.Status() failed: %v", err)
	}
	if len(entries) != want {
		t.Fatalf("Status() returned %d entries, want %d", len(entries), want)
	}
	for _, entry := range entries {
		if !entry.Applied || entry.Unknown || entry.AppliedAt == nil {
			t.Errorf("Status() entry %d = %+v, want applied and known with a timestamp", entry.Version, entry)
		}
	}
}

// TestMigrationUpIsConcurrencySafe proves the advisory lock does its job: several
// API replicas booting at the same moment must not double-apply a migration or
// fail each other with a duplicate-key error.
func TestMigrationUpIsConcurrencySafe(t *testing.T) {
	// An empty database on purpose: the race this test guards against only exists
	// on the FIRST run.
	pool := dbtest.EmptyPool(t)
	ctx := context.Background()
	want := embeddedMigrationCount(t)

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
	if applied != want {
		t.Errorf("concurrent Up() applied %d migrations in total, want exactly %d", applied, want)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != want {
		t.Errorf("schema_migrations has %d rows, want %d", rows, want)
	}
}

// objectExists runs an EXISTS query against the system catalog.
func objectExists(t *testing.T, pool *pgxpool.Pool, query string, arg string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), query, arg).Scan(&exists); err != nil {
		t.Fatalf("catalog query failed: %v", err)
	}
	return exists
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
