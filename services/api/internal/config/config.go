// Package config loads and validates every runtime setting of the ClassWatch API.
//
// Two rules shape this package:
//
//   - Boot must fail loudly, never partially. The Control Plane (Go API +
//     PostgreSQL) is the single Source of Truth; if it starts with a broken
//     DATABASE_URL the media plane would happily keep serving LiveKit rooms
//     while the database disagrees with reality. That is exactly the failure
//     mode the architecture forbids, so a missing/invalid required value is a
//     hard error here.
//
//   - No error returned by this package may contain a secret value. These
//     errors travel into logs, CI output and bug reports, so DSN passwords and
//     LiveKit secrets are always rendered through the redact helpers below.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Recognised values of APP_ENV. Anything else is a configuration bug: the value
// drives log format, cookie security expectations and error verbosity, so it
// must never be guessed.
const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"
)

// dotEnvSearchPaths are probed relative to the process working directory, in
// order. They cover the two ways this binary is started during development:
// `go run ./cmd/api` from services/api (repo root is ../../) and a compiled
// binary executed from services/api/bin (repo root is ../../../).
// A missing file is not an error: in containers configuration arrives purely
// through environment variables.
var dotEnvSearchPaths = []string{"./.env", "../../.env", "../../../.env"}

// Config is the fully validated runtime configuration. Every field is populated
// by Load, so consumers never need to re-read the environment or re-check for
// emptiness.
type Config struct {
	// AppEnv is one of development|test|production.
	AppEnv string
	// APIAddr is the listen address of the HTTP server, e.g. ":8080".
	APIAddr string
	// LogLevel is a log/slog level name: debug|info|warn|error.
	LogLevel slog.Level
	// LogFormat is "text", "json" or "" (empty = follow APP_ENV: JSON in
	// production, text elsewhere).
	//
	// WHY it is configuration and not only a consequence of APP_ENV: a staging
	// deployment wants the production log SHAPE (so the shipper and the dashboards
	// are exercised) without production's hardening, and an operator debugging a
	// single container wants JSON off. Format is a property of where the logs go,
	// which the process cannot infer.
	LogFormat string

	// HTTP server limits (§63). Every one of them has a default that is safe for
	// the public internet, and every one is overridable because a deployment
	// behind a slow internal proxy may need a different balance — but a zero value
	// is never allowed, because "no timeout" is how a slowloris takes the process
	// down.
	//
	// HTTPMaxBodyBytes bounds every request body (§63). The largest legitimate
	// body in this API is a classroom roster import, which is a few kilobytes;
	// 1 MiB leaves room for a much larger roster and still makes
	// memory-exhaustion-by-body impossible.
	HTTPMaxBodyBytes int64
	// HTTPReadHeaderTimeout bounds reading the request line and headers. It is the
	// one that defeats slowloris, because it covers the window before any handler
	// runs and before any body is read.
	HTTPReadHeaderTimeout time.Duration
	// HTTPReadTimeout bounds reading the whole request, headers and body. The
	// largest bodies here are kilobytes, so this is generous for the LAN and still
	// far below a client's patience.
	HTTPReadTimeout time.Duration
	// HTTPWriteTimeout bounds writing one response.
	//
	// # Why this does not cut a WebSocket connection
	//
	// A business socket (§47) is a HIJACKED connection: net/http clears the
	// connection deadline when a handler hijacks it, and the hub then sets its own
	// deadline around every frame it writes. A 30s write timeout therefore bounds
	// the HTTP request that performed the handshake and nothing else. This is not
	// an assumption: realtime.TestWriteTimeoutDoesNotCutWebSockets holds a socket
	// open past the timeout and exchanges a message on it.
	HTTPWriteTimeout time.Duration
	// HTTPIdleTimeout bounds how long a keep-alive connection may sit idle between
	// requests. Without it, a browser that walks away leaves a socket (and a file
	// descriptor) per abandoned tab.
	HTTPIdleTimeout time.Duration
	// HTTPShutdownTimeout bounds the whole graceful-shutdown sequence (§62):
	// stop accepting, drain in-flight requests, close the sockets, close the
	// pools. It must stay below the orchestrator's SIGKILL grace period.
	HTTPShutdownTimeout time.Duration
	// HTTPDrainDelay is how long the process keeps SERVING after the drain gate
	// opens and before it stops listening.
	//
	// WHY this exists, and why it is not optional in a load-balanced deployment:
	// http.Server.Shutdown stops the listeners immediately and — more importantly —
	// silently DROPS a request that arrives on a connection it has already decided
	// to retire. A request dropped that way is indistinguishable, in the browser,
	// from a network failure. Serving 503 SERVICE_UNAVAILABLE for a short window
	// instead is what turns a rolling deploy into "one retry" rather than "one
	// broken screen", and it is also the window in which a load balancer's
	// readiness probe (which now fails) removes this instance from rotation.
	//
	// Two seconds is the default: long enough for a 1-2s probe interval, short
	// enough that it does not noticeably extend a deploy. Zero is allowed and means
	// "stop listening immediately" — correct only when nothing routes to this
	// process (a single-instance deployment, or a test).
	HTTPDrainDelay time.Duration

	// MetricsSessionRefreshInterval is how often the session census and the other
	// sampled gauges are read from the database.
	//
	// WHY sampled and not updated on every transition: `classwatch_session_status`
	// answers an operational question ("how many sessions are in each state right
	// now?"), not a per-request one. Counting it on every state change would add a
	// database round trip to the hot path of the whole session state machine, and
	// an extra UPDATE of a counter row.
	MetricsSessionRefreshInterval time.Duration

	// CORSAllowedOrigins is the strict browser-origin allowlist. Wildcards are
	// intentionally not representable: credentialed requests plus "*" is both
	// illegal in the spec and an authorization hole (see §63).
	CORSAllowedOrigins []string
	// DBAutoMigrate runs versioned migrations at API boot. Convenient locally,
	// forbidden in production where migration is an explicit release step.
	DBAutoMigrate bool
	// StartupRequireDependencies makes boot fail when PostgreSQL, Redis or
	// LiveKit is unreachable. Default true; only tests should relax it.
	StartupRequireDependencies bool

	// DatabaseURL is the PostgreSQL DSN and the only durable Source of Truth of
	// the whole system. It must never be logged.
	DatabaseURL string
	// DB connection pool tuning. Defaults are deliberately conservative: this
	// API is a control plane, not a data pump.
	DBMaxConns          int32
	DBMinConns          int32
	DBMaxConnLifetime   time.Duration
	DBHealthCheckPeriod time.Duration

	// RedisAddr is host:port of the Redis instance used from Phase 1 onwards for
	// session store, rate limiting, presence and WebSocket fan-out.
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// LiveKitURL is the browser-facing WebSocket endpoint (ws:// or wss://).
	LiveKitURL string
	// LiveKitAPIURL is the server-side HTTP endpoint used for RoomService calls.
	// It defaults to LiveKitURL so a single-host deployment only sets one value.
	LiveKitAPIURL string
	// LiveKitAPIKey / LiveKitAPISecret sign server-to-server LiveKit calls. The
	// secret must exist only in the backend: never in frontend bundles, never in
	// logs, never in API responses (§44/§59).
	LiveKitAPIKey    string
	LiveKitAPISecret string
	// LiveKitTokenTTL is how long an issued participant token stays valid (§63 makes
	// it short-lived). Default 2h; must be > 0 and at most 24h.
	//
	// WHY not a very short TTL with an automatic refresh (the "5 minutes + renew"
	// pattern a media SDK makes easy): a reconnect in the middle of a lesson — a wifi
	// blip, a laptop lid, a browser that reloaded — has to happen with the token the
	// participant already holds, because the refresh call itself is a request the
	// student's browser may not be able to make while its media connection is down.
	// A five-minute token would turn every brief network interruption into "you must
	// re-enter the classroom", which is exactly when a supervision product must not
	// ask the student to do anything. The token is still bounded by the lesson: 2h
	// covers a long evening session, and Phase 11 adds rotation/refresh for
	// deployments that need a tighter window.
	LiveKitTokenTTL time.Duration

	// SessionCookieName is the base name of the HttpOnly session cookie. The
	// effective name is per entry point (see SessionCookieNameFor); this value is
	// only the prefix.
	SessionCookieName string
	// SessionTTL is the absolute session lifetime. Must parse and be > 0.
	SessionTTL time.Duration
	// SessionCookieSecure forces the Secure cookie attribute; must be true in
	// production (HTTPS-only).
	SessionCookieSecure bool
	// SessionIdleTouchInterval throttles the `sessions.last_seen_at` write.
	// WHY a throttle at all: without it every single request (assets included, in
	// a page that polls) would issue an UPDATE, turning a read-mostly session
	// lookup into a write-heavy hot row. Activity is still observable, just
	// coarser than the request rate.
	SessionIdleTouchInterval time.Duration

	// RateLimitLoginPerMinute caps password/account login attempts per client IP
	// and minute. §2.2 requires a rate limit because knowing a student account is
	// enough to log in as that student: brute force is the only attack against
	// the password-protected entries, so it must be expensive.
	RateLimitLoginPerMinute int
	// RateLimitLoginPerAccountPer10Min caps attempts against one account from one
	// IP over ten minutes. It is the second, slower layer: the per-minute limit
	// only slows an attacker down with a pool of addresses, while this one keeps a
	// single account from being hammered indefinitely.
	RateLimitLoginPerAccountPer10Min int
	// RateLimitAPIPerMinute is the coarse global limit for every /api/v1 route.
	// It is a blunt instrument on purpose: it stops a runaway client (or a
	// scripted scrape) from consuming the whole connection pool.
	RateLimitAPIPerMinute int

	// Per-endpoint limits added in Phase 11 (§63/§77). Each one exists because the
	// route is either expensive (a media-plane call), authorization-sensitive
	// (minting a credential) or long-lived (a socket): the coarse API limit alone
	// would let one client spend a whole classroom's budget on them.
	//
	// They share RateLimitEndpointWindow as their window, so the policy of "how
	// long is a burst" is one setting rather than four.
	RateLimitPrivateTalk    int
	RateLimitMediaToken     int
	RateLimitJoin           int
	RateLimitWSHandshake    int
	RateLimitEndpointWindow time.Duration

	// WSMaxConnectionsPerIP caps CONCURRENT business sockets per client address.
	//
	// WHY a concurrency cap in addition to a handshake rate limit: one browser
	// open on one classroom needs one socket, and a **whole classroom usually sits
	// behind one NAT address** (measured in Phase 12: 30 students + 1 teacher shared
	// one public IP). A rate limit alone still allows an attacker (or a broken
	// reconnect loop) to accumulate thousands of simultaneously open sockets, each
	// with a goroutine and a writer queue. The cap bounds the resource; the rate
	// limit bounds the churn. See docs/development/performance-test.md §9 for why
	// this must eventually be counted per account rather than per address.
	WSMaxConnectionsPerIP int

	// Windows of the pre-existing limiters. They are configuration because
	// "requests per minute" is a policy an operator may need to change without a
	// release, and because a test needs to drive expiry without sleeping a minute.
	RateLimitLoginWindow        time.Duration
	RateLimitLoginAccountWindow time.Duration
	RateLimitAPIWindow          time.Duration

	// PasswordMinLength is the minimum length for TEACHER/ADMIN passwords. Length
	// is the only password rule enforced, deliberately: composition rules push
	// people towards "Password1!" patterns, while Argon2id makes a long
	// passphrase genuinely expensive to crack.
	PasswordMinLength int

	// TrustedProxies lists the networks whose X-Forwarded-For / X-Real-IP headers
	// are believed. Empty (the default) means "believe no proxy header at all".
	//
	// WHY this default and not "trust everything": rate limiting, audit logs and
	// the sessions.ip column all key on the client address. If any caller could
	// set X-Forwarded-For, a single attacker could rotate the header to get a
	// fresh rate-limit bucket per request and to poison the audit trail. Behind a
	// real reverse proxy the operator opts in by listing that proxy's network.
	TrustedProxies []*net.IPNet
}

