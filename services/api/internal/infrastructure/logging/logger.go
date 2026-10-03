// Package logging builds the process-wide structured logger.
//
// The logger is a plain *slog.Logger. That is deliberate: the standard library
// handler covers JSON, levels and context without adding zap/logrus or a
// vendor-specific context type every layer would then have to import.
//
// # What must never be logged
//
// Session cookies, passwords, LiveKit API secrets and full auth or media tokens
// are forbidden in log lines (§59). Two habits make that rule survivable:
//
//   - Log Control Plane identifiers (the UUIDs below), never credentials and
//     never a media-plane identity that maps back to a person's real name
//     (§8/§44). LiveKit identities are opaque UUIDs for exactly this reason.
//   - When the question is "was this secret configured at all?", use Redact:
//     it answers presence without ever emitting the value.
//
// Request and response bodies must not be logged wholesale either — login
// payloads and media tokens travel through them.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
)

// Field names shared across the codebase. Centralising them is what makes log
// queries work later: a typo at one call site would silently create a second,
// sparsely populated field.
const (
	FieldRequestID   = "request_id"
	FieldUserID      = "user_id"
	FieldRole        = "role"
	FieldClassroomID = "classroom_id"
	FieldRunID       = "run_id"
	FieldSessionID   = "session_id"
)

// New returns the process logger.
//
// Production emits JSON so log shippers can index fields without regexes;
// development and test emit text because a human is reading them in a terminal.
// The shape is decided by LOG_FORMAT when it is set, and otherwise by APP_ENV, so
// raising verbosity locally never accidentally changes the machine-readable shape
// in production, while an operator who wants JSON in staging can say so.
//
// Every handler is wrapped in a RedactingHandler: the "must never be logged" list
// of §59 is enforced by the logger, not only by the discipline of its callers.
func New(cfg *config.Config, out io.Writer) *slog.Logger {
	if out == nil {
		out = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	var handler slog.Handler
	if cfg.UseJSONLogs() {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(NewRedactingHandler(handler))
}

// Redact builds an attribute for a value that must never appear in a log line:
// passwords, the LiveKit API secret, full tokens, session cookies.
//
// The failure mode it prevents is mundane — someone debugging "why is LiveKit
// rejecting us?" logs the secret once — and the consequence is permanent, since
// log files are copied into tickets and CI artifacts. Presence is the only fact
// an operator needs from a log; the value belongs in the secret store.
func Redact(key, secret string) slog.Attr {
	if secret == "" {
		return slog.String(key, "<empty>")
	}
	return slog.String(key, "<redacted>")
}

// WithRequestID returns a logger whose lines all carry the request id. The id is
// generated once per HTTP request and echoed in the X-Request-Id header, which
// is the only way to correlate what a user saw with what the server did.
func WithRequestID(l *slog.Logger, requestID string) *slog.Logger {
	return with(l, FieldRequestID, requestID)
}

// WithUserID attaches the authenticated account id (UUID, never a name).
func WithUserID(l *slog.Logger, userID string) *slog.Logger {
	return with(l, FieldUserID, userID)
}

// WithRole attaches the caller's role so RBAC denials stay auditable.
func WithRole(l *slog.Logger, role string) *slog.Logger {
	return with(l, FieldRole, role)
}

// WithClassroomID attaches the classroom (domain entity) id.
func WithClassroomID(l *slog.Logger, classroomID string) *slog.Logger {
	return with(l, FieldClassroomID, classroomID)
}

// WithRunID attaches the classroom run id — the id of one OPEN period, and the
// entity a LiveKit room name is derived from.
func WithRunID(l *slog.Logger, runID string) *slog.Logger {
	return with(l, FieldRunID, runID)
}

// WithSessionID attaches the student/teacher session id. A session is the unit
// the teacher console renders, so this is the field that links logs back to what
// the teacher actually saw on screen.
func WithSessionID(l *slog.Logger, sessionID string) *slog.Logger {
	return with(l, FieldSessionID, sessionID)
}

// LevelFromRequest maps a request path to its access-log level.
//
// Health probes are called every few seconds by the orchestrator and would
// otherwise drown every useful line at Info; demoting exactly those two paths
// keeps them available at Debug without hiding real traffic.
func LevelFromRequest(path string) slog.Level {
	if path == "/healthz" || path == "/readyz" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// DurationMillis converts a duration to the integer millisecond field used in
// access logs. Integers keep dashboards from disagreeing about rounding.
func DurationMillis(d time.Duration) int64 { return d.Milliseconds() }

// ContextWithLogger stores l in ctx so asynchronous work spawned from a request
// keeps the originating request id.
func ContextWithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext returns the request-scoped logger, falling back to the default.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// SetupDefault installs l as the slog default so libraries calling slog.Default
// share one destination and one format.
func SetupDefault(l *slog.Logger) { slog.SetDefault(l) }

// LogStartup writes the single line every deployment needs to confirm what is
// running: identifiers and addresses only, never credentials.
func LogStartup(l *slog.Logger, cfg *config.Config, version, commit string) {
	l.Info("classwatch api starting",
		"service", "classwatch-api",
		"env", cfg.AppEnv,
		"addr", cfg.APIAddr,
		"version", version,
		"commit", commit,
		"db_auto_migrate", cfg.DBAutoMigrate,
		"startup_require_dependencies", cfg.StartupRequireDependencies,
	)
}

func with(l *slog.Logger, key, value string) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	if strings.TrimSpace(value) == "" {
		// An empty identifier is worse than none: it would make aggregated
		// queries group unrelated requests together.
		return l
	}
	return l.With(key, value)
}

type loggerKey struct{}
