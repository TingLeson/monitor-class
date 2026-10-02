// Package dbtest holds the helpers shared by every PostgreSQL-backed test.
//
// WHY it is a normal package rather than a _test.go file: Go test helpers cannot
// be shared across packages, and the alternative — copy-pasting "connect, migrate,
// make a unique account" into four files — is exactly how test suites start
// disagreeing about what "a fresh database" means.
//
// Every helper here skips (never fails) when TEST_DATABASE_URL is unset, so
// `go test ./...` stays green on a machine with no PostgreSQL.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
)

// connectTimeout bounds every setup step so a wrong DSN fails fast instead of
// hanging the test binary.
const connectTimeout = 20 * time.Second

// URL returns TEST_DATABASE_URL or skips the test.
//
// The repository .env is loaded first so a developer who ran `make init` does not
// have to export the variable by hand.
func URL(t *testing.T) string {
	t.Helper()
	if _, err := config.LoadDotEnv(); err != nil {
		t.Fatalf("load .env: %v", err)
	}
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	return dsn
}

// Config builds the minimal configuration postgres.Connect needs.
func Config(dsn string) *config.Config {
	return &config.Config{
		AppEnv:              config.EnvTest,
		DatabaseURL:         dsn,
		DBMaxConns:          5,
		DBMinConns:          1,
		DBMaxConnLifetime:   time.Minute,
		DBHealthCheckPeriod: 30 * time.Second,
	}
}

// migratedOnce makes the shared-database migration run exactly once per test
// binary. Migrations are advisory-locked and idempotent, so this is an
// optimisation, not a correctness requirement.
var migratedOnce sync.Once

// Pool returns a migrated pool against the shared TEST_DATABASE_URL database.
//
// The schema is migrated in place rather than recreated: several test packages
// may run in parallel against the same database (`go test ./...`), and dropping
// the schema in one of them would break the others. Tests therefore never assume
// an empty database — they create rows with unique accounts and clean up after
// themselves.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := URL(t)
	db := connect(t, dsn)
	pool := db.Pool()

	migratedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := migrate.Up(ctx, pool); err != nil {
			t.Fatalf("migrate.Up() failed: %v", err)
		}
	})
	return pool
}

// FreshPool returns a pool against a brand-new throwaway database with every
// migration applied from scratch.
//
// It exists for the tests that must observe the FIRST migration run (does 0002
// create the constraints? is a concurrent run safe?). Those assertions cannot be
// made against a shared database that has already been migrated. The database is
// dropped again on cleanup.
func FreshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	freshPool := EmptyPool(t)

	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer migrateCancel()
	if _, err := migrate.Up(migrateCtx, freshPool); err != nil {
		t.Fatalf("migrate.Up() on a fresh database failed: %v", err)
	}
	return freshPool
}

// EmptyPool returns a pool against a brand-new throwaway database with NO
// migrations applied. The database is dropped again on cleanup.
func EmptyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := URL(t)
	admin := connect(t, dsn)
	adminPool := admin.Pool()

	name := "classwatch_test_" + randomHex(6)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	// CREATE DATABASE cannot run inside a transaction and cannot be parameterised,
	// so the identifier is built from a locally generated hex suffix: nothing here
	// comes from outside the process.
	if _, err := adminPool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Skipf("cannot create a throwaway database (needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		// DROP DATABASE needs no other session connected to the target; WITH
		// (FORCE) terminates leftovers from a failed test instead of failing the
		// cleanup and leaving a database behind.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), connectTimeout)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("could not drop test database %s: %v", name, err)
		}
	})

	freshDSN, err := replaceDatabase(dsn, name)
	if err != nil {
		t.Fatalf("build DSN for %s: %v", name, err)
	}
	// The cleanup registered by connect() runs before the DROP registered above
	// (cleanups are LIFO), so the pool is closed first.
	return connect(t, freshDSN).Pool()
}

// connect opens a pool that is closed on cleanup.
func connect(t *testing.T, dsn string) *postgres.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := postgres.Connect(ctx, Config(dsn))
	if err != nil {
		t.Fatalf("postgres.Connect() failed: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// replaceDatabase swaps the database name in a PostgreSQL URL.
func replaceDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}

// RandomAccount returns a unique, format-valid account for one test.
//
// Uniqueness is what lets several tests share one database without truncating
// each other's rows; the format compliance means the account passes
// users_account_format, so a test never fails for the wrong reason.
func RandomAccount(prefix string) string {
	return prefix + "_" + randomHex(6)
}

// RandomHex returns n random bytes as hex, for unique identifiers in tests.
func RandomHex(n int) string { return randomHex(n) }

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// A failing CSPRNG in a test binary is unrecoverable, and panicking here
		// is clearer than threading an error through every call site.
		panic(fmt.Sprintf("dbtest: read random bytes: %v", err))
	}
	return hex.EncodeToString(buf)
}
