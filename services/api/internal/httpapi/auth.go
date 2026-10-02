package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// sessionTokenContextKey holds the raw session token for the duration of one
// request so that logout can revoke exactly this session.
//
// WHY it is safe to keep in memory but must never be logged: it is a bearer
// credential. It lives here instead of in the Principal because the Principal is
// passed around and logged, while this value must never reach a log line (§59).
const sessionTokenContextKey = "classwatch.session_token"

// maxLoginBodyBytes bounds a login request body.
//
// A login body is three short fields. Without a bound, the per-account rate-limit
// middleware and the JSON binder would both happily read whatever a client sends,
// which turns an unauthenticated endpoint into a memory amplifier.
const maxLoginBodyBytes = 8 << 10

// maxAccountLength mirrors the upper bound of users_account_format. Longer input
// cannot match any account, so it is rejected as a malformed request instead of
// being sent to the database.
const maxAccountLength = 64

// loginRequest is the request body of every login endpoint.
//
// One struct for all three entries: the student entry simply never reads
// Password, and the handler for each entry decides which fields are required.
type loginRequest struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

// userDTO is the only user shape that ever leaves this API.
//
// WHY it is a separate type instead of serialising user.User: the domain struct
// carries PasswordHash. A DTO that does not have the field cannot leak it, no
// matter what a future refactor of the domain type does, and that is a stronger
// guarantee than remembering to add `json:"-"` (§9/§58).
//
// The field set is the Phase 1 contract the three frontends are built against.
type userDTO struct {
	ID          string  `json:"id"`
	Account     string  `json:"account"`
	DisplayName string  `json:"displayName"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"createdAt"`
	LastLoginAt *string `json:"lastLoginAt"`
}

func newUserDTO(u *user.User) userDTO {
	dto := userDTO{
		ID:          u.ID.String(),
		Account:     u.Account,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		Status:      string(u.Status),
		CreatedAt:   formatTimestamp(u.CreatedAt),
	}
	if u.LastLoginAt != nil {
		last := formatTimestamp(*u.LastLoginAt)
		dto.LastLoginAt = &last
	}
	return dto
}

// newUserDTOFromPrincipal builds the same DTO from an authenticated request. The
// values come from the sessions ⋈ users join, i.e. from the database, not from
// the cookie.
func newUserDTOFromPrincipal(p *auth.Principal) userDTO {
	dto := userDTO{
		ID:          p.UserID.String(),
		Account:     p.Account,
		DisplayName: p.DisplayName,
		Role:        string(p.Role),
		Status:      string(p.Status),
		CreatedAt:   formatTimestamp(p.CreatedAt),
	}
	if p.LastLoginAt != nil {
		last := formatTimestamp(*p.LastLoginAt)
		dto.LastLoginAt = &last
	}
	return dto
}

// formatTimestamp renders a time as RFC3339 in UTC. UTC keeps a client in any
// timezone from having to guess what "14:05" meant, which matters for a system
// whose audit trail is the product.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// authHandlers implements the six auth endpoints of the three entries.
type authHandlers struct {
	service  AuthService
	resolver *ClientIPResolver
	cfg      *config.Config
}

func newAuthHandlers(service AuthService, resolver *ClientIPResolver, cfg *config.Config) *authHandlers {
	return &authHandlers{service: service, resolver: resolver, cfg: cfg}
}

// loginStudent handles POST /api/v1/student/auth/login (account only, §38).
func (h *authHandlers) loginStudent(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := bindLogin(c)
		if !ok {
			return
		}
		account := strings.TrimSpace(req.Account)
		if !validLoginAccount(account) {
			RespondError(c, invalidLoginBody())
			return
		}
		result, err := h.service.LoginStudent(c.Request.Context(), account, h.loginMeta(c))
		h.finishLogin(c, entry, result, err)
	}
}

// loginWithPassword handles POST /api/v1/{teacher,admin}/auth/login (§39/§40).
func (h *authHandlers) loginWithPassword(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := bindLogin(c)
		if !ok {
			return
		}
		account := strings.TrimSpace(req.Account)
		// A missing password is a malformed request, not a failed login: the
		// frontend must never send it, and answering 400 keeps "wrong password"
		// (401) unambiguous in the logs.
		if !validLoginAccount(account) || req.Password == "" {
			RespondError(c, invalidLoginBody())
			return
		}
		result, err := h.service.LoginWithPassword(c.Request.Context(), entry.Role, account, req.Password, h.loginMeta(c))
		h.finishLogin(c, entry, result, err)
	}
}

// me handles GET /api/v1/{entry}/auth/me.
//
// The middleware has already resolved the session against the database, so this
// handler only renders what it was given. There is no second query and, more
// importantly, no path where a client could ask "who am I?" and be believed.
func (h *authHandlers) me() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := PrincipalFrom(c)
		if !ok {
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"user": newUserDTOFromPrincipal(principal)})
	}
}

