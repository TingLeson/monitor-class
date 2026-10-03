package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/realtime"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// SocketService upgrades one authenticated request into a live event connection (§47).
//
// The interface lives here because the handler's requirement is only "serve this socket
// for this principal": the hub owns upgrades, heartbeats and the write queue, and it
// receives a role and a user id that the middleware chain has already established.
type SocketService interface {
	Serve(w http.ResponseWriter, r *http.Request, role realtime.Role, userID uuid.UUID) error
	// Refuse ends a handshake that must not become a session, with a close code the
	// browser can actually read (see the Rejection section below).
	Refuse(w http.ResponseWriter, r *http.Request, code int, reason string) error
}

// socketHandlers serves the two WebSocket entry points.
type socketHandlers struct {
	socket SocketService
	// auth is the same middleware the REST routes use. It is called directly rather than
	// installed with router.Use so the failure can be presented as a close code.
	auth *authMiddleware
	// resolver is the process-wide client address policy. The concurrency cap below
	// keys on it, so the socket a NAT-ed classroom opens are counted against the
	// address the trusted proxy reported and not against the proxy itself.
	resolver *ClientIPResolver
	// sockets caps CONCURRENT sockets per address (§63/§77).
	sockets *socketCap
	// metrics is optional instrumentation (§77).
	metrics *metrics.Metrics
}

func newSocketHandlers(socket SocketService, auth *authMiddleware, resolver *ClientIPResolver, sockets *socketCap, m *metrics.Metrics) *socketHandlers {
	return &socketHandlers{socket: socket, auth: auth, resolver: resolver, sockets: sockets, metrics: m}
}

// socketCap counts live sockets per client address.
//
// WHY a concurrency cap on top of the handshake rate limit: a rate limit bounds how
// fast sockets are opened, not how many stay open. A reconnect loop that never
// closes, or a script that opens sockets and stops reading, accumulates a
// goroutine, a write queue and a file descriptor each. The cap makes "one address
// holds at most N sockets" a property of the process rather than a hope.
//
// The counter is decremented when the handler returns, which is exactly when the
// socket ends: SocketService.Serve blocks for the lifetime of the connection.
type socketCap struct {
	limit int
	mu    sync.Mutex
	live  map[string]int
}

func newSocketCap(limit int) *socketCap {
	if limit <= 0 {
		return nil
	}
	return &socketCap{limit: limit, live: make(map[string]int)}
}

// acquire reserves one slot for addr. The returned release function is idempotent
// and safe to defer immediately.
func (c *socketCap) acquire(addr string) (release func(), ok bool) {
	if c == nil {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live[addr] >= c.limit {
		return nil, false
	}
	c.live[addr]++
	// The release is idempotent (sync.Once) because the caller defers it and the
	// handler has more than one return path: a double release would free a slot that
	// belongs to another connection from the same address, and the cap would then be
	// silently higher than configured for exactly the client that abuses it.
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.live[addr] <= 1 {
				// Delete the bucket rather than leaving a zero: a process that has
				// been up for a term must not keep one map entry per address that
				// ever connected.
				delete(c.live, addr)
				return
			}
			c.live[addr]--
		})
	}, true
}

// liveCount reports how many sockets one address currently holds. It exists for the
// tests, which must not assert on timing.
func (c *socketCap) liveCount(addr string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[addr]
}

