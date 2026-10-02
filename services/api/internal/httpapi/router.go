// Package httpapi wires the HTTP surface of the ClassWatch API.
//
// Phase 0 exposes exactly three endpoints: two probes and a build descriptor.
// Business routes (auth, classrooms, sessions, media tokens) arrive in later
// phases and belong behind role and ownership middleware, never inline in a
// handler.
//
// Everything in this package follows one contract: success responses are plain
// JSON, every failure is the same error envelope, and the error `code` — not the
// HTTP status and not the message — is what a client branches on (§58).
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/config"
)

// Deps is everything NewRouter needs. Passing a struct instead of four arguments
// keeps later phases from changing the signature every time a service is added.
type Deps struct {
	Logger *slog.Logger
	Config *config.Config
	// Ready holds the dependency probes for /readyz. A nil field inside it means
	// "not connected"; the probe reports that as degraded instead of panicking.
	Ready ReadinessDeps
}

// NewRouter builds the fully configured HTTP handler.
//
// gin.New is used instead of gin.Default because gin's own logger writes
// unstructured text to stdout and its recovery handler prints the request body
// on panic. Both are replaced by the slog middleware in this package (§59).
func NewRouter(deps Deps) *gin.Engine {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Release mode in production silences gin's route-debug output and its
	// "running in debug mode" warning. Development keeps debug mode because the
	// route table printed at boot is genuinely useful there; tests use test mode
	// so a misconfigured route does not spam the test log.
	switch {
	case deps.Config != nil && deps.Config.IsProduction():
		gin.SetMode(gin.ReleaseMode)
	case deps.Config != nil && deps.Config.AppEnv == config.EnvTest:
		gin.SetMode(gin.TestMode)
	default:
		gin.SetMode(gin.DebugMode)
	}

	router := gin.New()
	// Order matters: the request id must exist before anything can log it, the
	// access log must wrap the recovery handler so a panic is still logged, and
	// CORS must reject a hostile origin before a handler does any work.
	router.Use(
		RequestIDMiddleware(logger),
		AccessLogMiddleware(logger),
		RecoveryMiddleware(logger),
	)

	env := ""
	var origins []string
	if deps.Config != nil {
		env = deps.Config.AppEnv
		origins = deps.Config.CORSOrigins()
	}
	router.Use(CORSMiddleware(origins))

	// Probes live outside /api/v1 on purpose: they are infrastructure endpoints
	// consumed by orchestrators, and versioning them would imply a compatibility
	// promise the probe contract does not make.
	router.GET("/healthz", healthzHandler)
	router.GET("/readyz", readyzHandler(deps.Ready))

	v1 := router.Group("/api/v1")
	{
		v1.GET("/meta", metaHandler(env))
	}

	// Unknown routes and methods must go through the same error envelope as a
	// handler failure. gin's defaults are bare text ("404 page not found"), which
	// would force every client to special-case the one response shape that does
	// not parse as JSON.
	//
	// HandleMethodNotAllowed is off by default in gin, so a POST to a GET-only
	// route would be answered as 404. Turning it on gives the correct 405 and an
	// Allow header, and makes NoMethod reachable.
	router.HandleMethodNotAllowed = true
	router.NoRoute(noRouteHandler(http.StatusNotFound))
	router.NoMethod(noRouteHandler(http.StatusMethodNotAllowed))

	return router
}

// noRouteHandler renders 404/405 in the standard envelope.
//
// The code is INVALID_REQUEST in both cases: from the client's point of view the
// request targets something this API does not serve, and one code keeps the
// frontend's error handler small. The HTTP status still separates "wrong path"
// from "wrong method" for tooling and for HTTP caches.
//
// No route, parameter or method detail is echoed back — a 404 that lists valid
// paths is a free map of the attack surface.
func noRouteHandler(code int) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := apperr.New(apperr.CodeInvalidRequest)
		err.Status = code
		if code == http.StatusMethodNotAllowed {
			err.Message = "Method not allowed for this endpoint."
		} else {
			err.Message = "Endpoint not found."
		}
		RespondError(c, err)
	}
}
