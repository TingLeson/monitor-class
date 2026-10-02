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
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Deps is everything NewRouter needs. Passing a struct instead of four arguments
// keeps later phases from changing the signature every time a service is added.
type Deps struct {
	Logger *slog.Logger
	Config *config.Config
	// Ready holds the dependency probes for /readyz. A nil field inside it means
	// "not connected"; the probe reports that as degraded instead of panicking.
	Ready ReadinessDeps
	// Auth is the authentication service. A nil value means the auth routes are
	// not registered at all — the router stays usable for probe-only deployments
	// and for tests that do not care about authentication, and no request can
	// reach a handler with a nil service behind it.
	Auth AuthService
	// Admin is the account-administration service (§4/§68). Like Auth, a nil
	// value means the admin routes are simply not registered: an API with no
	// database answers 404 there instead of serving a management surface it
	// cannot back with data.
	Admin AdminService
	// Classroom is the classroom domain service (§69). A nil value means the
	// teacher classroom routes are not registered — same reasoning as Admin: an API
	// that cannot read or write classrooms must not expose endpoints that pretend
	// otherwise.
	Classroom ClassroomService
	// Limiter protects /api/v1 and, more strictly, the login endpoints. A nil
	// value installs no rate limiting at all, which is only appropriate in tests:
	// §2.2/§63 make it mandatory in every real deployment, and main always wires
	// a Redis-backed limiter with an in-memory fallback.
	Limiter ratelimit.Limiter
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

	// One resolver, shared by the access log, the rate limiters and the login
	// handlers. It is the single place the X-Forwarded-For trust decision is made.
	var (
		resolver *ClientIPResolver
		entries  []AuthEntry
	)
	if deps.Config != nil {
		resolver = NewClientIPResolver(deps.Config.TrustedProxies)
		entries = authEntries(deps.Config)
	} else {
		resolver = NewClientIPResolver(nil)
	}

	router := gin.New()
	// Keep gin's own ClientIP() on the same trust policy as our resolver, so any
	// handler that reaches for c.ClientIP() (now or in a later phase) cannot
	// silently reintroduce spoofable addresses. An empty list means "trust no
	// proxy header", which is gin's safe setting and our default.
	if deps.Config != nil {
		if err := router.SetTrustedProxies(deps.Config.TrustedProxyCIDRs()); err != nil {
			// Unreachable: the CIDRs come from validated configuration. Logged
			// rather than ignored because it would mean the policies diverged.
			logger.Warn("could not apply trusted proxies to gin; falling back to trusting none", "error", err)
			_ = router.SetTrustedProxies(nil)
		}
	} else {
		_ = router.SetTrustedProxies(nil)
	}

	// Order matters: the request id must exist before anything can log it, the
	// access log must wrap the recovery handler so a panic is still logged, and
	// CORS must reject a hostile origin before a handler does any work.
	router.Use(
		RequestIDMiddleware(logger),
		AccessLogMiddleware(logger, resolver),
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
	// promise the probe contract does not make. They are also deliberately NOT
	// rate limited: a limiter must never be able to make a healthy process look
	// dead to its orchestrator.
	router.GET("/healthz", healthzHandler)
	router.GET("/readyz", readyzHandler(deps.Ready))

	v1 := router.Group("/api/v1")
	{
		// The coarse limit covers every API route, including the login endpoints
		// (which add their own, stricter limits below).
		if deps.Config != nil {
			v1.Use(RateLimitAPI(deps.Limiter, resolver,
				deps.Config.RateLimitAPIPerMinute, time.Minute))
		}
		v1.GET("/meta", metaHandler(env))
	}

	registerAuthRoutes(v1, deps, resolver, entries)
	registerAdminRoutes(v1, deps, entries)
	registerTeacherRoutes(v1, deps, entries)
	registerStudentRoutes(v1, deps, entries)

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

// registerAuthRoutes mounts the three independent entry points.
//
// Each entry gets its own cookie, its own role check and its own middleware
// chain. That structure is what makes cross-entry access impossible by
// construction: a student session is looked up through the student cookie and
// then checked against STUDENT, so it cannot satisfy the teacher group even if
// someone later forgets a check inside a handler (§37/§67).
func registerAuthRoutes(v1 *gin.RouterGroup, deps Deps, resolver *ClientIPResolver, entries []AuthEntry) {
	if deps.Auth == nil || deps.Config == nil || len(entries) == 0 {
		return
	}
	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)
	handlers := newAuthHandlers(deps.Auth, resolver, deps.Config)

	for _, entry := range entries {
		entry := entry
		group := v1.Group("/" + entry.PathPrefix)

		login := handlers.loginWithPassword(entry)
		if entry.Role == user.RoleStudent {
			login = handlers.loginStudent(entry)
		}
		// The login endpoints cannot require CSRF: there is no session yet, so
		// there is no token to compare against. They are protected by the origin
		// allowlist (CORS middleware) and by two rate limits, and they are the
		// endpoints an attacker is expected to hammer.
		group.POST("/auth/login",
			RateLimitLogin(deps.Limiter, resolver, deps.Config.RateLimitLoginPerMinute, time.Minute),
			RateLimitLoginPerAccount(deps.Limiter, resolver, deps.Config.RateLimitLoginPerAccountPer10Min, 10*time.Minute),
			login,
		)

		authed := group.Group("")
		authed.Use(
			mw.RequireSession(entry),
			mw.RequireRole(entry),
			mw.CSRFProtection(entry),
		)
		authed.GET("/auth/me", handlers.me())
		authed.POST("/auth/logout", handlers.logout(entry))
	}
}

// registerAdminRoutes mounts /api/v1/admin/** — the account administration
// surface of §4/§42/§68.
//
// Every route in this group goes through the SAME three layers, and the structure
// is what guarantees it rather than a reviewer remembering:
//
//   - RequireSession(admin entry): the caller holds a live session presented
//     through the ADMIN cookie, resolved against the database on every request.
//   - RequireRole(ADMIN): that session's account really has the ADMIN role. The
//     check is an equality test against the row, never against the cookie, so a
//     teacher or student session cannot satisfy it (§37).
//   - CSRFProtection on every route that can change state. Reads are exempt
//     because a GET from another origin is not a state change; the exempt set is
//     exactly the two read routes and nothing else.
//
// The nesting matters: writes are mounted on an inner group that has no read
// routes in it, so adding a write endpoint to the wrong group is the only way to
// lose the CSRF check — and there is no wrong group to add it to.
func registerAdminRoutes(v1 *gin.RouterGroup, deps Deps, entries []AuthEntry) {
	adminEntry, ok := entryForRole(entries, user.RoleAdmin)
	if !ok || deps.Admin == nil || deps.Auth == nil {
		// No admin entry (no configuration) or no admin service (no database):
		// the routes are not registered, so the endpoints answer 404 rather than
		// existing without an authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)
	handlers := newAdminHandlers(deps.Admin)

	group := v1.Group("/admin")
	group.Use(
		mw.RequireSession(adminEntry),
		mw.RequireRole(adminEntry),
	)

	group.GET("/users", handlers.listUsers())
	group.GET("/users/:id", handlers.getUser())

	writes := group.Group("")
	writes.Use(mw.CSRFProtection(adminEntry))
	writes.POST("/users", handlers.createUser())
	writes.PATCH("/users/:id", handlers.updateUser())
	writes.PATCH("/users/:id/status", handlers.setStatus())
	writes.POST("/teachers/:id/reset-password", handlers.resetTeacherPassword())
}

// registerTeacherRoutes mounts /api/v1/teacher/** — the classroom surface of
// §42/§69.
//
// The group is bound to the TEACHER entry point exactly as /admin is bound to the
// admin one, and the consequence is deliberate (§4): an administrator session
// cannot satisfy RequireSession(teacher entry) and gets 403 ROLE_FORBIDDEN on every
// route here. "Admin" is not a super-teacher in this system — the admin surface
// manages accounts, and a classroom can only be opened or closed by the teacher who
// owns it. Giving administrators a bypass "for support" would create exactly one
// account class whose actions the ownership check does not cover, and that is the
// account class an attacker would target.
//
// Ownership is a SECOND check, performed in the service for every endpoint, not a
// consequence of the route prefix: the prefix answers "is this caller a TEACHER?",
// never "is this YOUR classroom" (§37/§63).
//
// Reads are mounted on the outer group, writes on an inner group that carries
// CSRFProtection, so adding a write endpoint to the wrong group is the only way to
// lose the CSRF check — and there is no wrong group to add it to.
func registerTeacherRoutes(v1 *gin.RouterGroup, deps Deps, entries []AuthEntry) {
	teacherEntry, ok := entryForRole(entries, user.RoleTeacher)
	if !ok || deps.Classroom == nil || deps.Auth == nil {
		// No teacher entry (no configuration) or no classroom service (no
		// database): the routes are not registered, so they answer 404 instead of
		// existing without an authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)
	handlers := newClassroomHandlers(deps.Classroom)

	group := v1.Group("/teacher")
	group.Use(
		mw.RequireSession(teacherEntry),
		mw.RequireRole(teacherEntry),
	)

	group.GET("/classrooms", handlers.listClassrooms())
	group.GET("/classrooms/:id", handlers.getClassroom())
	group.GET("/classrooms/:id/students", handlers.listStudents())

	writes := group.Group("")
	writes.Use(mw.CSRFProtection(teacherEntry))
	writes.POST("/classrooms", handlers.createClassroom())
	writes.PATCH("/classrooms/:id", handlers.updateClassroom())
	writes.POST("/classrooms/:id/students", handlers.addStudents())
	writes.DELETE("/classrooms/:id/students/:studentId", handlers.removeStudent())
	writes.POST("/classrooms/:id/open", handlers.openClassroom())
	writes.POST("/classrooms/:id/close", handlers.closeClassroom())
}

// registerStudentRoutes mounts /api/v1/student/** — the student portal of §14/§70.
//
// The group is bound to the STUDENT entry exactly as /teacher is bound to the
// teacher one, and the consequence is the whole of the student-side access control:
// no session is 401 AUTH_REQUIRED, and a TEACHER or ADMIN session is 403
// ROLE_FORBIDDEN. An administrator is not a super-student either — the admin surface
// manages accounts, and a classroom roster is not something an administrator's
// session may read on a student's behalf.
//
// Authorization stops there only because there is nothing else to check: which
// classrooms a student sees is decided by the classroom_students JOIN inside the
// repository (§14), keyed on the session's user id, so there is no per-classroom
// ownership rule for a handler to state. That is deliberate — a filter applied in
// Go is a filter somebody can forget, and the list endpoint has no single resource
// whose owner could be compared.
//
// Phase 4 mounts READS ONLY (§70: "点击 Enter 暂时只进入 PreJoin"). The join, leave
// and event endpoints of §42 belong to Phase 5/6/8, where a screen gate and a media
// plane exist for them to mean something; they are not stubbed here. Because the
// group has no unsafe route, it carries no CSRFProtection — an inline
// `isSafeMethod` check would be doing that work for exactly one endpoint today and
// would hide the omission when the first POST arrives. The coarse API rate limit
// from NewRouter still applies to both routes like every other /api/v1 route.
func registerStudentRoutes(v1 *gin.RouterGroup, deps Deps, entries []AuthEntry) {
	studentEntry, ok := entryForRole(entries, user.RoleStudent)
	if !ok || deps.Classroom == nil || deps.Auth == nil {
		// No student entry (no configuration) or no classroom service (no
		// database): the routes are not registered, so they answer 404 instead of
		// existing without an authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)
	handlers := newStudentClassroomHandlers(deps.Classroom)

	group := v1.Group("/student")
	group.Use(
		mw.RequireSession(studentEntry),
		mw.RequireRole(studentEntry),
	)

	group.GET("/classrooms", handlers.listClassrooms())
	group.GET("/classrooms/:id", handlers.getClassroom())
}

// entryForRole finds the entry point of one role.
func entryForRole(entries []AuthEntry, role user.Role) (AuthEntry, bool) {
	for _, entry := range entries {
		if entry.Role == role {
			return entry, true
		}
	}
	return AuthEntry{}, false
}

// noRouteHandler renders 404/405 in the standard envelope.
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
