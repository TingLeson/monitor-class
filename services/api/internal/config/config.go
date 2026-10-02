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
	if err := parseTrustedProxies(cfg); err != nil {
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
