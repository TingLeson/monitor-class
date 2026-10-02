package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Context keys. They are unexported constants for the same reason the request id
// keys are: a typo would silently create a second, always-empty value.
const (
	principalContextKey = "classwatch.principal"
	authEntryContextKey = "classwatch.auth_entry"
)

// headerCSRF is the header the frontend must echo. It matches the constant the
// api-client package already sends (CSRF_HEADER in packages/api-client).
const headerCSRF = "X-CSRF-Token"

// AuthService is the part of *auth.Service the HTTP layer uses.
//
// WHY an interface here rather than the concrete type: middleware behaviour
// (401 vs 403, cookie clearing, CSRF, cross-entry rejection) is exactly what most
// needs testing, and those tests must not require PostgreSQL or an Argon2id hash.
type AuthService interface {
	LoginStudent(ctx context.Context, account string, meta auth.LoginMeta) (*auth.LoginResult, error)
	LoginWithPassword(ctx context.Context, role user.Role, account, password string, meta auth.LoginMeta) (*auth.LoginResult, error)
	Authenticate(ctx context.Context, rawToken string) (*auth.Principal, error)
	Logout(ctx context.Context, rawToken string) error
}

// AuthEntry is one of the three independent web entry points (§5).
//
// The role and the cookies travel together because they are one fact: the
// student entry authenticates STUDENT sessions through the student cookie, and
// nothing else. Keeping them in one struct means a route group cannot be wired to
// the wrong role by copy-paste without also getting the wrong cookie.
type AuthEntry struct {
	Role user.Role
	// PathPrefix is the URL segment: "student", "teacher" or "admin".
	PathPrefix string
	// SessionCookie is the HttpOnly cookie of this entry.
	SessionCookie string
	// CSRFCookie is the readable companion cookie of this entry.
	CSRFCookie string
}

// authEntries derives the three entry definitions from configuration.
//
// The order is fixed (student, teacher, admin) because the cross-entry check
// walks it; a stable order keeps that check deterministic.
func authEntries(cfg *config.Config) []AuthEntry {
	roles := []user.Role{user.RoleStudent, user.RoleTeacher, user.RoleAdmin}
	entries := make([]AuthEntry, 0, len(roles))
	for _, role := range roles {
		name := cfg.SessionCookieNameFor(string(role))
		entries = append(entries, AuthEntry{
			Role:          role,
			PathPrefix:    role.Lower(),
			SessionCookie: name,
			CSRFCookie:    csrfCookieName(name),
		})
	}
	return entries
}

// csrfCookieName is the single place the "-_csrf" suffix is defined: the cookie
// set at login and the cookie read by the frontend must not be able to drift.
func csrfCookieName(sessionCookie string) string { return sessionCookie + "_csrf" }

// authMiddleware carries what every auth middleware needs.
type authMiddleware struct {
	service AuthService
	entries []AuthEntry
	// cfg is kept so a cookie cleared on a 401/403 carries the same attributes as
	// the one that was set (browsers match on name+path+domain, but keeping them
	// identical avoids surprises in strict cookie modes).
	cfg *config.Config
}

func newAuthMiddleware(service AuthService, entries []AuthEntry, cfg *config.Config) *authMiddleware {
	return &authMiddleware{service: service, entries: entries, cfg: cfg}
}

