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

	// SessionCookieName is the name of the HttpOnly session cookie.
	SessionCookieName string
	// SessionTTL is the absolute session lifetime. Must parse and be > 0.
	SessionTTL time.Duration
	// SessionCookieSecure forces the Secure cookie attribute; must be true in
	// production (HTTPS-only).
	SessionCookieSecure bool
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
