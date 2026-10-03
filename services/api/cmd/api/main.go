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

	"github.com/classwatch/classwatch/services/api/internal/admin"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/redis"
	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/realtime"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

// shutdownTimeout bounds one STEP of graceful shutdown (the HTTP drain and the
// socket close each get it, and each gets a fresh deadline). The total budget is
// therefore bounded by roughly 3x this, which is still comfortably inside the 30s
// SIGKILL grace period container runtimes use by default.
//
// The value itself is HTTP_SHUTDOWN_TIMEOUT (see config.ApplyDefaults for why 10s).

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

	// The metric set exists before anything that reports into it. It is created
	// unconditionally: /metrics is part of the production surface of §77, and the
	// cost of an unused registry is a few hundred bytes per family.
	metricSet := metrics.New()
	metricSet.SetBuildInfo(httpapi.Version, httpapi.Commit)
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

	// The drain gate is the difference between "the listener is closed" and "this
	// process tells the truth about going away": connections that are already open
	// (keep-alive, HTTP/2) can still deliver requests after Shutdown starts, and
	// those must get 503 SERVICE_UNAVAILABLE instead of being served by a process
	// that is about to exit (see httpapi.DrainGate).
	drain := httpapi.NewDrainGate()

	deps, err := connectDependencies(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer deps.close()

	// The media client reports every RoomService call (§77). It is attached here,
	// before any service captures the client, so no call can happen uninstrumented.
	if deps.livekit != nil {
		deps.livekit.WithMetrics(metricSet)
	}

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

	// Authentication and administration share one user repository and one
	// session store: they are two views of the same accounts, and giving each its
	// own would let them disagree about what a user row says.
	users := deps.userRepository(logger)

	// Phase 8's runtime layer (§47): one hub for the process, one service that decides
	// who receives what, and the processor that turns media-plane observations into
	// session states, event rows and messages. It is built before the router because the
	// classroom lifecycle and the session service both report into it.
	//
	// events is the same realtime service handed out a second time, as the private-talk
	// message path of §31: the session service produces the facts and the realtime layer
	// decides who may hear them, exactly as it does for the screen and camera messages.
	runtime, hub, events := deps.runtimeLayer(logger, cfg, metricSet)
	if hub != nil {
		// Close the sockets with a proper "going away" frame before the HTTP server
		// stops: WebSocket connections are hijacked, so http.Server.Shutdown does not
		// wait for them, and a client that is told why it was disconnected reconnects
		// with a backoff instead of treating a deploy as a network failure.
		defer func() {
			hubCtx, hubCancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
			defer hubCancel()
			hub.Shutdown(hubCtx)
		}()
	}

	// The student session service is built before the router because the processor needs
	// it back: the private-talk state machine is what revokes the subscription of a target
	// who left or of a lesson that closed (§31), and the two are wired in a cycle on
	// purpose — the session service reports its lifecycle into the processor, and the
	// processor reports "the target is gone" back into the state machine.
	sessions := deps.sessionService(logger, cfg, users, runtime, events)
	if sessions != nil {
		sessions.WithMetrics(metricSet)
	}
	if runtime != nil && sessions != nil {
		runtime.WithPrivateTalkEnder(sessions)
	}

	depsForRouter := httpapi.Deps{
		Logger:    logger,
		Config:    cfg,
		Ready:     deps.readiness(),
		Auth:      deps.authService(logger, cfg, users),
		Admin:     deps.adminService(logger, cfg, users),
		Classroom: deps.classroomService(logger, users, runtime),
		Webhook:   deps.webhookDeps(cfg, runtime),
		Socket:    hub,
		Limiter:   deps.rateLimiter(logger),
		Metrics:   metricSet,
		Drain:     drain,
	}
	// The two session-shaped fields are interface fields, and a nil *session.Service stored
	// in one would NOT compare equal to nil: the route groups would register and then call a
	// method on a nil receiver. Assigning only when there is a service keeps the degraded
	// deployment (no database) answering 404 there, as it did before Phase 10.
	if sessions != nil {
		depsForRouter.Session = sessions
		depsForRouter.PrivateTalk = sessions
	}

	router := httpapi.NewRouter(depsForRouter)

	// Every derived gauge is published from this loop: the session census, the pool
	// pressure and the private-talk count (see cmd/api/metrics.go for why they are
	// sampled and not counted).
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	defer stopSampler()
	var poolForSampler *pgxpool.Pool
	if deps.postgres != nil {
		poolForSampler = deps.postgres.Pool()
	}
	go newMetricsSampler(logger, cfg, metricSet, poolForSampler, sessions).run(samplerCtx)

	// Every timeout comes from the configuration (§63), where the reasoning behind
	// each default is written down next to the field. The names are used instead of
	// literals so that reading this block tells an operator exactly which knob to
	// turn. WriteTimeout does NOT cut a WebSocket: see config.HTTPWriteTimeout and
	// realtime's timeout test.
	server := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening",
			"addr", cfg.APIAddr,
			"env", cfg.AppEnv,
			"read_header_timeout", cfg.HTTPReadHeaderTimeout.String(),
			"read_timeout", cfg.HTTPReadTimeout.String(),
			"write_timeout", cfg.HTTPWriteTimeout.String(),
			"idle_timeout", cfg.HTTPIdleTimeout.String(),
			"max_body_bytes", cfg.HTTPMaxBodyBytes,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	shutdownStarted := time.Time{}
	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownStarted = time.Now()
		logger.Info("shutdown signal received; draining connections",
			"timeout", cfg.HTTPShutdownTimeout.String(),
			"sequence", "drain gate -> drain delay -> http shutdown -> websocket close -> pool close",
		)

		// The order below is the whole of §62's graceful shutdown, and each step
		// exists because the previous one cannot cover it:
		//
		//  1. Open the drain gate. From this instant every request that arrives on a
		//     connection the load balancer has not retired yet is answered 503 with
		//     Retry-After, instead of being served by a process that is about to
		//     exit or dropped in a way the client cannot interpret.
		drain.BeginDraining()

		//  2. Keep SERVING for HTTP_DRAIN_DELAY while the gate answers 503.
		//
		//     WHY this step exists at all (it was found by observing a real SIGTERM,
		//     not by reading the docs): http.Server.Shutdown stops the listeners
		//     immediately AND silently drops a request that arrives on a connection it
		//     has already decided to retire. A browser cannot tell that apart from a
		//     network failure. The delay is the window in which
		//       (a) an already-established connection gets a real 503 with Retry-After
		//           instead of a reset, and
		//       (b) a load balancer's readiness probe (which now fails) takes this
		//           instance out of rotation before the listener closes.
		//     With a zero delay this step is skipped, which is correct only when
		//     nothing routes to this process.
		if cfg.HTTPDrainDelay > 0 {
			logger.Info("drain gate open; serving 503 before closing the listener",
				"drain_delay", cfg.HTTPDrainDelay.String())
			// ctx is already cancelled (it is what got us here), so the wait has its
			// own timer.
			timer := time.NewTimer(cfg.HTTPDrainDelay)
			<-timer.C
		}

		//  3. Stop accepting new connections and wait for in-flight requests. A
		//     fresh context: ctx is already cancelled, and Shutdown needs a live
		//     deadline to wait for.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
		if err := server.Shutdown(shutdownCtx); err != nil {
			// Report but do not retry: after the deadline the process must exit, and
			// the connections are gone either way. The websocket and pool steps below
			// still run, because leaving sockets open on a process that is exiting is
			// what makes a deploy look like a network failure to every browser.
			logger.Error("graceful shutdown of the http server did not finish in time",
				"action", "shutdown_timeout", "error", err, "timeout", cfg.HTTPShutdownTimeout.String())
		}
		cancel()

		//  4. Stop the metric sampler before the pool closes, so a sample cannot race
		//     the pool teardown and log a spurious error during a clean exit.
		stopSampler()

		//  5. Close the WebSocket connections with a "going away" frame (the deferred
		//     hub shutdown above does this against its own deadline), then let the
		//     deferred pool closes run. Both are deferred so that an early return from
		//     connectDependencies, or a panic, still tears them down — a shutdown path
		//     that only works on the happy path is not a shutdown path.
		logger.Info("http drain complete; closing websockets and dependencies",
			"elapsed_ms", time.Since(shutdownStarted).Milliseconds())
	}
	logger.Info("shutdown complete", "elapsed_ms", time.Since(shutdownStarted).Milliseconds())
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

