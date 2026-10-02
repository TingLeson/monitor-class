package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// clearEnv removes every variable Load understands so each test starts from a
// known-empty environment instead of inheriting the developer's shell, a CI
// secret store, or docker-compose leftovers.
func clearEnv(t *testing.T) {
	t.Helper()
	// t.Chdir also moves away from any developer .env file, which keeps Load
	// deterministic: the repository root .env is unreachable from a temp dir.
	t.Chdir(t.TempDir())

	keys := []string{
		"APP_ENV", "API_ADDR", "LOG_LEVEL", "CORS_ALLOWED_ORIGINS",
		"DB_AUTO_MIGRATE", "STARTUP_REQUIRE_DEPENDENCIES",
		"DATABASE_URL", "DB_MAX_CONNS", "DB_MIN_CONNS",
		"DB_MAX_CONN_LIFETIME", "DB_HEALTH_CHECK_PERIOD",
		"REDIS_ADDR", "REDIS_PASSWORD", "REDIS_DB",
		"LIVEKIT_URL", "LIVEKIT_API_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"LIVEKIT_TOKEN_TTL",
		"SESSION_COOKIE_NAME", "SESSION_TTL", "SESSION_COOKIE_SECURE",
		"SESSION_IDLE_TOUCH_INTERVAL", "PASSWORD_MIN_LENGTH",
		"RATE_LIMIT_LOGIN_PER_MINUTE", "RATE_LIMIT_LOGIN_PER_ACCOUNT_PER_10MIN",
		"RATE_LIMIT_API_PER_MINUTE", "TRUSTED_PROXIES",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

// setMinimalValidEnv provides the smallest environment that passes validation,
// so each test only has to override the field it is actually about.
func setMinimalValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://classwatch:pw@localhost:5432/classwatch?sslmode=disable")
	t.Setenv("LIVEKIT_URL", "ws://localhost:7880")
	t.Setenv("LIVEKIT_API_KEY", "devkey")
	t.Setenv("LIVEKIT_API_SECRET", "devsecret")
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.AppEnv != EnvDevelopment {
		t.Errorf("AppEnv = %q, want %q", cfg.AppEnv, EnvDevelopment)
	}
	if cfg.APIAddr != ":8080" {
		t.Errorf("APIAddr = %q, want %q", cfg.APIAddr, ":8080")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.SessionCookieName != "classwatch_session" {
		t.Errorf("SessionCookieName = %q, want %q", cfg.SessionCookieName, "classwatch_session")
	}
	if cfg.SessionTTL != 24*time.Hour {
		t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, 24*time.Hour)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("RedisAddr = %q, want %q", cfg.RedisAddr, "localhost:6379")
	}
	if cfg.RedisDB != 0 {
		t.Errorf("RedisDB = %d, want 0", cfg.RedisDB)
	}
	// Boot must refuse to run without dependencies unless explicitly relaxed:
	// PostgreSQL is the Source of Truth, not an optional cache.
	if !cfg.StartupRequireDependencies {
		t.Error("StartupRequireDependencies = false, want true by default")
	}
	if cfg.DBAutoMigrate {
		t.Error("DBAutoMigrate = true, want false by default")
	}
	if cfg.SessionCookieSecure {
		t.Error("SessionCookieSecure = true, want false by default")
	}
	if cfg.DBMaxConns != 10 || cfg.DBMinConns != 2 {
		t.Errorf("pool bounds = (%d, %d), want (10, 2)", cfg.DBMaxConns, cfg.DBMinConns)
	}
	if cfg.DBMaxConnLifetime != 30*time.Minute {
		t.Errorf("DBMaxConnLifetime = %v, want 30m", cfg.DBMaxConnLifetime)
	}
	if cfg.DBHealthCheckPeriod != time.Minute {
		t.Errorf("DBHealthCheckPeriod = %v, want 1m", cfg.DBHealthCheckPeriod)
	}
	// A single-host local setup must not need to repeat the LiveKit endpoint.
	if cfg.LiveKitAPIURL != "ws://localhost:7880" {
		t.Errorf("LiveKitAPIURL = %q, want it to fall back to LIVEKIT_URL", cfg.LiveKitAPIURL)
	}
	if cfg.IsProduction() {
		t.Error("IsProduction() = true for development env")
	}
}

func TestLoadRejectsInvalidAppEnv(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("APP_ENV", "staging")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error for APP_ENV=staging")
	}
	if !strings.Contains(err.Error(), "APP_ENV") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error when DATABASE_URL is missing")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Errorf("error = %q, want it to explain that DATABASE_URL is required", err)
	}
}

func TestLoadRejectsMalformedDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"missing scheme", "classwatch:pw@localhost:5432/classwatch"},
		{"wrong scheme", "mysql://classwatch:pw@localhost:3306/classwatch"},
		{"no host", "postgres:///classwatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv("DATABASE_URL", tc.dsn)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() succeeded for DATABASE_URL=%q, want error", tc.dsn)
			}
		})
	}
}

func TestLoadRequiresLiveKitSettings(t *testing.T) {
	for _, key := range []string{"LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET"} {
		t.Run(key, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv(key, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded, want error when %s is missing", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name %s", err, key)
			}
		})
	}
}

func TestLoadSessionTTL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "valid hours", raw: "24h", want: 24 * time.Hour},
		{name: "valid minutes", raw: "30m", want: 30 * time.Minute},
		{name: "not a duration", raw: "tomorrow", wantErr: true},
		{name: "missing unit", raw: "3600", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1h", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv("SESSION_TTL", tc.raw)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded for SESSION_TTL=%q, want error", tc.raw)
				}
				if !strings.Contains(err.Error(), "SESSION_TTL") {
					t.Errorf("error = %q, want it to name SESSION_TTL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if cfg.SessionTTL != tc.want {
				t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, tc.want)
			}
		})
	}
}

// TestLoadLiveKitTokenTTL pins the media token lifetime rules: a duration, strictly
// positive, and bounded by a day.
//
// WHY the upper bound is a test and not only validation code: a typo here ("240h"
// instead of "24h") would hand every participant a credential valid for a week, and
// the API would start happily. The ceiling is what turns that typo into a boot failure.
func TestLoadLiveKitTokenTTL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "hours", raw: "2h", want: 2 * time.Hour},
		{name: "a lesson length", raw: "90m", want: 90 * time.Minute},
		{name: "at the ceiling", raw: "24h", want: 24 * time.Hour},
		{name: "not a duration", raw: "two-hours", wantErr: true},
		{name: "missing unit", raw: "7200", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-30m", wantErr: true},
		{name: "beyond the ceiling", raw: "25h", wantErr: true},
		{name: "an obvious typo", raw: "240h", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv("LIVEKIT_TOKEN_TTL", tc.raw)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded for LIVEKIT_TOKEN_TTL=%q, want error", tc.raw)
				}
				if !strings.Contains(err.Error(), "LIVEKIT_TOKEN_TTL") {
					t.Errorf("error = %q, want it to name LIVEKIT_TOKEN_TTL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if cfg.LiveKitTokenTTL != tc.want {
				t.Errorf("LiveKitTokenTTL = %v, want %v", cfg.LiveKitTokenTTL, tc.want)
			}
		})
	}
}

// TestLoadLiveKitTokenTTLDefault keeps the default in one place: two hours is the
// value the media documentation promises and the value the session tests assume.
func TestLoadLiveKitTokenTTLDefault(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.LiveKitTokenTTL != 2*time.Hour {
		t.Errorf("LiveKitTokenTTL = %v, want the 2h default", cfg.LiveKitTokenTTL)
	}
}

