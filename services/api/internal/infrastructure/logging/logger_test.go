package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
)

// These tests are the executable form of §59's "must never be logged" list. A
// secret that reaches a log file stays there: it is copied into tickets, shipped
// to a log aggregator and read by people who never had access to it.

func testConfig(env, format string, level slog.Level) *config.Config {
	return &config.Config{AppEnv: env, LogFormat: format, LogLevel: level}
}

func TestNewEmitsOneJSONLinePerRecord(t *testing.T) {
	var out bytes.Buffer
	logger := New(testConfig(config.EnvProduction, "", slog.LevelInfo), &out)

	logger.Info("http request", "method", "GET", "status", 200)
	logger.Warn("readiness degraded")

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d is not JSON: %v (%q)", i, err, line)
		}
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["msg"] != "http request" || first["method"] != "GET" {
		t.Errorf("first line = %v", first)
	}
}

func TestLogFormatOverridesAppEnv(t *testing.T) {
	var out bytes.Buffer
	logger := New(testConfig(config.EnvDevelopment, config.LogFormatJSON, slog.LevelInfo), &out)
	logger.Info("hello")
	if !json.Valid([]byte(strings.TrimSpace(out.String()))) {
		t.Errorf("LOG_FORMAT=json in development must emit JSON, got %q", out.String())
	}

	out.Reset()
	logger = New(testConfig(config.EnvProduction, config.LogFormatText, slog.LevelInfo), &out)
	logger.Info("hello", "k", "v")
	if json.Valid([]byte(strings.TrimSpace(out.String()))) {
		t.Errorf("LOG_FORMAT=text in production must emit text, got %q", out.String())
	}
	if !strings.Contains(out.String(), "k=v") {
		t.Errorf("text handler output = %q", out.String())
	}
}

func TestRedactingHandlerRemovesSensitiveValues(t *testing.T) {
	const (
		password = "correct-horse-battery-staple"
		token    = "eyJhbGciOiJIUzI1NiJ9.SECRETPART.SIGNATURE"
		cookie   = "classwatch_session_teacher=8f14e45f-ea0b-4b1f-9a4d-1b2c3d4e5f60"
		apiKey   = "devkey"
		apiSec   = "classwatch_dev_livekit_secret_32b"
		dsn      = "postgres://classwatch:anotherpassword@localhost:5432/classwatch"
	)

	var out bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewJSONHandler(&out, nil)))

	logger.Info("login attempt",
		"user_id", "8f14e45f-ea0b-4b1f-9a4d-1b2c3d4e5f60",
		"password", password,
		"token", token,
		"Cookie", cookie,
		"api_secret", apiSec,
		"livekit_api_key", apiKey,
		"database_url", dsn,
		slog.Group("auth", slog.String("access_token", token), slog.String("role", "teacher")),
	)
	// The same via a child logger, which is how request-scoped fields are attached.
	logger.With("media_token", token, "request_id", "abc-123").Info("issued media token")

	body := out.String()
	for name, secret := range map[string]string{
		"password":     password,
		"token":        token,
		"cookie":       cookie,
		"api secret":   apiSec,
		"livekit key":  apiKey,
		"database url": dsn,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("%s leaked into the log:\n%s", name, body)
		}
	}
	if !strings.Contains(body, redactedValue) {
		t.Errorf("expected %s placeholders:\n%s", redactedValue, body)
	}
	// Non-sensitive fields must survive: a redactor that eats everything is as
	// useless as one that eats nothing.
	for _, want := range []string{"8f14e45f-ea0b-4b1f-9a4d-1b2c3d4e5f60", "abc-123", `"role":"teacher"`} {
		if !strings.Contains(body, want) {
			t.Errorf("non-sensitive value %q was removed:\n%s", want, body)
		}
	}
}

func TestIsSensitiveName(t *testing.T) {
	sensitive := []string{
		"password", "PASSWORD", "user_password", "passwd", "pwd",
		"token", "access_token", "accessToken", "media-token", "livekit_token",
		"cookie", "Cookie", "set_cookie",
		"api_secret", "clientSecret", "LIVEKIT_API_SECRET",
		"authorization", "Authorization",
		"api_key", "apiKey", "livekit_api_key",
		"database_url", "dsn", "jwt", "signature", "credential",
	}
	for _, name := range sensitive {
		if !IsSensitiveName(name) {
			t.Errorf("IsSensitiveName(%q) = false, want true", name)
		}
	}

	safe := []string{
		"request_id", "user_id", "role", "classroom_id", "run_id", "session_id",
		"event", "status", "method", "route", "duration_ms", "remote_ip",
	}
	for _, name := range safe {
		if IsSensitiveName(name) {
			t.Errorf("IsSensitiveName(%q) = true, want false: §59 lists this field as expected", name)
		}
	}
}

func TestSanitizeQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"empty", "", ""},
		{"nothing sensitive", "status=ONLINE&limit=50", "status=ONLINE&limit=50"},
		{"token value", "token=eyJhbGciOi.SECRET&room=abc", "token=<redacted>&room=abc"},
		{"session id", "session_id=8f14e45f&page=2", "session_id=<redacted>&page=2"},
		{"api key", "api_key=devkey", "api_key=<redacted>"},
		{"key without value", "token&page=2", "token&page=2"},
		{"case insensitive name", "Access_Token=SECRET", "Access_Token=<redacted>"},
		{"unknown param survives", "q=hello%20world", "q=hello%20world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeQuery(tc.query); got != tc.want {
				t.Fatalf("SanitizeQuery(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}

	t.Run("bounded", func(t *testing.T) {
		query := "q=" + strings.Repeat("a", 4000)
		got := SanitizeQuery(query)
		if len(got) > 600 {
			t.Fatalf("sanitized query is %d bytes; the access log must not carry a megabyte", len(got))
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("truncation is not visible: %q", got[len(got)-10:])
		}
	})
}

func TestRedactDescribesPresenceOnly(t *testing.T) {
	if got := Redact("livekit_api_secret", "super-secret").Value.String(); got != "<redacted>" {
		t.Errorf("Redact(secret) = %q", got)
	}
	if got := Redact("livekit_api_secret", "").Value.String(); got != "<empty>" {
		t.Errorf("Redact(\"\") = %q", got)
	}
}

func TestRedactingHandlerKeepsLevelsAndContext(t *testing.T) {
	var out bytes.Buffer
	handler := NewRedactingHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logger := slog.New(handler)

	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("a filtered handler must forward Enabled to the inner handler")
	}
	logger.WarnContext(context.Background(), "careful", "password", "x")

	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &decoded); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out.String())
	}
	if decoded["level"] != "WARN" || decoded["msg"] != "careful" || decoded["password"] != redactedValue {
		t.Errorf("decoded = %v", decoded)
	}
}

func TestWithGroupRedactsNestedAttributes(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewJSONHandler(&out, nil))).WithGroup("livekit")
	logger.Info("probe", "api_secret", "SECRETVALUE", "url", "wss://example")

	if strings.Contains(out.String(), "SECRETVALUE") {
		t.Fatalf("grouped secret leaked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "wss://example") {
		t.Fatalf("grouped non-secret was dropped:\n%s", out.String())
	}
}

func TestDurationMillis(t *testing.T) {
	if got := DurationMillis(1500 * time.Millisecond); got != 1500 {
		t.Fatalf("DurationMillis = %d", got)
	}
}