// serve authenticates the handshake and then either serves the socket or refuses it.
//
// # Why the authentication is not a middleware here
//
// Every other route in this package is guarded by RequireSession + RequireRole, and those
// middlewares answer a rejection with the standard JSON envelope. A BROWSER CANNOT READ
// THAT. A failed WebSocket handshake is reported to JavaScript as "the connection
// failed" — the status code, the body and the headers are all invisible — so a frontend
// cannot tell "the session expired, go to the login page" from "the network blipped,
// retry with a backoff". The only channel that reaches the client is a WebSocket close
// code, which requires an accepted upgrade.
//
// So this handler performs the SAME decision the middleware performs — literally the same
// function, authMiddleware.authenticate, which is why it is not duplicated logic — and
// then presents it two ways:
//
//   - a real WebSocket handshake from a browser → accept the upgrade and close with
//     4401 (no live session) or 4403 (authenticated on another entry point);
//   - anything else (curl, an ops probe, a monitoring check) → the standard 401/403
//     envelope, exactly like the rest of the API.
//
// The refused socket is never registered with the hub, has no reader goroutine and
// receives nothing: it exists for exactly one close frame.
func (h *socketHandlers) serve(role realtime.Role, entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, err := h.auth.authenticate(c, entry)
		if err != nil {
			h.reject(c, entry, role, err)
			return
		}
		principal, ok := PrincipalFrom(c)
		if !ok {
			// Unreachable: authenticate stores the principal it resolved.
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}
		if principal.Role != entry.Role {
			// Unreachable through authenticate (it rejects a session that belongs to
			// another entry point), kept because this is the check that decides which
			// socket a caller gets.
			h.reject(c, entry, role, apperr.New(apperr.CodeRoleForbidden))
			return
		}

		// The cap is applied AFTER authentication and before the socket is
		// registered. Charging an unauthenticated handshake against the cap would
		// let anyone exhaust an address's budget with cheap 401s and lock the real
		// student behind the same NAT out of their classroom.
		addr := ""
		if h.resolver != nil {
			addr = h.resolver.ClientIP(c.Request)
		}
		release, ok := h.sockets.acquire(addr)
		if !ok {
			h.rejectTooManySockets(c, entry, role, addr)
			return
		}
		defer release()

		if serveErr := h.socket.Serve(c.Writer, c.Request, role, principal.UserID); serveErr != nil {
			h.serveFailure(c, role, principal, serveErr)
		}
	}
}

// reject presents one authentication failure.
//
// The distinction between the two presentations is made on the REQUEST, not on the error:
// a WebSocket upgrade gets a close code, everything else gets the envelope. That keeps an
// ops probe (which cannot speak WebSocket) readable, and it means a future non-browser
// caller of /ws/** is not silently given a 101.
func (h *socketHandlers) reject(c *gin.Context, entry AuthEntry, role realtime.Role, err error) {
	code := apperr.From(err)
	closeCode, reason := realtime.CloseSessionInvalid, "AUTH_REQUIRED"
	if code != nil && code.Code == apperr.CodeRoleForbidden {
		// 403 from `authenticate` means "the cookie is valid on ANOTHER entry point".
		closeCode, reason = realtime.CloseRoleForbidden, "ROLE_FORBIDDEN"
	}

	LoggerFrom(c).Info("websocket handshake rejected",
		"action", "ws_handshake_rejected",
		"entry", entry.PathPrefix,
		"role", string(role),
		"path", c.Request.URL.Path,
		"close_code", closeCode,
		"cause", errorCause(err),
		"note", "a browser cannot read an HTTP status from a failed upgrade; the close code is the contract",
	)

	if !isWebSocketHandshake(c.Request) {
		RespondError(c, err)
		return
	}
	if refuseErr := h.socket.Refuse(c.Writer, c.Request, closeCode, reason); refuseErr != nil {
		// The upgrade failed (a rejected Origin, a malformed handshake). gorilla has
		// already written the HTTP error, so there is nothing left to answer with.
		c.Abort()
	}
}

// rejectTooManySockets answers a handshake that would exceed the per-address cap.
//
// The answer is the standard 429 envelope (a browser that is refused an upgrade
// cannot read it, but the frontend's reconnect loop treats a refused handshake as
// "back off" either way, and the log line names the reason). 429 rather than 503:
// nothing is broken, this address simply holds too many sockets.
func (h *socketHandlers) rejectTooManySockets(c *gin.Context, entry AuthEntry, role realtime.Role, addr string) {
	h.metrics.IncRateLimitRejected(metrics.RateLimitScopeWSHandshake, metrics.DimensionIP)
	LoggerFrom(c).Warn("websocket rejected: too many concurrent connections from this address",
		"action", "ws_connection_cap",
		"entry", entry.PathPrefix,
		"role", string(role),
		"ip", addr,
		"limit", h.sockets.limit,
	)
	c.Writer.Header().Set(headerRetryAfter, "5")
	RespondError(c, apperr.New(apperr.CodeRateLimited))
}

// serveFailure maps a hub failure onto a response, mirroring the pre-close-code
// behaviour: a refusal before the upgrade is a 5xx, and a failure AFTER the upgrade has
// already written its own status (the connection is hijacked by then, so writing here
// would append a second response to a finished request).
func (h *socketHandlers) serveFailure(c *gin.Context, role realtime.Role, principal *auth.Principal, err error) {
	if errors.Is(err, realtime.ErrUnavailable) {
		LoggerFrom(c).Info("websocket not served",
			"action", "ws_unavailable",
			"role", string(role),
			logging.FieldUserID, principal.UserID.String(),
		)
		RespondErrorCode(c, apperr.CodeInternal)
		return
	}
	c.Abort()
}

