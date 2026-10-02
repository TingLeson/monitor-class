// Package migrate applies versioned SQL migrations to PostgreSQL.
//
// WHY versioned files instead of an AutoMigrate step (§60):
//
//   - A migration is reviewable. Reviewers see the exact DDL, in order, in a
//     diff — including the ones that drop or rewrite data.
//   - Production migration becomes an explicit release action (cmd/migrate), not
//     a side effect of starting a new binary. That is what allows a rollback
//     window, a backup, and a maintenance note.
//   - The schema and the phase plan stay in sync: 0001_bootstrap.sql is Phase 0,
//     and Phase 1 adds 0002_*, never an edit to 0001.
//
// The runner is intentionally small and boring: forward-only, one transaction per
// file, advisory-locked against concurrent runners.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/migrations"
)

// advisoryLockKey serialises concurrent migrators (several API replicas booting
// with DB_AUTO_MIGRATE, or a deploy job racing a container restart).
//
// WHY an advisory lock and not a table lock: the lock must exist before the
// bookkeeping table does, and it must be released automatically if the process
// dies mid-migration — both of which session-level advisory locks guarantee.
const advisoryLockKey int64 = 0x0C1A55_0001 // "CLASS 0001", stable forever

// filenamePattern is the only accepted migration file name shape: NNNN_name.sql.
//
// The numeric prefix is zero-padded to four digits so lexicographic order equals
// numeric order; enforcing the shape at load time means a typo like
// "3_add_users.sql" fails loudly instead of being applied out of order.
var filenamePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is one parsed migration file.
type Migration struct {
	// Version is the numeric prefix, e.g. 1 for 0001_bootstrap.sql.
	Version int64
	// Name is the human-readable suffix, e.g. "bootstrap".
	Name string
	// Filename is kept for error messages: an operator needs the file, not the
	// version, to fix a failure.
	Filename string
	// SQL is the raw file content.
	SQL string
}

// Applied describes a row of schema_migrations, used by Status.
type Applied struct {
	Version   int64
	Name      string
	AppliedAt time.Time
}

// Up applies every pending migration in ascending version order.
//
// Idempotency is a hard requirement: running Up twice must be a no-op. Callers
// therefore get the list of migrations actually applied, which is empty on the
// second run.
func Up(ctx context.Context, pool *pgxpool.Pool) ([]Migration, error) {
	if pool == nil {
		return nil, errors.New("migrate: nil connection pool")
	}
	list, err := load()
	if err != nil {
		return nil, err
	}

	// The connection that holds the advisory lock must be the same one that runs
	// the migrations, so we take a dedicated connection instead of letting the
	// pool hand out different ones.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	// The lock comes FIRST, before any DDL. `CREATE TABLE IF NOT EXISTS` is not
	// atomic against a concurrent creator: two sessions can both pass the
	// existence check and then race on the catalog insert, and one of them fails
	// with a duplicate-key error on pg_type_typname_nsp_index. Taking the advisory
	// lock first removes the race entirely, and it works because an advisory lock
	// needs no table to exist.
	release, err := acquireLock(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}
	defer release()

	if err := ensureBookkeeping(ctx, conn.Conn()); err != nil {
		return nil, err
	}

	applied, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	var freshlyApplied []Migration
	for _, m := range list {
		if _, ok := applied[m.Version]; ok {
			// Already applied: skip silently. A migration is immutable once it has
			// run anywhere, so re-checking its contents would imply otherwise.
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return freshlyApplied, err
		}
		freshlyApplied = append(freshlyApplied, m)
	}
	return freshlyApplied, nil
}