// LoadDotEnv best-effort loads the first readable .env file found while walking
// up from the working directory. It returns the path it loaded, or "" when no
// candidate exists. Loading never overrides variables that the real environment
// already defines, so container/CI values always win.
func LoadDotEnv() (string, error) {
	for _, path := range dotEnvSearchPaths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := godotenv.Load(path); err != nil {
			return "", fmt.Errorf("load dotenv %s: %w", path, err)
		}
		return path, nil
	}
	return "", nil
}

// Load reads, parses and validates the whole configuration.
//
// It applies defaults first, then validates, accumulating independent problems
// is intentionally NOT done: the first failure is returned immediately so the
// operator fixes configuration one step at a time instead of chasing a wall of
// cascading errors.
func Load() (*Config, error) {
	// .env is a development convenience only; a malformed one is still a bug we
	// want surfaced, an absent one is normal.
	if _, err := LoadDotEnv(); err != nil {
		return nil, err
	}

	cfg := &Config{
		AppEnv:                     envString("APP_ENV", EnvDevelopment),
		APIAddr:                    envString("API_ADDR", ":8080"),
		DatabaseURL:                envString("DATABASE_URL", ""),
		RedisAddr:                  envString("REDIS_ADDR", "localhost:6379"),
		RedisPassword:              envString("REDIS_PASSWORD", ""),
		LiveKitURL:                 envString("LIVEKIT_URL", ""),
		LiveKitAPIURL:              envString("LIVEKIT_API_URL", ""),
		LiveKitAPIKey:              envString("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret:           envString("LIVEKIT_API_SECRET", ""),
		SessionCookieName:          envString("SESSION_COOKIE_NAME", "classwatch_session"),
		StartupRequireDependencies: true,
	}

	if err := parseAppEnv(cfg); err != nil {
		return nil, err
	}
	if err := parseLogLevel(cfg); err != nil {
		return nil, err
	}
	if err := parseLogFormat(cfg); err != nil {
		return nil, err
	}
	if err := parseHTTPServer(cfg); err != nil {
		return nil, err
	}
	if err := parseCORSOrigins(cfg); err != nil {
		return nil, err
	}
	if err := parseBoolFields(cfg); err != nil {
		return nil, err
	}
	if err := parseRedis(cfg); err != nil {
		return nil, err
	}
	if err := parseSession(cfg); err != nil {
		return nil, err
	}
	if err := parsePasswordPolicy(cfg); err != nil {
		return nil, err
	}
	if err := parseRateLimits(cfg); err != nil {
		return nil, err
	}
	if err := parseEndpointRateLimits(cfg); err != nil {
		return nil, err
	}
	if err := parseMetrics(cfg); err != nil {
		return nil, err
	}
	if err := parseTrustedProxies(cfg); err != nil {
		return nil, err
	}
	if err := parseLiveKitToken(cfg); err != nil {
		return nil, err
	}
	if err := parsePostgresPool(cfg); err != nil {
		return nil, err
	}
	if err := validateRequired(cfg); err != nil {
		return nil, err
	}

	// The server-side LiveKit endpoint is a different URL than the browser
	// endpoint in Docker (http://livekit:7880 vs ws://localhost:7880), but in a
	// single-process local setup one value is enough. Falling back keeps the
	// deployment configuration small without hiding a required setting.
	if cfg.LiveKitAPIURL == "" {
		cfg.LiveKitAPIURL = cfg.LiveKitURL
	}
	// Every field above is already populated; this call is what keeps the "zero
	// value means a default, never means off" property true even if a future
	// parser forgets a field (see ApplyDefaults).
	cfg.ApplyDefaults()
	return cfg, nil
}

func parseAppEnv(cfg *Config) error {
	switch cfg.AppEnv {
	case EnvDevelopment, EnvTest, EnvProduction:
		return nil
	default:
		return fmt.Errorf("APP_ENV must be one of %q, %q, %q (got %q)",
			EnvDevelopment, EnvTest, EnvProduction, cfg.AppEnv)
	}
}

func parseLogLevel(cfg *Config) error {
	raw := envString("LOG_LEVEL", "info")
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error (got %q)", raw)
	}
	cfg.LogLevel = level
	return nil
}

