// Command api runs the ClassWatch Control Plane HTTP server.
//
// The API is the only component that decides what is true: authentication,
// classroom state and media authorization all live here, backed by PostgreSQL.
// LiveKit moves pixels; it never decides permission (§33).
//
// Boot policy (see config.StartupRequireDependencies): if PostgreSQL, Redis or
// LiveKit cannot be reached, the process FAILS instead of serving traffic. An API
// that accepts requests it cannot answer would report healthy while the Control
// Plane disagrees with reality — the exact failure this architecture forbids.
// Setting STARTUP_REQUIRE_DEPENDENCIES=false relaxes this for tests and for local
// work when only part of the stack is up; /readyz then reports the truth.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/redis"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// shutdownTimeout bounds graceful shutdown. Ten seconds is enough for in-flight
// requests to finish and short enough that an orchestrator's SIGKILL window
// (usually 30s) is never reached.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet (configuration failure), so this last line
		// goes to stderr through slog's default handler — which is also what makes
		// a crash visible in `docker logs` for a container that never got far
		// enough to configure logging.
		slog.Error("classwatch api exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	logger := logging.New(cfg, os.Stdout)
	logging.SetupDefault(logger)
	logging.LogStartup(logger, cfg, httpapi.Version, httpapi.Commit)
	// A debug-only line that answers "did the secret load?" without ever printing
	// it: logging the secret once while debugging is the most common way it ends
	// up in a file that later gets attached to a ticket.
	logger.Debug("dependency endpoints resolved",
		"postgres", config.RedactDSN(cfg.DatabaseURL),
		"redis_addr", cfg.RedisAddr,
		"livekit_api_url", cfg.LiveKitAPIURL,
		logging.Redact("livekit_api_secret", cfg.LiveKitAPISecret),
	)

	// Cancelled on SIGINT/SIGTERM, which is what makes a `docker stop` or a
	// Kubernetes rollout drain instead of cutting connections.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := connectDependencies(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer deps.close()

	if cfg.DBAutoMigrate {
		if deps.postgres == nil {
			logger.Warn("DB_AUTO_MIGRATE is enabled but PostgreSQL is not connected; skipping migrations")
		} else {
			applied, err := migrate.Up(ctx, deps.postgres.Pool())
			if err != nil {
				// A failed migration means the schema does not match the binary.
				// Starting anyway would produce queries against columns that do not
				// exist, so this is always fatal.
				return fmt.Errorf("auto migrate: %w", err)
			}
			logger.Info("auto migrate finished", "applied", len(applied))
		}
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Logger: logger,
		Config: cfg,
		Ready:  deps.readiness(),
	})

	server := &http.Server{
		Addr:    cfg.APIAddr,
		Handler: router,
		// Bounds a slow or malicious client. ReadHeaderTimeout is the one that
		// matters for slowloris: it covers the window before any handler runs.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.APIAddr, "env", cfg.AppEnv)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining connections", "timeout", shutdownTimeout)
	}

	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to wait for in-flight requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// Report but do not retry: after the deadline the process must exit, and
		// the connections are gone either way.
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// dependencies bundles the connected clients so their lifetimes are managed in
// one place.
type dependencies struct {
	postgres *postgres.DB
	redis    *redis.Client
	livekit  *media.Client
}

// readiness maps the connected clients onto the probe interfaces. A client that
// failed to connect stays nil, which /readyz reports as "not configured" — the
// honest answer when nothing was ever contacted.
func (d *dependencies) readiness() httpapi.ReadinessDeps {
	deps := httpapi.ReadinessDeps{}
	if d.postgres != nil {
		deps.Postgres = d.postgres
	}
	if d.redis != nil {
		deps.Redis = d.redis
	}
	if d.livekit != nil {
		deps.LiveKit = d.livekit
	}
	return deps
}

func (d *dependencies) close() {
	// Reverse order of connection, and each Close tolerates a nil client.
	if d.redis != nil {
		_ = d.redis.Close()
	}
	if d.postgres != nil {
		d.postgres.Close()
	}
}

// connectDependencies brings up PostgreSQL, Redis and the LiveKit client.
//
// When StartupRequireDependencies is false a failure is logged and the process
// keeps running with a nil client: useful for tests and for local UI work, and
// safe because /readyz reports degraded, so nothing routes real traffic here.
func connectDependencies(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*dependencies, error) {
	deps := &dependencies{}

	fatal := func(what string, err error) error {
		if cfg.StartupRequireDependencies {
			return fmt.Errorf("%s: %w", what, err)
		}
		logger.Error("dependency unavailable; continuing in degraded mode",
			"dependency", what, "error", err,
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false")
		return nil
	}

	db, err := postgres.Connect(ctx, cfg)
	if err != nil {
		if err := fatal("postgres", err); err != nil {
			return nil, err
		}
	} else {
		deps.postgres = db
		logger.Info("postgres connected", "pool_max_conns", cfg.DBMaxConns)
	}

	rds, err := redis.Connect(ctx, redis.Config{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		if err := fatal("redis", err); err != nil {
			return nil, err
		}
	} else {
		deps.redis = rds
		logger.Info("redis connected", "addr", cfg.RedisAddr, "db", cfg.RedisDB)
	}

	// NewClient only fails on missing configuration, which config.Load has
	// already rejected — so this is a programming error, not an outage.
	lk, err := media.NewClient(cfg.LiveKitAPIURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
	if err != nil {
		return nil, fmt.Errorf("livekit client: %w", err)
	}
	deps.livekit = lk

	// Reachability, not just construction: a client pointing at a dead server is
	// a boot failure by default, because the Control Plane cannot authorize media
	// it cannot talk to.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := lk.HealthCheck(probeCtx); err != nil {
		if err := fatal("livekit", err); err != nil {
			return nil, err
		}
		deps.livekit = nil
	} else {
		logger.Info("livekit reachable", "api_url", cfg.LiveKitAPIURL)
	}

	return deps, nil
}
