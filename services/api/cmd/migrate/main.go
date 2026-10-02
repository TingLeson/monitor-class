// Command migrate applies and inspects the versioned SQL migrations.
//
// WHY this is a separate binary rather than a flag on the API (§60): applying a
// schema change must be an explicit, observable release step. A migration that
// runs as a side effect of starting a new container is impossible to review, hard
// to time against a backup, and races every replica that boots at the same
// moment. Here it is one command with one exit code.
//
// Usage:
//
//	migrate up      apply every pending migration
//	migrate status   list migrations and whether they are applied
//
// Exit codes: 0 on success, 1 on failure (including usage errors), 2 on a
// usage error, so a deploy script can distinguish "bad invocation" from
// "migration failed".
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// commandTimeout bounds the whole run. A migration that hangs holds an advisory
// lock, which blocks every other deploy; failing fast is better than a silent
// deadlock in CI.
const commandTimeout = 5 * time.Minute

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: migrate <up|status>")
		return exitUsage
	}
	command := args[0]
	if command != "up" && command != "status" {
		fmt.Fprintf(os.Stderr, "unknown command %q: usage: migrate <up|status>\n", command)
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration: %v\n", err)
		return exitFailure
	}

	logger := logging.New(cfg, os.Stdout)
	logging.SetupDefault(logger)
	// The DSN is logged redacted: a migration log is exactly the kind of file that
	// gets attached to a ticket, and it must still say which database was touched.
	logger.Info("migrate starting",
		"command", command,
		"env", cfg.AppEnv,
		"database", config.RedactDSN(cfg.DatabaseURL),
	)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	db, err := postgres.Connect(ctx, cfg)
	if err != nil {
		// The failure is fatal by definition here: there is nothing to migrate
		// without a database. STARTUP_REQUIRE_DEPENDENCIES does not apply.
		logger.Error("cannot connect to postgres", "error", err)
		return exitFailure
	}
	defer db.Close()

	switch command {
	case "up":
		return runUp(ctx, logger, db)
	default:
		return runStatus(ctx, logger, db)
	}
}

func runUp(ctx context.Context, logger *slog.Logger, db *postgres.DB) int {
	applied, err := migrate.Up(ctx, db.Pool())
	if err != nil {
		logger.Error("migration failed", "error", err, "applied_before_failure", len(applied))
		return exitFailure
	}
	if len(applied) == 0 {
		logger.Info("database is up to date; nothing to apply")
		return exitOK
	}
	for _, m := range applied {
		logger.Info("migration applied", "version", m.Version, "name", m.Name, "file", m.Filename)
	}
	logger.Info("migrations complete", "applied", len(applied))
	return exitOK
}

func runStatus(ctx context.Context, logger *slog.Logger, db *postgres.DB) int {
	entries, err := migrate.Status(ctx, db.Pool())
	if err != nil {
		logger.Error("status failed", "error", err)
		return exitFailure
	}

	pending := 0
	for _, entry := range entries {
		switch {
		case entry.Unknown:
			// Present in the database but unknown to this binary: the running
			// version is older than the schema. Reported as an error because
			// silently ignoring it is how a deploy turns into an incident.
			logger.Error("migration recorded in the database is unknown to this binary",
				"version", entry.Version, "name", entry.Name)
		case entry.Applied:
			logger.Info("applied",
				"version", entry.Version,
				"name", entry.Name,
				"applied_at", entry.AppliedAt.Format(time.RFC3339),
			)
		default:
			pending++
			logger.Info("pending", "version", entry.Version, "name", entry.Name, "file", entry.Filename)
		}
	}

	for _, entry := range entries {
		if entry.Unknown {
			return exitFailure
		}
	}
	logger.Info("status complete", "total", len(entries), "pending", pending)
	return exitOK
}
