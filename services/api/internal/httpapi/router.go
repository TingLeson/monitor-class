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

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
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
	// Session is the student media session service of §12/§43/§51: join, leave, the
	// teacher media token and the monitoring wall. A nil value means those four
	// routes are not registered, again for the same reason — an API with no database
	// cannot record a session, and an endpoint that issued media tokens without one
	// would produce participants nobody can supervise.
	Session SessionService
	// PrivateTalk is the private-talk state machine of §31/§76 (start, stop, read). It is
	// a field of its own rather than part of SessionService because it is a different
	// subject with a different authority (the media-plane subscription of ONE student), and
	// a nil value means the three routes are simply not registered. In production both
	// fields are satisfied by the same *session.Service.
	PrivateTalk PrivateTalkService
	// Webhook is the LiveKit webhook endpoint of §45: signature verification plus the
	// state machine that consumes the verified event. Both halves must be present for
	// the route to exist — a verifier without a processor would accept events and drop
	// them, and a processor without a verifier would apply unauthenticated input.
	//
	// The endpoint is mounted OUTSIDE /api/v1 on purpose: the per-IP limiter lives on
	// that group, and LiveKit's delivery must not be throttled by (or counted against)
	// a browser's budget. The signature is its authentication (§63).
	Webhook *WebhookDeps
	// Socket is the business WebSocket layer of §47 (/ws/student, /ws/teacher). A nil
	// value means the routes are not registered: a deployment without a hub cannot serve
	// a socket, and answering 404 is more honest than upgrading into nothing.
	Socket SocketService
	// Limiter protects /api/v1 and, more strictly, the login endpoints. A nil
	// value installs no rate limiting at all, which is only appropriate in tests:
	// §2.2/§63 make it mandatory in every real deployment, and main always wires
	// a Redis-backed limiter with an in-memory fallback.
	Limiter ratelimit.Limiter
	// Metrics is the §77 metric set. A nil value installs no instrumentation and
	// does not register /metrics: a deployment that did not ask for metrics should
	// not have an endpoint that pretends to serve them, and every existing test
	// keeps working unchanged.
	Metrics *metrics.Metrics
	// Drain is the shutdown gate (§62). A nil value means the router never reports
	// "going away", which is correct for a test that runs a router without an
	// http.Server lifecycle.
	Drain *DrainGate
}