// isWebSocketHandshake reports whether the request is asking to upgrade.
//
// It is a header check and not a "did it work" check: the point is to choose between the
// close-code path and the envelope BEFORE the upgrade is attempted.
func isWebSocketHandshake(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(r.Header.Get("Connection"), "upgrade")
}

// headerContainsToken reports whether a comma-separated header contains one token, which
// is how "Connection: keep-alive, Upgrade" must be read.
func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// errorCause renders an app error's internal cause for a log line (never for a response).
func errorCause(err error) string {
	appErr := apperr.From(err)
	if appErr == nil || appErr.Err == nil {
		return ""
	}
	return appErr.Err.Error()
}

// registerSocketRoutes mounts GET /ws/student and GET /ws/teacher (§47).
//
// # Why these two paths and not /api/v1/student/ws
//
// They are long-lived connections, not API calls: the versioned group is where the
// request/response contract lives, and a socket that stays open for a lesson does not
// version the same way. They are outside /api/v1 for the same reason the webhook is —
// the coarse per-IP limiter is installed on that group, and a connection that carries a
// lesson's worth of events must not consume an API budget or be cut off mid-lesson.
//
// # Authentication
//
// Exactly the same chain as every other authenticated route: the entry point's session
// cookie (classwatch_session_student / classwatch_session_teacher), resolved against the
// database on every handshake, then the role check. There is deliberately NO token in the
// URL: a query string is written to access logs, proxy logs and browser history, which is
// where a credential must never end up (§47/§63). A handshake that fails answers the
// standard 401/403 envelope and never upgrades — the socket simply does not exist.
//
// # Why there is no CSRF middleware on a GET
//
// CSRFProtection exempts safe methods, so adding it would be a no-op and would suggest
// this route is protected by something it is not. The real protection for a WebSocket
// handshake — which browsers do NOT subject to CORS — is the Origin check in the
// upgrader (realtime.Hub): a hostile page cannot forge the Origin header, so a
// connection from an origin outside the allowlist is refused even though the browser
// would happily attach the victim's cookie.
func registerSocketRoutes(router *gin.Engine, deps Deps, entries []AuthEntry, resolver *ClientIPResolver) {
	if deps.Socket == nil || deps.Auth == nil {
		// No hub (a deployment without the realtime layer) or no auth service (no
		// database): the routes are not registered, so they answer 404 instead of
		// existing without an authorization chain in front of them.
		return
	}

	mw := newAuthMiddleware(deps.Auth, entries, deps.Config)
	// Same reasoning as the body cap in NewRouter: a nil Config is supported (there
	// are no entries to serve then, because the entry points come from the config),
	// and the cap must default rather than panic.
	maxSocketsPerIP := 0
	if deps.Config != nil {
		maxSocketsPerIP = deps.Config.WSMaxConnectionsPerIP
	}
	handlers := newSocketHandlers(deps.Socket, mw, resolver, newSocketCap(maxSocketsPerIP), deps.Metrics)

	// The handshake limiter is per address and cheap (it is the coarse front door of
	// a long-lived resource); the concurrency cap inside the handler is what bounds
	// the resource itself. Both are needed: see socketCap.
	handshakeLimit := RateLimitWSHandshake(deps.Limiter, deps.Metrics, resolver,
		deps.Config.RateLimitWSHandshake, deps.Config.RateLimitEndpointWindow)

	// One route per entry point, each bound to its own cookie and role — the same
	// structure as registerAuthRoutes, and for the same reason: a student cookie cannot
	// satisfy the teacher socket even if somebody later forgets a check inside a handler.
	//
	// The authentication itself happens INSIDE the handler (see socketHandlers.serve for
	// why: a browser cannot read the status of a failed upgrade), and it calls the same
	// authMiddleware the REST routes use.
	for _, entry := range entries {
		switch entry.Role {
		case user.RoleStudent:
			router.GET("/ws/student", handshakeLimit, handlers.serve(realtime.RoleStudent, entry))
		case user.RoleTeacher:
			router.GET("/ws/teacher", handshakeLimit, handlers.serve(realtime.RoleTeacher, entry))
		default:
			// No admin socket exists in V1: the administrator's surface manages accounts
			// and has nothing to watch live.
			continue
		}
	}
}