// Status returns every migration known to the binary and whether it is applied.
//
// It is read-only on purpose: answering "where are we?" must be safe to run
// against production at any moment, including during an incident.
func Status(ctx context.Context, pool *pgxpool.Pool) ([]StatusEntry, error) {
	if pool == nil {
		return nil, errors.New("migrate: nil connection pool")
	}
	list, err := load()
	if err != nil {
		return nil, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	if err := ensureBookkeeping(ctx, conn.Conn()); err != nil {
		return nil, err
	}
	applied, err := appliedFull(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	byVersion := make(map[int64]Applied, len(applied))
	for _, a := range applied {
		byVersion[a.Version] = a
	}

	entries := make([]StatusEntry, 0, len(list)+len(applied))
	seen := make(map[int64]bool, len(list))
	for _, m := range list {
		entry := StatusEntry{Migration: m}
		if a, ok := byVersion[m.Version]; ok {
			entry.Applied = true
			appliedAt := a.AppliedAt
			entry.AppliedAt = &appliedAt
		}
		entries = append(entries, entry)
		seen[m.Version] = true
	}
	// A row without a matching file means the running binary is older than the
	// database. That is information, not an error: it tells an operator to
	// deploy, and hiding it would make the mismatch unexplainable.
	for _, a := range applied {
		if seen[a.Version] {
			continue
		}
		appliedAt := a.AppliedAt
		entries = append(entries, StatusEntry{
			Migration: Migration{Version: a.Version, Name: a.Name, Filename: fmt.Sprintf("%04d_%s.sql", a.Version, a.Name)},
			Applied:   true,
			AppliedAt: &appliedAt,
			Unknown:   true,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Version < entries[j].Version })
	return entries, nil
}

// StatusEntry is one row of the Status report.
type StatusEntry struct {
	Migration
	Applied   bool
	AppliedAt *time.Time
	// Unknown marks a row present in the database but absent from this binary.
	Unknown bool
}

// applyOne runs a single migration inside its own transaction.
//
// WHY one transaction per file: a partially applied migration would leave the
// schema in a state no file describes, and the version would not be recorded, so
// a retry would re-run the first half. PostgreSQL DDL is transactional, so this
// is achievable and worth the cost.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migration %s (version %d): begin: %w", m.Filename, m.Version, err)
	}
	// Rollback is a no-op after a successful Commit, so it can be deferred
	// unconditionally.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %s (version %d): exec: %w", m.Filename, m.Version, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("migration %s (version %d): record: %w", m.Filename, m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration %s (version %d): commit: %w", m.Filename, m.Version, err)
	}
	return nil
}

// createBookkeepingTable creates the bookkeeping table, tolerating the
// concurrent-creator race.
//
// WHY the DO block: `CREATE TABLE IF NOT EXISTS` checks for the table and creates
// it in two steps, so two sessions doing it at the same moment can both pass the
// check and then collide in the system catalog. Up() prevents that by holding the
// advisory lock, but Status() deliberately does not take the lock — it must stay
// answerable during an incident, even while a slow migration holds it. Catching
// the two expected errors keeps both paths safe.
const createBookkeepingTable = `
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS schema_migrations (
        version    bigint PRIMARY KEY,
        name       text NOT NULL,
        applied_at timestamptz NOT NULL DEFAULT now()
    );
EXCEPTION
    WHEN duplicate_table OR unique_violation THEN
        -- The other session won the race; its table is the one we want.
        NULL;
END $$`

func ensureBookkeeping(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, createBookkeepingTable); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	return nil
}

func acquireLock(ctx context.Context, conn *pgx.Conn) (func(), error) {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	return func() {
		// Use a fresh context: the caller's may already be cancelled during
		// shutdown, and the lock must still be released on this connection.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(releaseCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int64]struct{}, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	versions := make(map[int64]struct{})
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		versions[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: iterate schema_migrations: %w", err)
	}
	return versions, nil
}

func appliedFull(ctx context.Context, conn *pgx.Conn) ([]Applied, error) {
	rows, err := conn.Query(ctx, `SELECT version, name, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []Applied
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: iterate schema_migrations: %w", err)
	}
	return out, nil
}

// load reads and parses the embedded migration files.
func load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}

	list := make([]Migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		m, err := parseMigration(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, dup := seen[m.Version]; dup {
			return nil, fmt.Errorf("migrate: duplicate version %d in %s and %s", m.Version, previous, m.Filename)
		}
		content, err := fs.ReadFile(migrations.FS, path.Join(".", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", entry.Name(), err)
		}
		m.SQL = string(content)
		seen[m.Version] = m.Filename
		list = append(list, m)
	}

	if len(list) == 0 {
		return nil, errors.New("migrate: no embedded migrations found")
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Version < list[j].Version })
	return list, nil
}

// parseMigration extracts version and name from a file name, rejecting anything
// that does not match NNNN_name.sql.
func parseMigration(filename string) (Migration, error) {
	matches := filenamePattern.FindStringSubmatch(filename)
	if matches == nil {
		return Migration{}, fmt.Errorf(
			"migrate: invalid migration filename %q: expected NNNN_name.sql, e.g. 0002_users.sql",
			filename)
	}
	version, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return Migration{}, fmt.Errorf("migrate: parse version from %q: %w", filename, err)
	}
	if version == 0 {
		// Version 0 would sort before the bootstrap migration and is reserved as
		// "nothing applied yet" in status output.
		return Migration{}, fmt.Errorf("migrate: invalid version 0 in %q", filename)
	}
	return Migration{Version: version, Name: matches[2], Filename: filename}, nil
}