// parseLogFormat reads LOG_FORMAT. The empty value is the default and means
// "decide from APP_ENV" (see LogFormat).
func parseLogFormat(cfg *Config) error {
	raw := strings.ToLower(envString("LOG_FORMAT", ""))
	switch raw {
	case "", LogFormatText, LogFormatJSON:
		cfg.LogFormat = raw
		return nil
	default:
		return fmt.Errorf("LOG_FORMAT must be one of %q, %q or empty (got %q)", LogFormatText, LogFormatJSON, raw)
	}
}

// Recognised LOG_FORMAT values.
const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

// UseJSONLogs reports whether the process must emit one JSON object per line.
func (c *Config) UseJSONLogs() bool {
	switch c.LogFormat {
	case LogFormatJSON:
		return true
	case LogFormatText:
		return false
	default:
		return c.IsProduction()
	}
}

// Defaults of the Phase 11 transport and limit settings.
//
// They live here, once, because two callers need them: parseHTTPServer (which
// applies them while reading the environment) and ApplyDefaults (which applies
// them to a Config that was built by hand — a test, a tool, or a future embedder).
// Two copies of "1 MiB" would eventually disagree, and the copy that loses is the
// one nobody reads.
const (
	defaultHTTPMaxBodyBytes       = 1 << 20 // 1 MiB
	defaultHTTPReadHeaderTimeout  = 5 * time.Second
	defaultHTTPReadTimeout        = 15 * time.Second
	defaultHTTPWriteTimeout       = 30 * time.Second
	defaultHTTPIdleTimeout        = 60 * time.Second
	defaultHTTPShutdownTimeout    = 10 * time.Second
	defaultHTTPDrainDelay         = 2 * time.Second
	defaultMetricsRefreshInterval = 15 * time.Second
	defaultEndpointWindow         = time.Minute
	defaultLoginWindow            = time.Minute
	defaultLoginAccountWindow     = 10 * time.Minute
	defaultAPIWindow              = time.Minute
	defaultRateLimitPrivateTalk   = 30
	defaultRateLimitMediaToken    = 20
	defaultRateLimitJoin          = 60
	defaultRateLimitWSHandshake   = 60
	// Phase 12 压测把这个默认值从 16 提到 64。
	//
	// WHY：一间教室（甚至一所学校）通常只有**一个 NAT 出口**，30~60 个学生共享一个公网 IP。
	// 按 IP 计数 16 条并发，等价于"第 17 个进教室的学生连不上实时通道"——
	// 实测 20 条并发里 16 成功 4 被 429。上限的作用是防止单地址堆积成千上万条 socket
	// （每条一个 goroutine + 写队列），64 仍然把资源绑得很紧，而一个班不再被误伤。
	// 真正的粒度问题（老师与全班挤同一个桶）留给"按账号计数 + IP 只做粗粒度防洪"，
	// 已记入 docs/development/performance-test.md §9。
	defaultWSMaxConnectionsPerIP = 64
)