// TestLoadErrorsNeverLeakSecrets is a security regression test, not a cosmetic
// one: configuration errors end up in logs, CI output and bug reports. A leaked
// database password or LiveKit API secret there is a full compromise of the
// Control Plane, so every failure path must render DSNs through RedactDSN.
func TestLoadErrorsNeverLeakSecrets(t *testing.T) {
	const (
		dbPassword    = "sup3r-s3cret-db-password"
		liveKitSecret = "sup3r-s3cret-livekit-api-secret"
	)

	// A DSN that cannot be parsed (control character) plus a scheme error: both
	// branches of validateRequired that mention the DSN.
	dsns := []string{
		"postgres://classwatch:" + dbPassword + "@localhost:5432/classwatch\x7f",
		"not-a-dsn:" + dbPassword + "@localhost",
		"mysql://classwatch:" + dbPassword + "@localhost:3306/classwatch",
		"postgres://classwatch:" + dbPassword + "@",
	}
	for _, dsn := range dsns {
		clearEnv(t)
		setMinimalValidEnv(t)
		t.Setenv("DATABASE_URL", dsn)

		_, err := Load()
		if err == nil {
			t.Fatalf("Load() succeeded for malformed DSN %q, want error", dsn)
		}
		if strings.Contains(err.Error(), dbPassword) {
			t.Errorf("DATABASE_URL password leaked into error: %q", err)
		}
	}

	// The same guarantee for a value that is merely missing: the message reports
	// presence, never content.
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("LIVEKIT_API_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded without LIVEKIT_API_SECRET")
	}
	if strings.Contains(err.Error(), liveKitSecret) {
		t.Errorf("LiveKit secret leaked into error: %q", err)
	}
	if strings.Contains(err.Error(), dbPassword) {
		t.Errorf("database password leaked into error: %q", err)
	}
}

func TestRedactDSNKeepsContextButMasksPassword(t *testing.T) {
	got := RedactDSN("postgres://classwatch:topsecret@db.internal:5432/classwatch?sslmode=disable")
	if strings.Contains(got, "topsecret") {
		t.Fatalf("RedactDSN leaked the password: %q", got)
	}
	for _, want := range []string{"classwatch:xxxxx@db.internal:5432", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("RedactDSN(%q) = %q, want it to still contain %q", "postgres://...", got, want)
		}
	}

	if got := RedactDSN(""); got != "<empty>" {
		t.Errorf("RedactDSN(\"\") = %q, want %q", got, "<empty>")
	}
	if got := RedactDSN("postgres://bad\x7f"); got != "<redacted>" {
		t.Errorf("RedactDSN(unparsable) = %q, want %q", got, "<redacted>")
	}
	if got := Redact(""); got != "<empty>" {
		t.Errorf("Redact(\"\") = %q, want %q", got, "<empty>")
	}
	if got := Redact("s3cret"); got != "<redacted>" {
		t.Errorf("Redact(secret) = %q, want %q", got, "<redacted>")
	}
}

func TestLoadCORSOrigins(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://localhost:5173, https://teacher.example.com ,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	got := cfg.CORSOrigins()
	want := []string{"http://localhost:5173", "https://teacher.example.com"}
	if len(got) != len(want) {
		t.Fatalf("CORSOrigins() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CORSOrigins()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// The returned slice must be a copy: a caller trimming the allowlist must not
	// mutate validated configuration.
	got[0] = "http://evil.example.com"
	if cfg.CORSOrigins()[0] != want[0] {
		t.Error("CORSOrigins() returned the internal slice; callers can corrupt config")
	}
}

func TestLoadRejectsWildcardAndRelativeCORSOrigins(t *testing.T) {
	for _, origin := range []string{"*", "teacher.example.com", "/relative"} {
		t.Run(origin, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv("CORS_ALLOWED_ORIGINS", origin)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted CORS_ALLOWED_ORIGINS=%q, want error", origin)
			}
		})
	}
}

func TestLoadProductionMode(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("APP_ENV", EnvProduction)
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("DB_AUTO_MIGRATE", "true")
	t.Setenv("SESSION_COOKIE_SECURE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() = false for APP_ENV=production")
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelWarn)
	}
	if !cfg.DBAutoMigrate || !cfg.SessionCookieSecure {
		t.Errorf("boolean fields not parsed: DBAutoMigrate=%v SessionCookieSecure=%v", cfg.DBAutoMigrate, cfg.SessionCookieSecure)
	}
}

func TestLoadRejectsInvalidBooleansAndNumbers(t *testing.T) {
	tests := []struct{ key, value string }{
		{"DB_AUTO_MIGRATE", "yes-please"},
		{"STARTUP_REQUIRE_DEPENDENCIES", "maybe"},
		{"SESSION_COOKIE_SECURE", "1.5"},
		{"REDIS_DB", "not-a-number"},
		{"DB_MAX_CONNS", "many"},
		{"DB_MIN_CONNS", "-1"},
		{"LOG_LEVEL", "verbose"},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q, want error", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error = %q, want it to name %s", err, tc.key)
			}
		})
	}
}