// logout handles POST /api/v1/{entry}/auth/logout.
//
// 204 with no body on success — the frontend has nothing to read, and the state
// it cares about is in the cleared cookies.
func (h *authHandlers) logout(entry AuthEntry) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := sessionTokenFrom(c)
		if err := h.service.Logout(c.Request.Context(), raw); err != nil {
			// The cookie is deliberately NOT cleared here: the session may still
			// be live server-side, and dropping the cookie would leave the client
			// unable to revoke it. A failed logout must look like a failure.
			RespondError(c, err)
			return
		}
		clearAuthCookies(c, entry, h.cfg)
		LoggerFrom(c).Info("logout completed", "entry", entry.PathPrefix)
		c.Status(http.StatusNoContent)
	}
}

// bindLogin reads and validates the JSON body, answering 400 in the standard
// envelope when it is unusable.
func bindLogin(c *gin.Context) (loginRequest, bool) {
	// Bounded before the decoder sees it: see maxLoginBodyBytes.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxLoginBodyBytes)
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// The decoder error is kept server-side. It can quote fragments of the
		// request body, and a login body contains a password (§59).
		LoggerFrom(c).Info("login body rejected", "error", err.Error())
		RespondError(c, invalidLoginBody())
		return loginRequest{}, false
	}
	return req, true
}

// validLoginAccount applies the cheap, offline part of account validation.
func validLoginAccount(account string) bool {
	return account != "" && len(account) <= maxAccountLength
}

// invalidLoginBody builds the 400 for a malformed login request. The message is
// fixed: echoing which field failed would be a free oracle for probing the
// endpoint, and the frontend already validates its own form.
func invalidLoginBody() *apperr.Error {
	err := apperr.New(apperr.CodeInvalidRequest)
	err.Message = "The request body must contain a valid account" +
		" (and a password for the teacher and admin entries)."
	return err
}

// finishLogin maps the service outcome onto the response, and clears the cookies
// on the two failures that mean "this client must not keep a session".
func (h *authHandlers) finishLogin(c *gin.Context, entry AuthEntry, result *auth.LoginResult, err error) {
	switch {
	case err == nil && result != nil:
		h.setAuthCookies(c, entry, result)
		RespondJSON(c, http.StatusOK, gin.H{"user": newUserDTO(result.User)})

	case errors.Is(err, auth.ErrAccountDisabled):
		clearAuthCookies(c, entry, h.cfg)
		RespondError(c, apperr.New(apperr.CodeAccountDisabled))

	case errors.Is(err, auth.ErrInvalidCredentials):
		// One code, one message, one status for "unknown account" and "wrong
		// password" — the whole point of the sentinel (§58).
		RespondError(c, apperr.New(apperr.CodeInvalidCredentials))

	default:
		RespondError(c, err)
	}
}

// loginMeta describes where the login came from, for the sessions row.
func (h *authHandlers) loginMeta(c *gin.Context) auth.LoginMeta {
	return auth.LoginMeta{
		UserAgent: c.Request.UserAgent(),
		IP:        h.resolver.ClientAddr(c.Request),
	}
}

// setAuthCookies issues the session cookie and its CSRF companion.
//
// Session cookie: HttpOnly, so a successful XSS cannot read the token; Secure per
// configuration (mandatory in production, impossible on plain-HTTP localhost);
// SameSite=Lax, which still allows a normal top-level navigation back into the
// app while blocking cross-site POSTs; Path=/ because the three entry points
// share one origin in development and the API mounts them under /api/v1.
//
// The name is per entry point (`classwatch_session_student`, ...): cookies are
// scoped by host, not by port, so with a single shared name a teacher logging in
// on localhost:5174 would overwrite the student's cookie on localhost:5173 and
// silently log that student out.
//
// CSRF cookie: the same token as the session row, intentionally NOT HttpOnly —
// the frontend must read it and echo it in X-CSRF-Token. It is not a credential
// by itself: without the HttpOnly session cookie it grants nothing.
func (h *authHandlers) setAuthCookies(c *gin.Context, entry AuthEntry, result *auth.LoginResult) {
	maxAge := int(h.cfg.SessionTTL.Seconds())
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     entry.SessionCookie,
		Value:    result.SessionToken,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   h.cfg.SessionCookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     entry.CSRFCookie,
		Value:    result.CSRFToken,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: false,
		Secure:   h.cfg.SessionCookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearAuthCookies expires both cookies of an entry point.
//
// It is called on every 401/403 for that entry: the frontend treats "the cookie
// exists" as "I am logged in", so a dead cookie left in place produces a UI that
// retries forever instead of showing the login page.
func clearAuthCookies(c *gin.Context, entry AuthEntry, cfg *config.Config) {
	secure := cfg != nil && cfg.SessionCookieSecure
	expired := time.Unix(0, 0).UTC()
	for _, cookie := range []struct {
		name     string
		httpOnly bool
	}{
		{entry.SessionCookie, true},
		{entry.CSRFCookie, false},
	} {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     cookie.name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1, // tells the browser to delete it now
			Expires:  expired,
			HttpOnly: cookie.httpOnly,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// setSessionToken stores the raw token for the duration of the request.
func setSessionToken(c *gin.Context, raw string) { c.Set(sessionTokenContextKey, raw) }

// sessionTokenFrom returns the raw token of the current request, or "".
func sessionTokenFrom(c *gin.Context) string {
	if v, ok := c.Get(sessionTokenContextKey); ok {
		if token, ok := v.(string); ok {
			return token
		}
	}
	return ""
}