// ApplyDefaults fills every Phase 11 transport/limit field that is still zero.
//
// WHY this exists on top of Load: Load already produces a fully populated Config,
// but Config is also constructed directly — by tests, and by any future tool that
// embeds the API. A zero window silently DISABLES a rate limiter and a zero
// timeout silently disables a slowloris guard, and "the guard was off because
// nobody set the field" is not a failure anyone notices until it is an incident.
// Filling the gaps here makes the zero value of those fields impossible to
// misread as "off".
//
// It is idempotent, so calling it twice is harmless.
func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.HTTPMaxBodyBytes <= 0 {
		c.HTTPMaxBodyBytes = defaultHTTPMaxBodyBytes
	}
	if c.HTTPReadHeaderTimeout <= 0 {
		c.HTTPReadHeaderTimeout = defaultHTTPReadHeaderTimeout
	}
	if c.HTTPReadTimeout <= 0 {
		c.HTTPReadTimeout = defaultHTTPReadTimeout
	}
	if c.HTTPWriteTimeout <= 0 {
		c.HTTPWriteTimeout = defaultHTTPWriteTimeout
	}
	if c.HTTPIdleTimeout <= 0 {
		c.HTTPIdleTimeout = defaultHTTPIdleTimeout
	}
	if c.HTTPShutdownTimeout <= 0 {
		c.HTTPShutdownTimeout = defaultHTTPShutdownTimeout
	}
	// A zero drain delay is meaningful (nothing routes here yet), so it is left
	// alone: only a negative value is nonsense.
	if c.HTTPDrainDelay < 0 {
		c.HTTPDrainDelay = 0
	}
	if c.MetricsSessionRefreshInterval <= 0 {
		c.MetricsSessionRefreshInterval = defaultMetricsRefreshInterval
	}
	if c.RateLimitEndpointWindow <= 0 {
		c.RateLimitEndpointWindow = defaultEndpointWindow
	}
	if c.RateLimitLoginWindow <= 0 {
		c.RateLimitLoginWindow = defaultLoginWindow
	}
	if c.RateLimitLoginAccountWindow <= 0 {
		c.RateLimitLoginAccountWindow = defaultLoginAccountWindow
	}
	if c.RateLimitAPIWindow <= 0 {
		c.RateLimitAPIWindow = defaultAPIWindow
	}
	if c.RateLimitPrivateTalk <= 0 {
		c.RateLimitPrivateTalk = defaultRateLimitPrivateTalk
	}
	if c.RateLimitMediaToken <= 0 {
		c.RateLimitMediaToken = defaultRateLimitMediaToken
	}
	if c.RateLimitJoin <= 0 {
		c.RateLimitJoin = defaultRateLimitJoin
	}
	if c.RateLimitWSHandshake <= 0 {
		c.RateLimitWSHandshake = defaultRateLimitWSHandshake
	}
	if c.WSMaxConnectionsPerIP <= 0 {
		c.WSMaxConnectionsPerIP = defaultWSMaxConnectionsPerIP
	}
}