// TestLoadEnvOverridesDefaults proves explicit environment values always win
// over the built-in defaults, including the ones with non-string types.
func TestLoadEnvOverridesDefaults(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)
	t.Setenv("API_ADDR", "127.0.0.1:9090")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_PASSWORD", "redis-pw")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("SESSION_TTL", "90m")
	t.Setenv("SESSION_COOKIE_NAME", "cw_session")
	t.Setenv("LIVEKIT_API_URL", "http://livekit:7880")
	t.Setenv("DB_MAX_CONNS", "25")
	t.Setenv("DB_MIN_CONNS", "5")
	t.Setenv("DB_MAX_CONN_LIFETIME", "5m")
	t.Setenv("DB_HEALTH_CHECK_PERIOD", "15s")
	t.Setenv("STARTUP_REQUIRE_DEPENDENCIES", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.APIAddr != "127.0.0.1:9090" || cfg.RedisAddr != "redis:6379" {
		t.Errorf("string overrides not applied: APIAddr=%q RedisAddr=%q", cfg.APIAddr, cfg.RedisAddr)
	}
	if cfg.RedisDB != 3 || cfg.SessionTTL != 90*time.Minute || cfg.SessionCookieName != "cw_session" {
		t.Errorf("redis/session overrides not applied: db=%d ttl=%v name=%q", cfg.RedisDB, cfg.SessionTTL, cfg.SessionCookieName)
	}
	if cfg.LiveKitAPIURL != "http://livekit:7880" {
		t.Errorf("LiveKitAPIURL = %q, want the explicit override to win over the LIVEKIT_URL fallback", cfg.LiveKitAPIURL)
	}
	if cfg.DBMaxConns != 25 || cfg.DBMinConns != 5 {
		t.Errorf("pool overrides not applied: (%d, %d)", cfg.DBMaxConns, cfg.DBMinConns)
	}
	if cfg.DBMaxConnLifetime != 5*time.Minute || cfg.DBHealthCheckPeriod != 15*time.Second {
		t.Errorf("duration overrides not applied: (%v, %v)", cfg.DBMaxConnLifetime, cfg.DBHealthCheckPeriod)
	}
	if cfg.StartupRequireDependencies {
		t.Error("STARTUP_REQUIRE_DEPENDENCIES=false was ignored")
	}
}

// ---------------------------------------------------------------------------
// Phase 1 additions: session idle touch, password policy, rate limits and the
// proxy trust list.
// ---------------------------------------------------------------------------

func TestLoadPhase1Defaults(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.SessionIdleTouchInterval != 5*time.Minute {
		t.Errorf("SessionIdleTouchInterval = %v, want 5m", cfg.SessionIdleTouchInterval)
	}
	if cfg.PasswordMinLength != 12 {
		t.Errorf("PasswordMinLength = %d, want 12", cfg.PasswordMinLength)
	}
	if cfg.RateLimitLoginPerMinute != 10 {
		t.Errorf("RateLimitLoginPerMinute = %d, want 10", cfg.RateLimitLoginPerMinute)
	}
	if cfg.RateLimitLoginPerAccountPer10Min != 5 {
		t.Errorf("RateLimitLoginPerAccountPer10Min = %d, want 5", cfg.RateLimitLoginPerAccountPer10Min)
	}
	if cfg.RateLimitAPIPerMinute != 300 {
		t.Errorf("RateLimitAPIPerMinute = %d, want 300", cfg.RateLimitAPIPerMinute)
	}
	// The safe default is "trust no proxy header": a deployment behind a proxy
	// must opt in explicitly, and a deployment that is not must never believe a
	// client-supplied address.
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty by default", cfg.TrustedProxies)
	}
	if got := cfg.TrustedProxyCIDRs(); got != nil {
		t.Errorf("TrustedProxyCIDRs() = %v, want nil when nothing is trusted", got)
	}
}

func TestSessionCookieNameIsPerEntryPoint(t *testing.T) {
	clearEnv(t)
	setMinimalValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	// Cookies are scoped by host, not port: distinct names are what let a browser
	// hold the student, teacher and admin sessions at the same time.
	for role, want := range map[string]string{
		"STUDENT": "classwatch_session_student",
		"TEACHER": "classwatch_session_teacher",
		"ADMIN":   "classwatch_session_admin",
	} {
		if got := cfg.SessionCookieNameFor(role); got != want {
			t.Errorf("SessionCookieNameFor(%q) = %q, want %q", role, got, want)
		}
	}
}