// RequireSession loads the Principal for this entry point into the context.
//
// Failure mapping, and why it is not "everything is 401":
//
//   - No session at all           → 401 AUTH_REQUIRED (log in).
//   - A valid session belonging to ANOTHER entry point → 403 ROLE_FORBIDDEN.
//     The caller is authenticated; they are simply not a student/teacher/admin.
//     Answering 401 would tell a logged-in teacher that they are "not logged in",
//     and would send the frontend into a login loop it cannot escape (§67).
//   - Account disabled            → 403 ACCOUNT_DISABLED, and the session has
//     already been revoked by the service so re-enabling the account does not
//     resurrect it.
//
// Every 401/403 also clears this entry's cookies. WHY: the cookie is what the
// frontend reads to decide "am I logged in?"; leaving a dead one in the browser
// makes the app believe it has a session and retry forever instead of showing the
// login page.
func (m *authMiddleware) RequireSession(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := m.cookieValue(c, entry.SessionCookie)

		if raw == "" {
			// No cookie for this entry. Before answering 401, check whether the
			// caller holds a live session on a different entry point — that is an
			// authorization failure, not a missing login.
			if other, principal := m.sessionFromOtherEntry(c, entry); principal != nil {
				clearAuthCookies(c, entry, m.cfg)
				LoggerFrom(c).Info("cross-entry access rejected",
					"entry", entry.PathPrefix,
					"session_entry", other.PathPrefix,
					logging.FieldUserID, principal.UserID.String(),
					logging.FieldRole, string(principal.Role),
					"path", c.Request.URL.Path,
				)
				RespondError(c, apperr.New(apperr.CodeRoleForbidden))
				return
			}
			clearAuthCookies(c, entry, m.cfg)
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}

		principal, err := m.service.Authenticate(c.Request.Context(), raw)
		switch {
		case err == nil:
			c.Set(principalContextKey, principal)
			c.Set(authEntryContextKey, entry)
			// The raw token is kept in the request context (never logged) so
			// logout can revoke exactly this session.
			setSessionToken(c, raw)
			// NOTE: the request logger is deliberately NOT enriched with the
			// identity here. Every line that needs it (login, logout, session
			// rejection, CSRF rejection) adds user_id and role itself, and doing it
			// in both places emits duplicate fields — noise in text logs and an
			// ambiguous document in JSON ones. The access log reads the principal
			// from the context instead (see AccessLogMiddleware).
			c.Next()

		case isAuthError(err, auth.ErrAccountDisabled):
			clearAuthCookies(c, entry, m.cfg)
			LoggerFrom(c).Info("session rejected: account disabled",
				"entry", entry.PathPrefix, "path", c.Request.URL.Path)
			RespondError(c, apperr.New(apperr.CodeAccountDisabled))

		case isAuthError(err, auth.ErrSessionInvalid):
			clearAuthCookies(c, entry, m.cfg)
			LoggerFrom(c).Info("session rejected: invalid session",
				"entry", entry.PathPrefix, "path", c.Request.URL.Path)
			RespondError(c, apperr.New(apperr.CodeAuthRequired))

		default:
			// Database failure and friends: an internal error, and the cookie is
			// deliberately left alone — clearing it would log the user out because
			// of a transient outage.
			RespondError(c, err)
		}
	}
}

// RequireRole enforces the role of the entry point.
//
// 403 rather than 401 on a mismatch, always: the caller IS authenticated, they
// just are not allowed here. Returning 401 would make the frontend clear its
// session and bounce the user to a login page that cannot help them. The role
// comes from the database row loaded by RequireSession, never from the cookie,
// the URL or a client-supplied header (§37).
//
// The entry's cookies are cleared on a mismatch for the same reason they are
// cleared on a 401: a cookie that cannot be used on this entry must not keep the
// frontend believing it is logged in here.
func (m *authMiddleware) RequireRole(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := PrincipalFrom(c)
		if !ok {
			// Unreachable when the middleware order is correct; answering 401 is
			// still the safe failure rather than panicking or allowing.
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}
		if principal.Role != entry.Role {
			clearAuthCookies(c, entry, m.cfg)
			LoggerFrom(c).Warn("role forbidden",
				logging.FieldUserID, principal.UserID.String(),
				logging.FieldRole, string(principal.Role),
				"required_role", string(entry.Role),
				"path", c.Request.URL.Path,
			)
			RespondError(c, apperr.New(apperr.CodeRoleForbidden))
			return
		}
		c.Next()
	}
}