// parseHTTPServer reads the §63 transport hardening knobs.
//
// The defaults are chosen for a public deployment behind a reverse proxy:
//
//   - body 1 MiB: every legitimate body in this API is a few kilobytes, and a
//     bound is what makes "send 4 GiB and watch the heap grow" impossible.
//   - ReadHeaderTimeout 5s: the slowloris window. A real client sends its headers
//     in one round trip; five seconds is generous even on a bad mobile link.
//   - ReadTimeout 15s: headers plus body. Three times the header budget, because
//     a roster import over a slow uplink is the slowest legitimate request.
//   - WriteTimeout 30s: enough for the slowest handler (a media-plane call with a
//     retry, or a monitoring poll that waits on LiveKit) and short enough that a
//     stuck client cannot hold a response forever. It does NOT cut WebSockets:
//     see the field comment.
//   - IdleTimeout 60s: a keep-alive connection is cheaper than a TLS handshake,
//     but an abandoned tab must eventually release its socket.
//   - Shutdown 10s: comfortably below the 30s SIGKILL grace period that container
//     runtimes use by default.
func parseHTTPServer(cfg *Config) error {
	body, err := envInt("HTTP_MAX_BODY_BYTES", defaultHTTPMaxBodyBytes)
	if err != nil {
		return err
	}
	// The upper bound is a sanity check, not a policy: a body limit above 64 MiB
	// means a single request can allocate more than a small instance has, which
	// defeats the point of having the limit.
	const maxBodyBytes = 64 << 20
	if body <= 0 || body > maxBodyBytes {
		return fmt.Errorf("HTTP_MAX_BODY_BYTES must be between 1 and %d (got %d)", maxBodyBytes, body)
	}
	cfg.HTTPMaxBodyBytes = int64(body)

	if cfg.HTTPReadHeaderTimeout, err = envDuration("HTTP_READ_HEADER_TIMEOUT", defaultHTTPReadHeaderTimeout); err != nil {
		return err
	}
	if cfg.HTTPReadTimeout, err = envDuration("HTTP_READ_TIMEOUT", defaultHTTPReadTimeout); err != nil {
		return err
	}
	if cfg.HTTPWriteTimeout, err = envDuration("HTTP_WRITE_TIMEOUT", defaultHTTPWriteTimeout); err != nil {
		return err
	}
	if cfg.HTTPIdleTimeout, err = envDuration("HTTP_IDLE_TIMEOUT", defaultHTTPIdleTimeout); err != nil {
		return err
	}
	if cfg.HTTPShutdownTimeout, err = envDuration("HTTP_SHUTDOWN_TIMEOUT", defaultHTTPShutdownTimeout); err != nil {
		return err
	}
	if cfg.HTTPDrainDelay, err = envNonNegativeDuration("HTTP_DRAIN_DELAY", defaultHTTPDrainDelay); err != nil {
		return err
	}
	// ReadHeaderTimeout must not exceed ReadTimeout: the whole-request deadline
	// starts at the same instant, so a larger header budget would be silently
	// truncated by the outer one — a configuration that lies about itself.
	if cfg.HTTPReadHeaderTimeout > cfg.HTTPReadTimeout {
		return fmt.Errorf("HTTP_READ_HEADER_TIMEOUT (%s) must not exceed HTTP_READ_TIMEOUT (%s)",
			cfg.HTTPReadHeaderTimeout, cfg.HTTPReadTimeout)
	}
	return nil
}

func parseMetrics(cfg *Config) error {
	interval, err := envDuration("METRICS_SESSION_REFRESH_INTERVAL", defaultMetricsRefreshInterval)
	if err != nil {
		return err
	}
	cfg.MetricsSessionRefreshInterval = interval
	return nil
}

func parseCORSOrigins(cfg *Config) error {
	raw := envString("CORS_ALLOWED_ORIGINS", "")
	origins := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		// A wildcard would let any website issue credentialed cross-origin
		// requests with the student/teacher session cookie attached. Refuse it
		// at boot instead of silently weakening CORS at runtime.
		if origin == "*" {
			return errors.New("CORS_ALLOWED_ORIGINS must list explicit origins; \"*\" is not allowed with credentials")
		}
		if u, err := url.Parse(origin); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must be an absolute origin such as https://teacher.example.com", origin)
		}
		origins = append(origins, origin)
	}
	cfg.CORSAllowedOrigins = origins
	return nil
}