// userRepository builds the account repository, or returns nil when PostgreSQL is
// not connected. A nil repository is what makes both the auth and the admin
// routes disappear from the router (see the two services below).
func (d *dependencies) userRepository(logger *slog.Logger) user.Repository {
	if d.postgres == nil {
		return nil
	}
	return user.NewPostgres(d.postgres.Pool())
}

// authService builds the authentication service, or returns nil when PostgreSQL
// is not connected.
//
// nil is not a fallback to "no authentication": httpapi.NewRouter simply does not
// register the auth routes, so an API without a database answers 404 there
// instead of accepting logins it could never verify. That is the honest behaviour
// for the degraded mode STARTUP_REQUIRE_DEPENDENCIES=false enables.
func (d *dependencies) authService(logger *slog.Logger, cfg *config.Config, users user.Repository) httpapi.AuthService {
	if users == nil {
		logger.Warn("postgres is not connected; authentication routes are disabled",
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false must only be used for local work")
		return nil
	}
	return auth.NewService(
		users,
		sessionstore.New(d.postgres.Pool()),
		auth.Config{
			SessionTTL:        cfg.SessionTTL,
			IdleTouchInterval: cfg.SessionIdleTouchInterval,
			PasswordPolicy:    auth.NewPasswordPolicy(cfg.PasswordMinLength),
		},
	)
}

// adminService builds the account-administration service (§4/§68).
//
// It shares the repository with authentication on purpose: "an account is
// ACTIVE" must mean the same thing to the login path and to the admin list, and
// two repositories would make that a coincidence rather than a property.
//
// The password policy is the same object the login flow uses, so the API cannot
// accept a password the CLI would reject.
func (d *dependencies) adminService(logger *slog.Logger, cfg *config.Config, users user.Repository) httpapi.AdminService {
	if users == nil {
		logger.Warn("postgres is not connected; admin user-management routes are disabled",
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false must only be used for local work")
		return nil
	}
	return admin.NewService(
		users,
		sessionstore.New(d.postgres.Pool()),
		auth.NewPasswordPolicy(cfg.PasswordMinLength),
	)
}

// classroomService builds the classroom domain service (§69).
//
// It shares the account repository with authentication and administration: adding
// a student to a roster must see the same account rows the login path validates,
// and "this account is a STUDENT" has to mean one thing in the whole process. The
// classroom repository itself is separate because it owns different tables — and,
// more importantly, because its Open/Close transactions are the only place allowed
// to lock a classroom row.
//
// The LiveKit client is attached as the room terminator (§49) so a closed classroom
// ends its media room. It is attached only when LiveKit was reachable at boot: a
// degraded process has no media plane to clean up, and Close must not fail because of
// one.
//
// The runtime hooks (§47/§48/§49) are attached when the event layer exists: open
// broadcasts ROOM_OPENED, close marks the run's sessions ROOM_CLOSED and broadcasts
// ROOM_CLOSED — all after the commit, all unable to fail the teacher's request.
func (d *dependencies) classroomService(logger *slog.Logger, users user.Repository, runtime *session.Processor) httpapi.ClassroomService {
	if users == nil {
		logger.Warn("postgres is not connected; teacher classroom routes are disabled",
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false must only be used for local work")
		return nil
	}
	service := classroom.NewService(classroom.NewPostgres(d.postgres.Pool()), users)
	if d.livekit != nil {
		service.WithRoomTerminator(d.livekit)
	}
	if runtime != nil {
		service.WithRuntimeHooks(runtime)
	}
	return service
}

// sessionService builds the student media session service (§43/§51).
//
// It reuses the classroom service for every authorization question (roster, ownership,
// classroom state) instead of querying those tables itself: "may this student enter?"
// must have exactly one implementation, or the join endpoint and the classroom portal
// will eventually disagree about who is allowed in.
//
// The media client is passed even when LiveKit was unreachable at boot — the service
// then answers MEDIA_TOKEN_FAILED on join/media-token rather than pretending to have a
// media plane, while the monitor still reports the control plane's own state (§33).
//
// The runtime processor is attached as the lifecycle event path (§13): join records
// SESSION_CREATED, leave records STUDENT_LEFT and tells the teacher's console.
//
// Phase 10 adds the private-talk collaborators (§31): events is the message path
// (PRIVATE_TALK_*), and the session repository doubles as the audit log of
// TEACHER_TALK_STARTED/ENDED. Both may be nil — the state machine and the media plane still
// work, and only the two browsers and the event log lose their copy.
func (d *dependencies) sessionService(
	logger *slog.Logger,
	cfg *config.Config,
	users user.Repository,
	runtime *session.Processor,
	events *realtime.Service,
) *session.Service {
	if users == nil {
		logger.Warn("postgres is not connected; student session routes are disabled",
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false must only be used for local work")
		return nil
	}
	classrooms := classroom.NewService(classroom.NewPostgres(d.postgres.Pool()), users)
	sessionRepo := session.NewPostgres(d.postgres.Pool())

	// The classroom service instance is deliberately a second one (not the one wired
	// into the teacher routes): both are thin wrappers over the same repositories and
	// the same rules, and sharing one mutable object across two Deps fields would make
	// the room terminator attachment above depend on initialization order.
	mediaPlane := mediaPlaneOrNil(d.livekit)
	if mediaPlane == nil {
		logger.Warn("livekit is not reachable; join and media-token will answer MEDIA_TOKEN_FAILED",
			"hint", "media sessions need the media plane; the monitor still reports control-plane state")
	}
	service := session.NewService(sessionRepo, classrooms, mediaPlane, session.Config{
		LiveKitURL: cfg.LiveKitURL,
		TokenTTL:   cfg.LiveKitTokenTTL,
	})
	if runtime != nil {
		service.WithLifecycleEvents(runtime)
	}
	// The audit log is passed even when the hub is absent: recording a private talk needs
	// the database, not a websocket, and a deployment without a hub must still be able to
	// answer "was this student spoken to privately?" months later.
	service.WithPrivateTalk(events, sessionRepo)
	return service
}

// runtimeLayer builds the runtime event path of Phase 8: the WebSocket hub (§47), the
// service that resolves who may receive what (§26), and the processor that owns the
// session state machine and the event log (§13/§45).
//
// It returns (nil, nil, nil) when PostgreSQL is not connected, and (processor, hub, service)
// otherwise. A nil hub is a valid deployment: the state machine keeps working and only the
// live messages are skipped (§52's single-instance assumption, and any deployment that has
// not enabled the sockets). A nil processor disables the webhook route, because events
// that cannot be recorded must not be accepted.
//
// The realtime.Service is returned next to the processor because Phase 10 needs it as a
// second port: the private-talk state machine lives in internal/session (not in the
// processor), and it produces messages through the same audience rules.
func (d *dependencies) runtimeLayer(logger *slog.Logger, cfg *config.Config, m *metrics.Metrics) (*session.Processor, *realtime.Hub, *realtime.Service) {
	if d.postgres == nil {
		logger.Warn("postgres is not connected; runtime events and websockets are disabled",
			"hint", "STARTUP_REQUIRE_DEPENDENCIES=false must only be used for local work")
		return nil, nil, nil
	}
	hub := realtime.NewHub(realtime.HubConfig{
		Logger: logger,
		// The hub owns connection lifetime and the write queue, so it is the only
		// place that can count live sockets, messages and slow-consumer drops
		// accurately (§77).
		Metrics: m,
		// The same origin allowlist the HTTP API uses: the frontends are configured once,
		// and a WebSocket handshake must be refused for exactly the origins that are
		// refused a cross-origin fetch (§63).
		AllowedOrigins: cfg.CORSOrigins(),
	})
	runtime := realtime.NewService(hub, realtime.NewAudience(d.postgres.Pool()), logger)
	return session.NewProcessor(session.NewPostgres(d.postgres.Pool()), runtime).WithMetrics(m), hub, runtime
}

// webhookDeps builds the LiveKit webhook endpoint's two halves (§45).
//
// A nil result means the route is not registered, which is the correct behaviour when
// the LiveKit key pair is not configured or PostgreSQL is missing: an endpoint that
// cannot verify a signature, or cannot record what it verified, must not exist.
func (d *dependencies) webhookDeps(cfg *config.Config, runtime *session.Processor) *httpapi.WebhookDeps {
	if runtime == nil {
		return nil
	}
	verifier, err := media.NewWebhookVerifier(cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
	if err != nil {
		// The secret itself is never printed (§59); the message says only what is
		// missing.
		slog.Warn("livekit webhook verification is not configured; the endpoint is not registered",
			"reason", err)
		return nil
	}
	return &httpapi.WebhookDeps{Verifier: verifier, Processor: runtime}
}

// mediaPlaneOrNil narrows the LiveKit client to the media-plane port, mapping "not
// connected at boot" onto a nil interface so the session service can answer honestly
// instead of calling into a client that was never verified reachable.
func mediaPlaneOrNil(client *media.Client) session.MediaPlane {
	if client == nil {
		return nil
	}
	return client
}

// rateLimiter builds the rate limiter: Redis when it is available, degrading to
// per-process memory when it is not.
//
// WHY the fallback instead of failing: an unreachable Redis must not remove the
// only protection a password-less student login has, and it must not turn a
// cache outage into a login outage either. The fallback keeps both properties
// (see internal/ratelimit/fallback.go); the log line is what makes the degraded
// state visible.
func (d *dependencies) rateLimiter(logger *slog.Logger) ratelimit.Limiter {
	memory := ratelimit.NewMemory()
	if d.redis == nil || d.redis.Client() == nil {
		logger.Warn("redis is not connected; rate limiting runs in-memory only",
			"consequence", "limits are per API process, not cluster-wide")
		return memory
	}
	return ratelimit.NewFallback(ratelimit.NewRedis(d.redis.Client()), memory, logger)
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