// WebhookDeps bundles the two collaborators of the LiveKit webhook endpoint.
//
// They travel together because the endpoint is meaningless without both: the verifier
// proves the caller is LiveKit, the processor applies what LiveKit observed.
type WebhookDeps struct {
	Verifier  LiveKitVerifier
	Processor MediaEventProcessor
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
	// A Config built by hand (a test, a tool) must not silently lose the §63
	// hardening: a zero body limit or a zero window would read as "off".
	// ApplyDefaults only fills fields that are still zero, so a loaded Config is
	// unchanged.
	if deps.Config != nil {
		deps.Config.ApplyDefaults()
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

	// Order matters, and every step of it is load-bearing (§59/§62/§63):
	//
	//  1. RequestID first: nothing can be logged or correlated before it exists.
	//  2. AccessLog next: it must wrap everything, so a request that dies in the
	//     middle of the chain is still one log line.
	//  3. DrainGate: a request that arrives while the process is shutting down is
	//     refused before it can touch a handler or a database pool that is about
	//     to close.
	//  4. Metrics OUTSIDE Recovery: a panic must be observed as the 500 that
	//     Recovery produces, not as the 200 the writer still claims while the
	//     panic unwinds (see MetricsMiddleware).
	//  5. SecurityHeaders before CORS and the handlers: the headers must also be
	//     present on a refused cross-origin request and on a 404.
	//  6. BodyLimit before any handler can read a body.
	router.Use(RequestIDMiddleware(logger))
	router.Use(AccessLogMiddleware(logger, resolver))
	router.Use(deps.Drain.Middleware())
	router.Use(MetricsMiddleware(deps.Metrics))
	router.Use(RecoveryMiddleware(logger))
	router.Use(SecurityHeadersMiddleware(resolver))
	// Read through a local because a nil Config is a supported input (the router is
	// usable without one: only the probes and /metrics exist then), and a middleware
	// must never be the thing that panics on a supported input. A zero limit means
	// "no body cap", which is what BodyLimitMiddleware already does.
	var maxBodyBytes int64
	if deps.Config != nil {
		maxBodyBytes = deps.Config.HTTPMaxBodyBytes
	}
	router.Use(BodyLimitMiddleware(maxBodyBytes))

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

	// /metrics sits next to the probes: it is an infrastructure endpoint consumed
	// by a scraper, not part of the versioned browser contract, and it needs no
	// session (see metrics.Handler for why, and for the production advice).
	if deps.Metrics != nil {
		router.GET("/metrics", gin.WrapH(deps.Metrics.Handler()))
	}

	v1 := router.Group("/api/v1")
	{
		// The coarse limit covers every API route, including the login endpoints
		// (which add their own, stricter limits below).
		if deps.Config != nil {
			v1.Use(RateLimitAPI(deps.Limiter, deps.Metrics, resolver,
				deps.Config.RateLimitAPIPerMinute, deps.Config.RateLimitAPIWindow))
		}
		v1.GET("/meta", metaHandler(env))
	}

	registerAuthRoutes(v1, deps, resolver, entries)
	registerAdminRoutes(v1, deps, entries)
	registerTeacherRoutes(v1, deps, entries)
	registerStudentRoutes(v1, deps, entries)

	// Phase 8's two surfaces, both deliberately OUTSIDE /api/v1:
	//
	//   - The LiveKit webhook (§45) is called by LiveKit's infrastructure, not by a
	//     browser. It authenticates with a signature over the request body and must not
	//     be behind the per-IP API limiter: LiveKit Cloud delivers from a small pool of
	//     egress addresses, so an IP budget would throttle every school at once and could
	//     drop state transitions. Signature verification is the defense (§63).
	//   - The business WebSocket endpoints (§47) are long-lived connections, not API
	//     calls. They authenticate with the same session cookie as everything else, and
	//     they must not consume (or be cut off by) an API request budget mid-lesson.
	registerWebhookRoute(router, deps, resolver)
	registerSocketRoutes(router, deps, entries, resolver)

	// Unknown routes and methods must go through the same error envelope as a
	// handler failure. gin's defaults are bare text ("404 page not found"), which
	// would force every client to special-case the one response shape that does
	// not parse as JSON.
	//
	// HandleMethodNotAllowed is off by default in gin, so a POST to a GET-only
	// route would be answered as 404. Turning it on gives the correct 405 and an
	// Allow header, and makes NoMethod reachable.
	router.HandleMethodNotAllowed = true
	router.NoRoute(notFoundHandler())
	router.NoMethod(methodNotAllowedHandler())

	return router
}

// SessionService is the whole session surface of §43/§51 as the router sees it: the
// student half (join, leave) and the teacher half (media token, monitor).
//
// WHY one interface here while the handlers accept two narrow ones: the router is
// where the two ENTRY POINTS are wired, and it must be able to hand each group only
// the methods it is allowed to use. The narrow interfaces live next to their handlers
// (StudentSessionService, TeacherSessionService), so the split is enforced by the
// compiler at the point where a route could overreach.
type SessionService interface {
	StudentSessionService
	TeacherSessionService
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
			RateLimitLogin(deps.Limiter, deps.Metrics, resolver,
				deps.Config.RateLimitLoginPerMinute, deps.Config.RateLimitLoginWindow),
			RateLimitLoginPerAccount(deps.Limiter, deps.Metrics, resolver,
				deps.Config.RateLimitLoginPerAccountPer10Min, deps.Config.RateLimitLoginAccountWindow),
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
	if !ok || deps.Auth == nil || (deps.Classroom == nil && deps.Session == nil && deps.PrivateTalk == nil) {
		// No teacher entry (no configuration) or no service behind the routes: the
		// routes are not registered, so they answer 404 instead of existing without an
		// authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)

	group := v1.Group("/teacher")
	group.Use(
		mw.RequireSession(teacherEntry),
		mw.RequireRole(teacherEntry),
	)

	writes := group.Group("")
	writes.Use(mw.CSRFProtection(teacherEntry))

	if deps.Classroom != nil {
		handlers := newClassroomHandlers(deps.Classroom)
		group.GET("/classrooms", handlers.listClassrooms())
		group.GET("/classrooms/:id", handlers.getClassroom())
		group.GET("/classrooms/:id/students", handlers.listStudents())
		writes.POST("/classrooms", handlers.createClassroom())
		writes.PATCH("/classrooms/:id", handlers.updateClassroom())
		writes.POST("/classrooms/:id/students", handlers.addStudents())
		writes.DELETE("/classrooms/:id/students/:studentId", handlers.removeStudent())
		writes.POST("/classrooms/:id/open", handlers.openClassroom())
		writes.POST("/classrooms/:id/close", handlers.closeClassroom())
	}

	if deps.Session != nil {
		// The two media routes of Phase 6. The monitor read lands on the outer group
		// (a GET changes no state from the client's point of view) and the media token
		// on the write group: minting a credential is exactly the kind of action a
		// cross-site request must not be able to trigger with the teacher's cookie.
		handlers := newTeacherSessionHandlers(deps.Session)
		group.GET("/classrooms/:id/monitor", handlers.monitor())
		writes.POST("/classrooms/:id/media-token", handlers.mediaToken())
	}

	if deps.PrivateTalk != nil {
		// §31's three endpoints. The read is on the outer group and the two state changes
		// (start, stop) are on the CSRF-protected write group — the same structure as every
		// other teacher write, and for the same reason: a page the teacher happens to be
		// logged into must not be able to start or stop a private talk on their behalf.
		handlers := newPrivateTalkHandlers(deps.PrivateTalk)
		group.GET("/classrooms/:id/private-talk", handlers.show())
		writes.POST("/classrooms/:id/private-talk", handlers.start())
		writes.DELETE("/classrooms/:id/private-talk", handlers.stop())
	}
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
// Phase 4 mounted READS ONLY (§70: "点击 Enter 暂时只进入 PreJoin"). Phase 6 adds the two
// endpoints the screen gate needs — join and leave — and they are the first unsafe
// routes of this group, so the group now has a write subgroup carrying
// CSRFProtection. The structure is the same one /admin and /teacher use: reads outside,
// writes inside, so a new write endpoint added to the wrong group is the only way to
// lose the check — and there is no wrong group to add it to.
func registerStudentRoutes(v1 *gin.RouterGroup, deps Deps, entries []AuthEntry) {
	studentEntry, ok := entryForRole(entries, user.RoleStudent)
	if !ok || deps.Auth == nil || (deps.Classroom == nil && deps.Session == nil) {
		// No student entry (no configuration) or no service behind the routes: the
		// routes are not registered, so they answer 404 instead of existing without an
		// authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)

	group := v1.Group("/student")
	group.Use(
		mw.RequireSession(studentEntry),
		mw.RequireRole(studentEntry),
	)

	if deps.Classroom != nil {
		handlers := newStudentClassroomHandlers(deps.Classroom)
		group.GET("/classrooms", handlers.listClassrooms())
		group.GET("/classrooms/:id", handlers.getClassroom())
	}

	if deps.Session != nil {
		handlers := newStudentSessionHandlers(deps.Session)
		writes := group.Group("")
		writes.Use(mw.CSRFProtection(studentEntry))
		// Both are POSTs and both change state: join creates or revives a session, and
		// leave ends one. A join without a CSRF token would let any website start a
		// media session in a logged-in student's name.
		writes.POST("/classrooms/:id/join", handlers.join())
		writes.POST("/sessions/:id/leave", handlers.leave())
	}
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

// notFoundHandler renders an unmatched path as the standard envelope.
//
// The code is NOT_FOUND and the status is 404: gin's default is a bare text
// "404 page not found", which would force every client to special-case the one
// response shape that does not parse as JSON (§58).
//
// No route, parameter or method detail is echoed back — a 404 that lists the valid
// paths is a free map of the attack surface.
func notFoundHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		RespondError(c, apperr.New(apperr.CodeNotFound))
	}
}

// methodNotAllowedHandler renders a known path with an unknown method.
//
// It is only reachable because HandleMethodNotAllowed is on: gin answers 404 for a
// method mismatch by default, which hides a real client/server disagreement (the
// frontend calls a PUT the API does not implement) behind "this endpoint does not
// exist". Gin adds the Allow header itself.
func methodNotAllowedHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		RespondError(c, apperr.New(apperr.CodeMethodNotAllowed))
	}
}