func parseBoolFields(cfg *Config) error {
	autoMigrate, err := envBool("DB_AUTO_MIGRATE", false)
	if err != nil {
		return err
	}
	cfg.DBAutoMigrate = autoMigrate

	requireDeps, err := envBool("STARTUP_REQUIRE_DEPENDENCIES", true)
	if err != nil {
		return err
	}
	cfg.StartupRequireDependencies = requireDeps

	cookieSecure, err := envBool("SESSION_COOKIE_SECURE", false)
	if err != nil {
		return err
	}
	cfg.SessionCookieSecure = cookieSecure
	return nil
}

func parseRedis(cfg *Config) error {
	db, err := envInt("REDIS_DB", 0)
	if err != nil {
		return err
	}
	if db < 0 {
		return fmt.Errorf("REDIS_DB must be >= 0 (got %d)", db)
	}
	cfg.RedisDB = db
	return nil
}

func parseSession(cfg *Config) error {
	raw := envString("SESSION_TTL", "24h")
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("SESSION_TTL must be a Go duration such as 24h or 30m (got %q)", raw)
	}
	if ttl <= 0 {
		return fmt.Errorf("SESSION_TTL must be greater than zero (got %q)", raw)
	}
	cfg.SessionTTL = ttl

	touch, err := envDuration("SESSION_IDLE_TOUCH_INTERVAL", 5*time.Minute)
	if err != nil {
		return err
	}
	// A touch interval longer than the TTL would mean last_seen_at is never
	// refreshed for a session that is still alive, which makes the column
	// useless for the operational question it exists to answer.
	if touch > ttl {
		return fmt.Errorf("SESSION_IDLE_TOUCH_INTERVAL (%s) must not exceed SESSION_TTL (%s)", touch, ttl)
	}
	cfg.SessionIdleTouchInterval = touch
	return nil
}

// parsePasswordPolicy reads the password length floor.
//
// The upper bound is not configurable on purpose: MaxPasswordLength in
// internal/auth exists to bound Argon2 work, and letting an operator raise it
// would turn a login form into a memory-exhaustion vector.
func parsePasswordPolicy(cfg *Config) error {
	minLength, err := envInt("PASSWORD_MIN_LENGTH", 12)
	if err != nil {
		return err
	}
	// 8 is a hard floor rather than a suggestion: an operator can decide that 12
	// is too strict for their staff, but not that 4 is fine.
	if minLength < 8 || minLength > 128 {
		return fmt.Errorf("PASSWORD_MIN_LENGTH must be between 8 and 128 (got %d)", minLength)
	}
	cfg.PasswordMinLength = minLength
	return nil
}

func parseRateLimits(cfg *Config) error {
	login, err := envInt("RATE_LIMIT_LOGIN_PER_MINUTE", 10)
	if err != nil {
		return err
	}
	loginPerAccount, err := envInt("RATE_LIMIT_LOGIN_PER_ACCOUNT_PER_10MIN", 5)
	if err != nil {
		return err
	}
	api, err := envInt("RATE_LIMIT_API_PER_MINUTE", 300)
	if err != nil {
		return err
	}
	// Zero would silently disable a limit, and "disabled by typo" is exactly the
	// kind of security regression nobody notices. Disabling a limit must be an
	// explicit code change, not a configuration value.
	if login <= 0 {
		return fmt.Errorf("RATE_LIMIT_LOGIN_PER_MINUTE must be > 0 (got %d)", login)
	}
	if loginPerAccount <= 0 {
		return fmt.Errorf("RATE_LIMIT_LOGIN_PER_ACCOUNT_PER_10MIN must be > 0 (got %d)", loginPerAccount)
	}
	if api <= 0 {
		return fmt.Errorf("RATE_LIMIT_API_PER_MINUTE must be > 0 (got %d)", api)
	}
	cfg.RateLimitLoginPerMinute = login
	cfg.RateLimitLoginPerAccountPer10Min = loginPerAccount
	cfg.RateLimitAPIPerMinute = api
	return nil
}

