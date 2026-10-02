package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
)

// Context keys set by the middleware. They are unexported constants so no other
// package can accidentally write a value that collides with requestIdKey and
// silently corrupts every log line and response body.
const (
	requestIDContextKey = "classwatch.request_id"
	loggerContextKey    = "classwatch.logger"
)

// errorEnvelope is the only error shape this API ever returns.
//
// The shape is fixed on purpose. A frontend must be able to write one error
// handler: read error.code, branch on it, show error.message. Anything that
// breaks that — a bare string, a nested "details" object, an HTML error page
// from a proxy — turns a handled case into "something went wrong".
type errorEnvelope struct {
	Error     errorDetail `json:"error"`
	RequestID string      `json:"requestId"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RespondError converts any error into the standard failure response.
//
// Two things happen here and their order matters:
//
//  1. The internal cause is logged in full, server-side, with the request id.
//  2. The client receives only the business code and a safe message.
//
// A raw error must never reach the wire: driver errors contain SQL and DSN
// fragments, and LiveKit errors contain room names and endpoints (§58/§63).
// Anything not already an *apperr.Error is collapsed to INTERNAL so a new call
// site cannot leak by forgetting to wrap.
func RespondError(c *gin.Context, err error) {
	appErr := apperr.From(err)
	if appErr == nil {
		appErr = apperr.New(apperr.CodeInternal)
	}

	attrs := []any{"code", string(appErr.Code), "status", appErr.Status}
	// The cause is the only place internal detail is allowed to appear, and only
	// when there is one: an empty cause= field is noise in every log query.
	if appErr.Err != nil {
		attrs = append(attrs, "cause", appErr.Err.Error())
	}

	if appErr.Status >= http.StatusInternalServerError {
		LoggerFrom(c).Error("request failed", attrs...)
	} else {
		// 4xx are expected outcomes (a closed classroom, a missing session), not
		// incidents. Logging them at Error would train everyone to ignore Error.
		LoggerFrom(c).Info("request rejected", attrs...)
	}

	c.AbortWithStatusJSON(appErr.Status, errorEnvelope{
		Error: errorDetail{
			Code:    string(appErr.Code),
			Message: appErr.Message,
		},
		RequestID: RequestIDFrom(c),
	})
}

// RespondErrorCode is shorthand for the common "reject with this code" case.
func RespondErrorCode(c *gin.Context, code apperr.Code) {
	RespondError(c, apperr.New(code))
}

// RespondJSON writes a success response.
//
// Success payloads are NOT wrapped in an envelope: only errors need a stable
// machine-readable shape, and wrapping every payload would force the frontend to
// unwrap twice for no benefit.
func RespondJSON(c *gin.Context, status int, payload any) {
	c.JSON(status, payload)
}

// RequestIDFrom returns the id assigned to this request, or "" when the
// middleware has not run (only possible in tests that call handlers directly).
func RequestIDFrom(c *gin.Context) string {
	if v, ok := c.Get(requestIDContextKey); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// LoggerFrom returns the request-scoped logger carrying request_id.
func LoggerFrom(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(loggerContextKey); ok {
		if l, ok := v.(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}

// setRequestContext stores the identifiers every subsequent log line and error
// response needs. It also mirrors the logger into the request context so
// repository code that only has a context.Context can still log consistently.
func setRequestContext(c *gin.Context, requestID string, logger *slog.Logger) {
	c.Set(requestIDContextKey, requestID)
	c.Set(loggerContextKey, logger)
	c.Request = c.Request.WithContext(logging.ContextWithLogger(c.Request.Context(), logger))
}
