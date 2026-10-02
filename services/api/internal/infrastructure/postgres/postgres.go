// Package postgres owns the PostgreSQL connection pool.
//
// PostgreSQL is the Source of Truth of the whole system: whether a classroom is
// open, who is assigned to it and which sessions are active are facts that live
// here and nowhere else. A LiveKit room existing proves nothing (§33).
//
// There is deliberately no AutoMigrate in this package. Schema changes go
// through internal/infrastructure/migrate as versioned SQL files, because an
// implicit "make the schema match the structs" step cannot be reviewed, cannot
// be rolled forward safely and would be a production hazard (§60).
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/config"
)

// DB wraps *pgxpool.Pool so call sites depend on this package's small surface
// rather than on pgx directly, which keeps a future pool swap a one-file change.
type DB struct {
	pool *pgxpool.Pool
}

// Connect opens the pool and verifies it can actually reach the server.
//
// WHY ping at connect time instead of lazily on first query: a control-plane API
// that accepts traffic it cannot serve would report healthy to the load balancer
// while failing every request. Failing here lets main decide between "abort
// boot" (default) and "start degraded" (tests).
func Connect(ctx context.Context, cfg *config.Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		// The DSN is never echoed: it carries the database password.
		return nil, fmt.Errorf("parse DATABASE_URL (%s): %w", config.RedactDSN(cfg.DatabaseURL), err)
	}

	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	poolCfg.MaxConnLifetime = cfg.DBMaxConnLifetime
	poolCfg.HealthCheckPeriod = cfg.DBHealthCheckPeriod
	// Idle connections are recycled well before typical NAT/firewall timeouts so
	// the first query after a quiet period does not pay for a reconnect.
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	// A bounded connect timeout turns an unreachable database into a fast,
	// readable startup error instead of a hanging process.
	poolCfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool (%s): %w", config.RedactDSN(cfg.DatabaseURL), err)
	}

	db := &DB{pool: pool}
	if err := db.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.RedactDSN(cfg.DatabaseURL), err)
	}
	return db, nil
}

// Pool exposes the underlying pool for repositories added in later phases.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping verifies connectivity with a caller-controlled timeout.
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return fmt.Errorf("postgres: not connected")
	}
	return db.pool.Ping(ctx)
}

// Close releases every pooled connection. It is idempotent, so shutdown paths
// can call it unconditionally.
func (db *DB) Close() {
	if db == nil || db.pool == nil {
		return
	}
	db.pool.Close()
	db.pool = nil
}