// parseEndpointRateLimits reads the Phase 11 per-endpoint limits and every window
// (§63/§77).
//
// Defaults are derived from what one teacher and one student legitimately do:
//
//   - private talk: a teacher starts/stops a talk by hand, so 30/minute is already
//     an order of magnitude more than a human can produce; the limit exists to stop
//     a script, and the state machine would serialize it anyway.
//   - media token: one per join, plus a retry after a LiveKit hiccup. 20/minute per
//     address lets a whole classroom behind one NAT retry and still stops a loop
//     that mints credentials.
//   - join: 60/minute. A student joins once; a reconnecting browser retries a few
//     times. A NAT-ed classroom of 30 students sharing one egress address all join
//     within the same minute, so this one is deliberately roomy — the per-session
//     rules (§43/§50), not this limit, are what prevent abuse.
//   - ws handshake: 60/minute, for the same NAT reason (one classroom opening its
//     consoles at once is ~31 handshakes), while still bounding the realistic
//     failure mode: a reconnect loop, where each handshake costs a database session
//     lookup.
func parseEndpointRateLimits(cfg *Config) error {
	privateTalk, err := envInt("RATE_LIMIT_PRIVATE_TALK", defaultRateLimitPrivateTalk)
	if err != nil {
		return err
	}
	mediaToken, err := envInt("RATE_LIMIT_MEDIA_TOKEN", defaultRateLimitMediaToken)
	if err != nil {
		return err
	}
	join, err := envInt("RATE_LIMIT_JOIN", defaultRateLimitJoin)
	if err != nil {
		return err
	}
	ws, err := envInt("RATE_LIMIT_WS_HANDSHAKE", defaultRateLimitWSHandshake)
	if err != nil {
		return err
	}
	maxSockets, err := envInt("WS_MAX_CONNECTIONS_PER_IP", defaultWSMaxConnectionsPerIP)
	if err != nil {
		return err
	}
	// A slice, not a map: the error message must name the same variable on every
	// run, or an operator chasing a boot failure sees a different complaint each
	// time they retry.
	for _, limit := range []struct {
		name  string
		value int
	}{
		{"RATE_LIMIT_PRIVATE_TALK", privateTalk},
		{"RATE_LIMIT_MEDIA_TOKEN", mediaToken},
		{"RATE_LIMIT_JOIN", join},
		{"RATE_LIMIT_WS_HANDSHAKE", ws},
		// 16 sockets from one address is far above one browser (which opens one
		// socket per console tab) and far below what it takes to exhaust a process.
		{"WS_MAX_CONNECTIONS_PER_IP", maxSockets},
	} {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be > 0 (got %d); disabling a limit must be a code change, not a configuration value", limit.name, limit.value)
		}
	}
	cfg.RateLimitPrivateTalk = privateTalk
	cfg.RateLimitMediaToken = mediaToken
	cfg.RateLimitJoin = join
	cfg.RateLimitWSHandshake = ws
	cfg.WSMaxConnectionsPerIP = maxSockets

	if cfg.RateLimitEndpointWindow, err = envDuration("RATE_LIMIT_ENDPOINT_WINDOW", defaultEndpointWindow); err != nil {
		return err
	}
	if cfg.RateLimitLoginWindow, err = envDuration("RATE_LIMIT_LOGIN_WINDOW", defaultLoginWindow); err != nil {
		return err
	}
	if cfg.RateLimitLoginAccountWindow, err = envDuration("RATE_LIMIT_LOGIN_ACCOUNT_WINDOW", defaultLoginAccountWindow); err != nil {
		return err
	}
	if cfg.RateLimitAPIWindow, err = envDuration("RATE_LIMIT_API_WINDOW", defaultAPIWindow); err != nil {
		return err
	}
	return nil
}

// parseTrustedProxies turns TRUSTED_PROXIES into networks.
//
// Both a bare address and a CIDR are accepted ("10.0.0.5" is treated as
// 10.0.0.5/32) because operators routinely write the proxy's own address, and
// refusing it would push them towards the far worse "trust everything" habit.
// Anything unparseable is a boot failure: a typo here silently disables rate
// limiting per client, which is a security setting, not a cosmetic one.
func parseTrustedProxies(cfg *Config) error {
	raw := envString("TRUSTED_PROXIES", "")
	nets := make([]*net.IPNet, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return fmt.Errorf("TRUSTED_PROXIES entry %q is neither an IP address nor a CIDR", entry)
			}
			bits := 8 * net.IPv6len
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 8 * net.IPv4len
			}
			entry = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid CIDR: %w", part, err)
		}
		nets = append(nets, network)
	}
	cfg.TrustedProxies = nets
	return nil
}

// parseLiveKitToken validates the media token lifetime.
//
// The upper bound is not decoration: a token is a bearer credential for a media room,
// and an operator who typed "240h" instead of "2h" would hand every participant a
// valid key for a week. 24 hours is the ceiling because no lesson of this product
// lasts a day, so anything beyond it is a typo rather than a policy.
func parseLiveKitToken(cfg *Config) error {
	raw := envString("LIVEKIT_TOKEN_TTL", "2h")
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("LIVEKIT_TOKEN_TTL must be a Go duration such as 2h or 90m (got %q)", raw)
	}
	if ttl <= 0 {
		return fmt.Errorf("LIVEKIT_TOKEN_TTL must be greater than zero (got %q)", raw)
	}
	if ttl > maxLiveKitTokenTTL {
		return fmt.Errorf("LIVEKIT_TOKEN_TTL must be at most %s (got %q)", maxLiveKitTokenTTL, raw)
	}
	cfg.LiveKitTokenTTL = ttl
	return nil
}

// maxLiveKitTokenTTL is the hard ceiling for a media token's lifetime. It is a
// constant and not a configuration value: see parseLiveKitToken.
const maxLiveKitTokenTTL = 24 * time.Hour

