package logging

import (
	"context"
	"log/slog"
	"strings"
)

// This file is the safety net for §59's "must never be logged" list.
//
// The rule is stated in the package doc, but a rule that depends on every call
// site remembering it is a rule that will eventually be broken by someone
// debugging at 2am ("just log the whole request, I'll remove it later"). So the
// logger itself refuses to carry the value of a field whose NAME says it is a
// credential: password, token, cookie, secret, API key, DSN.
//
// # What this can and cannot do
//
// It cannot stop a secret that was interpolated into a MESSAGE
// (log.Info("login failed for " + password)) or into a non-suspicious field
// (`"payload"`, `"body"`). Those cases are why §59 also requires discipline at
// the call site and why bodies are never logged wholesale. What it does guarantee
// is that the mundane mistake — `"token", token`, `"cookie", raw`,
// `"api_secret", secret` — produces a redacted line instead of a leaked one.

// sensitiveNameMarkers are substrings that mark a field name as credential-bearing.
//
// The comparison normalises case, underscores and dashes away, so `API-Secret`,
// `api_secret` and `apiSecret` all match. Over-matching is deliberate: redacting
// `token_ttl` (a duration) loses a little debugging value, while missing
// `livekit_token` puts a credential in a log file forever.
var sensitiveNameMarkers = []string{
	"password",
	"passwd",
	"pwd",
	"secret",
	"token",
	"cookie",
	"authorization",
	"credential",
	"bearer",
	"jwt",
	"apikey",
	"privatekey",
	"signature",
	"dsn",
	"databaseurl",
	"connectionstring",
}

// redactedValue is the placeholder. It is deliberately not a hash or a prefix of
// the real value: a partial secret is still a secret, and `xxxxx` (the same
// placeholder net/url.Redacted uses) is obviously not a credential.
const redactedValue = "<redacted>"

// IsSensitiveName reports whether a log field name must never carry its value.
//
// It is exported because the package's tests state the rule, and because a caller
// that needs to log a boolean about a secret ("was it configured?") can ask
// whether the name it is about to use is one of the sensitive ones.
func IsSensitiveName(name string) bool {
	normalized := normalizeName(name)
	if normalized == "" {
		return false
	}
	for _, marker := range sensitiveNameMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// normalizeName lower-cases a field name and drops separators, so the marker
// match is insensitive to the naming style of the call site.
func normalizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// Separators (_, -, .) and anything else are dropped.
		}
	}
	return b.String()
}

// redactAttr replaces the value of a sensitive attribute, recursing into groups.
//
// A group is descended into rather than dropped: `slog.Group("livekit",
// slog.String("api_secret", s))` is a natural way to log, and dropping the whole
// group would hide the non-sensitive fields next to it.
func redactAttr(attr slog.Attr) slog.Attr {
	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		out := make([]slog.Attr, 0, len(group))
		for _, inner := range group {
			out = append(out, redactAttr(inner))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(out...)}
	}
	if IsSensitiveName(attr.Key) {
		return slog.String(attr.Key, redactedValue)
	}
	return attr
}

// RedactingHandler wraps a slog.Handler and filters credential-bearing fields.
//
// It is installed by New for every environment, including development: a leaked
// secret in a developer's terminal ends up in the same ticket as one leaked in
// production.
type RedactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler wraps inner.
func NewRedactingHandler(inner slog.Handler) slog.Handler {
	if inner == nil {
		return nil
	}
	return &RedactingHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *RedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	filtered := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		filtered.AddAttrs(redactAttr(attr))
		return true
	})
	return h.inner.Handle(ctx, filtered)
}

// WithAttrs implements slog.Handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		filtered = append(filtered, redactAttr(attr))
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(filtered)}
}

// WithGroup implements slog.Handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name)}
}

// SensitiveQueryParams is the set of query-parameter names whose VALUE must not
// reach an access log.
//
// WHY the access log may carry a query string at all: `?status=ONLINE&limit=50`
// is exactly the context that makes a log line actionable, and refusing to log
// any query would push an operator towards logging the whole URL somewhere worse.
// The rule is therefore "log the shape, redact the credential": the parameter
// NAME is kept (so the request is still diagnosable) and its value is replaced.
//
// A WebSocket or media token in a query string is the case that matters most:
// §47 forbids putting one there at all, but a proxy, an old client or a future
// endpoint can still try, and the log must not be the place it lands.
var SensitiveQueryParams = map[string]struct{}{
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"id_token":      {},
	"code":          {},
	"password":      {},
	"passwd":        {},
	"pwd":           {},
	"secret":        {},
	"client_secret": {},
	"api_key":       {},
	"apikey":        {},
	"key":           {},
	"signature":     {},
	"sig":           {},
	"jwt":           {},
	"authorization": {},
	"cookie":        {},
	"session":       {},
	"sessionid":     {},
	"session_id":    {},
}

// SanitizeQuery renders a request's RawQuery with credential values replaced.
//
// The parameter order and every non-sensitive value are preserved, because the
// value of an access log is being able to read what the client asked for. The
// output is bounded, so a caller cannot use a megabyte-long query string to fill
// the log with one line.
func SanitizeQuery(rawQuery string) string {
	const maxSanitizedQueryLength = 512
	if rawQuery == "" {
		return ""
	}
	truncated := false
	if len(rawQuery) > maxSanitizedQueryLength {
		rawQuery = rawQuery[:maxSanitizedQueryLength]
		truncated = true
	}

	parts := strings.Split(rawQuery, "&")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, _, hasValue := strings.Cut(part, "=")
		normalized := strings.ToLower(name)
		if _, sensitive := SensitiveQueryParams[normalized]; sensitive || IsSensitiveName(name) {
			if hasValue {
				out = append(out, name+"="+redactedValue)
			} else {
				out = append(out, name)
			}
			continue
		}
		out = append(out, part)
	}
	result := strings.Join(out, "&")
	if truncated {
		result += "&…"
	}
	return result
}