func TestLoadPhase1NumericSettings(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"password minimum accepted", "PASSWORD_MIN_LENGTH", "16", false},
		{"password minimum below the floor", "PASSWORD_MIN_LENGTH", "4", true},
		{"password minimum above the ceiling", "PASSWORD_MIN_LENGTH", "500", true},
		{"password minimum not a number", "PASSWORD_MIN_LENGTH", "twelve", true},
		{"login limit accepted", "RATE_LIMIT_LOGIN_PER_MINUTE", "20", false},
		{"login limit zero", "RATE_LIMIT_LOGIN_PER_MINUTE", "0", true},
		{"login limit negative", "RATE_LIMIT_LOGIN_PER_MINUTE", "-5", true},
		{"per-account limit zero", "RATE_LIMIT_LOGIN_PER_ACCOUNT_PER_10MIN", "0", true},
		{"api limit zero", "RATE_LIMIT_API_PER_MINUTE", "0", true},
		{"api limit not a number", "RATE_LIMIT_API_PER_MINUTE", "many", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setMinimalValidEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded for %s=%s, want error", tc.key, tc.value)
				}
				if !strings.Contains(err.Error(), tc.key) {
					t.Errorf("error = %q, want it to name %s", err, tc.key)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
		})
	}
}

func TestLoadSessionIdleTouchInterval(t *testing.T) {
	t.Run("must not exceed the TTL", func(t *testing.T) {
		clearEnv(t)
		setMinimalValidEnv(t)
		t.Setenv("SESSION_TTL", "10m")
		t.Setenv("SESSION_IDLE_TOUCH_INTERVAL", "1h")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted an idle touch interval longer than the session TTL")
		}
		if !strings.Contains(err.Error(), "SESSION_IDLE_TOUCH_INTERVAL") {
			t.Errorf("error = %q, want it to name SESSION_IDLE_TOUCH_INTERVAL", err)
		}
	})

	t.Run("parsed when valid", func(t *testing.T) {
		clearEnv(t)
		setMinimalValidEnv(t)
		t.Setenv("SESSION_IDLE_TOUCH_INTERVAL", "90s")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned error: %v", err)
		}
		if cfg.SessionIdleTouchInterval != 90*time.Second {
			t.Errorf("SessionIdleTouchInterval = %v, want 90s", cfg.SessionIdleTouchInterval)
		}
	})

	t.Run("rejects a non-positive duration", func(t *testing.T) {
		clearEnv(t)
		setMinimalValidEnv(t)
		t.Setenv("SESSION_IDLE_TOUCH_INTERVAL", "0s")

		if _, err := Load(); err == nil {
			t.Fatal("Load() accepted SESSION_IDLE_TOUCH_INTERVAL=0s")
		}
	})
}

func TestLoadTrustedProxies(t *testing.T) {
	t.Run("accepts CIDRs and bare addresses", func(t *testing.T) {
		clearEnv(t)
		setMinimalValidEnv(t)
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.0.1 , 2001:db8::/32")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() returned error: %v", err)
		}
		if len(cfg.TrustedProxies) != 3 {
			t.Fatalf("TrustedProxies = %v, want 3 networks", cfg.TrustedProxies)
		}
		// A bare address means "exactly this host", not "the whole internet".
		if got := cfg.TrustedProxies[1].String(); got != "192.168.0.1/32" {
			t.Errorf("bare address became %q, want 192.168.0.1/32", got)
		}
		cidrs := cfg.TrustedProxyCIDRs()
		if len(cidrs) != 3 || cidrs[0] != "10.0.0.0/8" {
			t.Errorf("TrustedProxyCIDRs() = %v", cidrs)
		}
	})

	t.Run("rejects a typo", func(t *testing.T) {
		clearEnv(t)
		setMinimalValidEnv(t)
		// A silent typo here would disable per-client rate limiting, so it must be
		// a boot failure rather than a warning.
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8,not-an-ip")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted an invalid TRUSTED_PROXIES entry")
		}
		if !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
			t.Errorf("error = %q, want it to name TRUSTED_PROXIES", err)
		}
	})
}