func parsePostgresPool(cfg *Config) error {
	maxConns, err := envInt("DB_MAX_CONNS", 10)
	if err != nil {
		return err
	}
	minConns, err := envInt("DB_MIN_CONNS", 2)
	if err != nil {
		return err
	}
	if maxConns <= 0 {
		return fmt.Errorf("DB_MAX_CONNS must be > 0 (got %d)", maxConns)
	}
	if minConns < 0 || minConns > maxConns {
		return fmt.Errorf("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS=%d (got %d)", maxConns, minConns)
	}
	cfg.DBMaxConns = int32(maxConns)
	cfg.DBMinConns = int32(minConns)

	lifetime, err := envDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute)
	if err != nil {
		return err
	}
	healthCheck, err := envDuration("DB_HEALTH_CHECK_PERIOD", time.Minute)
	if err != nil {
		return err
	}
	cfg.DBMaxConnLifetime = lifetime
	cfg.DBHealthCheckPeriod = healthCheck
	return nil
}

func validateRequired(cfg *Config) error {
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required: the Control Plane database is the single Source of Truth and the API refuses to start without it")
	}
	// Validate the DSN shape here rather than at first query: a typo such as a
	// missing "postgres://" scheme would otherwise surface as an obscure pool
	// error minutes after boot. Never echo the DSN itself — it carries the
	// password — only its redacted form.
	parsed, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL is not a valid connection string (%s)", RedactDSN(cfg.DatabaseURL))
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("DATABASE_URL must use the postgres:// scheme (%s)", RedactDSN(cfg.DatabaseURL))
	}
	if parsed.Host == "" {
		return fmt.Errorf("DATABASE_URL must include host:port (%s)", RedactDSN(cfg.DatabaseURL))
	}
	if cfg.LiveKitURL == "" {
		return errors.New("LIVEKIT_URL is required: students and teachers need a media endpoint to connect to")
	}
	if cfg.LiveKitAPIKey == "" {
		return errors.New("LIVEKIT_API_KEY is required for server-to-server LiveKit calls")
	}
	if cfg.LiveKitAPISecret == "" {
		return errors.New("LIVEKIT_API_SECRET is required for server-to-server LiveKit calls; it must stay backend-only")
	}
	return nil
}

// IsProduction reports whether the API runs with production hardening rules
// (JSON logs, no verbose internals in responses).
func (c *Config) IsProduction() bool { return c.AppEnv == EnvProduction }

// CORSOrigins returns a defensive copy of the origin allowlist so callers cannot
// mutate validated configuration by accident.
func (c *Config) CORSOrigins() []string {
	out := make([]string, len(c.CORSAllowedOrigins))
	copy(out, c.CORSAllowedOrigins)
	return out
}

// SessionCookieNameFor returns the session cookie name of one entry point, e.g.
// "classwatch_session_teacher" for the teacher console.
//
// WHY the session cookie is namespaced per entry point instead of being one
// shared name: in development all three frontends are served from `localhost`,
// and cookies are scoped by host — not by port. With a single name, logging into
// the teacher console would overwrite the student console's cookie and silently
// log that student out (and vice versa). In production the three apps live on
// three domains, where the same problem appears the moment a shared parent
// domain or a single sign-on style deployment is used. Namespacing makes "one
// browser, three independent sessions" work by construction, and it also lets
// RBAC answer "which entry point is this request on?" from the cookie itself.
func (c *Config) SessionCookieNameFor(role string) string {
	return c.SessionCookieName + "_" + strings.ToLower(role)
}

// TrustedProxyCIDRs renders the trusted networks for libraries that take CIDR
// strings (gin's SetTrustedProxies). Round-tripping through net.IPNet.String is
// lossless for every network parseTrustedProxies accepts.
func (c *Config) TrustedProxyCIDRs() []string {
	if len(c.TrustedProxies) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.TrustedProxies))
	for _, n := range c.TrustedProxies {
		if n == nil {
			continue
		}
		out = append(out, n.String())
	}
	return out
}

// RedactDSN renders a PostgreSQL DSN safe for logs and error messages: user,
// host, database and non-secret options are kept because they are what makes an
// error actionable, while any password is replaced by a fixed placeholder.
//
// It is deliberately a pure string function (no parsing side effects) because it
// is also used on values that failed to parse.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return "<empty>"
	}
	u, err := url.Parse(dsn)
	// Opaque URLs and host-less URLs are not something pgx accepts. They are also
	// the shape that hides a password in a field we would otherwise print
	// verbatim (for example "not-a-dsn:secret@localhost"), so refuse to reveal
	// anything at all instead of guessing which part is secret.
	if err != nil || u.Opaque != "" || u.Host == "" {
		return "<redacted>"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), redactedPasswordPlaceholder)
		}
	}
	return u.String()
}

// redactedPasswordPlaceholder mirrors net/url.Redacted's convention: it is
// obviously not a credential, so a copied redacted DSN fails loudly instead of
// silently connecting with a bogus password.
const redactedPasswordPlaceholder = "xxxxx"

// Redact describes a secret without disclosing it. Presence is the only thing an
// operator ever needs to know from a log line.
func Redact(secret string) string {
	if secret == "" {
		return "<empty>"
	}
	return "<redacted>"
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean such as true or false (got %q)", key, raw)
	}
	return v, nil
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (got %q)", key, raw)
	}
	return v, nil
}

// envNonNegativeDuration is envDuration for a delay where ZERO is a legitimate
// value ("do not wait"), as opposed to a bound where zero would disable a guard.
func envNonNegativeDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 2s (got %q)", key, raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must not be negative (got %q)", key, raw)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 30m (got %q)", key, raw)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero (got %q)", key, raw)
	}
	return v, nil
}