// CSRFProtection requires the session's CSRF token on every unsafe method.
//
// WHY this is needed even though the session cookie is SameSite=Lax: Lax stops a
// cross-site POST from carrying the cookie, but it is a browser default, not a
// contract — an older browser, a same-site subdomain takeover or a future
// SameSite=None change would remove it silently. The token is the check the
// server performs for itself (§63).
//
// There is deliberately NO exception for logout. An endpoint that skips the check
// because "it only destroys a session" is exactly the exception a later endpoint
// gets copy-pasted from.
//
// On failure the entry's two cookies are cleared, but the SESSION IS NOT REVOKED:
//
//   - A CSRF failure means the browser's cookie state is self-contradictory
//     (the readable token is missing or does not match the session), so leaving
//     the HttpOnly session cookie in place would strand the frontend in a zombie
//     state: it believes it is logged in, while every write — including logout —
//     fails with 403. Clearing both lets the next page load start cleanly.
//   - It does NOT mean the session was stolen. A cross-site request that fails
//     this check never proved possession of the session, and revoking on every
//     failed check would hand any website a denial-of-service button against any
//     logged-in user. The session therefore stays valid until its own TTL.
//
// It must be installed AFTER RequireSession (it reads the Principal) and only on
// authenticated routes: the login endpoints have no session yet, which is exactly
// why they cannot be CSRF targets in the first place.
func (m *authMiddleware) CSRFProtection(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isSafeMethod(c.Request.Method) {
			c.Next()
			return
		}
		principal, ok := PrincipalFrom(c)
		if !ok {
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}
		provided := strings.TrimSpace(c.GetHeader(headerCSRF))
		// Constant-time comparison: a byte-by-byte compare would let an attacker
		// recover the token one byte at a time from response timing. Both values
		// are secrets, so neither may be logged.
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(principal.CSRFToken)) != 1 {
			clearAuthCookies(c, entry, m.cfg)
			LoggerFrom(c).Info("csrf rejected",
				logging.FieldUserID, principal.UserID.String(),
				logging.FieldRole, string(principal.Role),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"cookies_cleared", true,
			)
			RespondError(c, apperr.New(apperr.CodeCSRFInvalid))
			return
		}
		c.Next()
	}
}

// PrincipalFrom returns the authenticated caller stored by RequireSession.
func PrincipalFrom(c *gin.Context) (*auth.Principal, bool) {
	if v, ok := c.Get(principalContextKey); ok {
		if p, ok := v.(*auth.Principal); ok && p != nil {
			return p, true
		}
	}
	return nil, false
}

// AuthEntryFrom returns the entry point that authenticated this request.
func AuthEntryFrom(c *gin.Context) (AuthEntry, bool) {
	if v, ok := c.Get(authEntryContextKey); ok {
		if e, ok := v.(AuthEntry); ok {
			return e, true
		}
	}
	return AuthEntry{}, false
}

// sessionFromOtherEntry looks for a live session on the two other entry points.
//
// The cookie is verified (not merely present): a stale cookie left over from an
// expired session proves nothing about being authenticated, and answering 403 to
// it would strand a user who simply needs to log in again. The cost is at most
// two indexed lookups on a request that is about to be rejected anyway.
func (m *authMiddleware) sessionFromOtherEntry(c *gin.Context, entry AuthEntry) (AuthEntry, *auth.Principal) {
	for _, other := range m.entries {
		if other.SessionCookie == entry.SessionCookie {
			continue
		}
		raw := m.cookieValue(c, other.SessionCookie)
		if raw == "" {
			continue
		}
		principal, err := m.service.Authenticate(c.Request.Context(), raw)
		if err != nil || principal == nil {
			continue
		}
		return other, principal
	}
	return AuthEntry{}, nil
}

// cookieValue reads a cookie, tolerating the whitespace a hand-edited cookie jar
// may contain.
func (m *authMiddleware) cookieValue(c *gin.Context, name string) string {
	value, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// isAuthError reports whether err matches an auth sentinel. errors.Is is used
// rather than == because the service is free to wrap its errors with context as
// long as the sentinel stays in the chain.
func isAuthError(err error, target error) bool {
	return errors.Is(err, target)
}
