package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
)

const (
	// headerRequestID is both read (to continue a trace started by a proxy or the
	// frontend) and written, so a user can quote one id from a failing screen.
	headerRequestID = "X-Request-Id"
	// maxRequestIDLength bounds a client-supplied id: it is echoed into a response
	// header and into every log line, so an unbounded value is a log-flooding and
	// header-injection vector.
	maxRequestIDLength = 128
)

// RequestIDMiddleware assigns every request a correlation id.
//
// An incoming X-Request-Id is honoured so a gateway or the frontend can start the
// trace; it is validated first, because the value is echoed back in a header and
// written into structured logs. Anything non-printable, over-long or empty is
// replaced by a fresh UUID rather than trusted.
func RequestIDMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := sanitizeRequestID(c.GetHeader(headerRequestID))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		c.Writer.Header().Set(headerRequestID, requestID)
		setRequestContext(c, requestID, logging.WithRequestID(logger, requestID))
		c.Next()
	}
}

// AccessLogMiddleware emits one structured line per request.
//
// It runs after the handler so status and duration are known. Health probes are
// demoted to Debug (see logging.LevelFromRequest): an orchestrator probing every
// few seconds would otherwise bury every real request.
func AccessLogMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.Request.URL.Path
		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"duration_ms", logging.DurationMillis(time.Since(start)),
			"remote_ip", c.ClientIP(),
			"response_bytes", c.Writer.Size(),
		}
		if len(c.Errors) > 0 {
			// gin.Errors holds handler-level annotations; the full cause is logged
			// by RespondError, so only the count belongs here.
			attrs = append(attrs, "handler_errors", len(c.Errors))
		}

		level := logging.LevelFromRequest(path)
		// A 5xx is never routine, even on /healthz: it means the process cannot
		// serve and an operator must see it without lowering the log level.
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		l := LoggerFrom(c)
		if !l.Enabled(c.Request.Context(), level) {
			return
		}
		l.Log(c.Request.Context(), level, "http request", attrs...)
	}
}

// RecoveryMiddleware turns a panic into a normal error response.
//
// The recovered value and the stack are logged server-side only. The client gets
// the same INTERNAL envelope as any other failure, and the request body is never
// echoed back: a panic while handling a login or a media-token request would
// otherwise reflect credentials into the response and into the logs (§59).
func RecoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A broken pipe / client disconnect is not a server bug and must not be
			// reported as one; gin uses this exact sentinel.
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}

			l := LoggerFrom(c)
			l.Error("panic recovered",
				"panic", recovered,
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				// The stack identifies the code, which is what has to change.
				"stack", string(debug.Stack()),
			)
			// Headers may already be flushed; AbortWithStatusJSON then only records
			// the status, which is the best that can be done at this point.
			RespondError(c, apperr.New(apperr.CodeInternal))
		}()
		c.Next()
	}
}

// CORSMiddleware enforces a strict origin allowlist.
//
// WHY an allowlist instead of "*": this API authenticates with a cookie, and
// browsers refuse credentials together with a wildcard. The only ways to make
// cross-origin cookie auth work are "echo the request origin" or "echo an
// allowlisted origin" — and the first one lets any website on the internet issue
// authenticated requests with the student's or teacher's session attached (§63).
// So the origin is echoed only when it is on the list, and Vary: Origin is always
// set so no cache can serve one origin's response to another.
//
// The allowlist is configuration, never code: production values come from
// CORS_ALLOWED_ORIGINS and "*" is rejected at boot (see config.Load).
func CORSMiddleware(origins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			// Not a browser cross-origin request: curl, health probes and
			// server-to-server calls carry no Origin and need no CORS headers.
			c.Next()
			return
		}

		// Always vary, on both the allowed and the rejected path, or a shared cache
		// could hand an allowlisted origin's response to a hostile one.
		c.Writer.Header().Add("Vary", "Origin")

		_, ok := allowed[origin]
		if !ok {
			isPreflight := c.Request.Method == http.MethodOptions
			// A disallowed preflight is refused outright: answering it would tell the
			// browser the request may proceed.
			//
			// A disallowed *state-changing* request is also refused. CORS alone would
			// let the browser block the response while the server still executed the
			// side effect, which is cross-site request forgery. Safe methods (GET,
			// HEAD) are allowed through without CORS headers, because the browser
			// blocks the response anyway and some of those paths are public probes.
			if isPreflight || !isSafeMethod(c.Request.Method) {
				RespondError(c, crossOriginRejected(origin))
				return
			}
			c.Next()
			return
		}

		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		// Without this, the frontend cannot read the correlation id it needs for
		// support requests.
		h.Set("Access-Control-Expose-Headers", headerRequestID)

		if c.Request.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			requestedHeaders := c.GetHeader("Access-Control-Request-Headers")
			if requestedHeaders == "" {
				requestedHeaders = "Content-Type, X-Request-Id"
			}
			// Echo the requested headers after filtering: an unfiltered echo would
			// let a page negotiate headers the API never intended to accept.
			h.Set("Access-Control-Allow-Headers", sanitizeRequestedHeaders(requestedHeaders))
			// Cache the preflight so a chatty SPA does not double its request count.
			h.Set("Access-Control-Max-Age", "600")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// crossOriginRejected builds a 403 for an origin outside the allowlist. The
// offending origin is included because it is a request header the caller already
// knows, and it is the single most useful fact when debugging a misconfigured
// deployment — it is not a secret.
func crossOriginRejected(origin string) *apperr.Error {
	err := apperr.New(apperr.CodeInvalidRequest)
	err.Status = http.StatusForbidden
	err.Message = "Origin " + origin + " is not allowed to call this API."
	return err
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// sanitizeRequestID keeps only characters that are safe in a header and a JSON
// log field. Anything else means the caller sent something unexpected, and the
// safest response is to ignore it and mint a fresh id.
func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxRequestIDLength {
		return ""
	}
	for _, r := range raw {
		// Only visible, non-space ASCII. That excludes CR/LF — log forgery and
		// header injection — and also spaces, which are legal in an HTTP field but
		// have no business in an identifier: they would let a caller smuggle
		// something that reads like extra log content.
		if r >= 0x21 && r <= 0x7e {
			continue
		}
		return ""
	}
	return raw
}

// sanitizeRequestedHeaders filters the Access-Control-Request-Headers echo down
// to a conservative character set.
func sanitizeRequestedHeaders(raw string) string {
	const maxLen = 256
	if len(raw) > maxLen {
		raw = raw[:maxLen]
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ',':
			b.WriteString(", ")
		default:
			// Drop everything else: the caller does not need it and we do not
			// trust it.
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "Content-Type, X-Request-Id"
	}
	return out
}
