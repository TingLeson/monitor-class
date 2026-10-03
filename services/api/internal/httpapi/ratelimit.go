package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
)

// headerRetryAfter is the standard header a limited client reads to know when to
// come back. Without it a blocked client retries immediately, which is precisely
// the traffic the limit exists to stop.
const headerRetryAfter = "Retry-After"

// Rate-limit key prefixes. They keep every policy in a separate bucket: the same
// IP is allowed 300 API calls a minute while only 10 login attempts, and a shared
// counter would make one policy consume the other's budget.
const (
	loginIPKeyPrefix      = "login:ip:"
	loginAccountKeyPrefix = "login:acct:"
	apiKeyPrefix          = "api:ip:"
	privateTalkKeyPrefix  = "ptalk:ip:"
	mediaTokenKeyPrefix   = "mtoken:ip:"
	joinKeyPrefix         = "join:ip:"
	wsHandshakeKeyPrefix  = "ws:ip:"
)

// RateLimitLogin limits login attempts per client IP.
//
// This is the limit that makes the student entry a real authentication step
// rather than a name lookup: §2.2 accepts that knowing an account is enough to
// log in, and the rate limit is the control that turns that from "one request"
// into "an impractical number of guesses from any single address".
func RateLimitLogin(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return rateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeLogin, metrics.DimensionIP,
		func(_ *gin.Context, ip string) string {
			if ip == "" {
				// No trustworthy address (an unparseable RemoteAddr): counting every
				// such request in one shared bucket would let one broken client lock
				// out others, so they are not counted here. They are still covered by
				// the other layers.
				return ""
			}
			return loginIPKeyPrefix + ip
		})
}

// RateLimitLoginPerAccount limits attempts against ONE account from one address.
//
// WHY a second, slower limit on top of the per-IP one: a distributed attempt on a
// single account stays under any per-IP threshold, and this is the bucket that
// notices it. The account is lower-cased because accounts are citext — `S10086`
// and `s10086` are the same login, and a case-sensitive key would hand an attacker
// a fresh bucket per capitalisation.
func RateLimitLoginPerAccount(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return rateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeLoginAccount, metrics.DimensionAccount,
		func(c *gin.Context, ip string) string {
			account := peekLoginAccount(c)
			if account == "" || ip == "" {
				// No account in the body: the handler will reject it as malformed, and
				// keying on "" would make every malformed request share one bucket.
				return ""
			}
			return loginAccountKeyPrefix + ip + "|" + strings.ToLower(account)
		})
}

// RateLimitAPI is the coarse per-IP limit on every /api/v1 route.
//
// It is deliberately blunt: one runaway tab, one misbehaving script or one
// scanner gets a 429 instead of consuming the connection pool, and no single
// client can make the API unavailable for a whole class.
func RateLimitAPI(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return keyedRateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeAPI, metrics.DimensionIP, apiKeyPrefix)
}

// RateLimitPrivateTalk limits §31's three private-talk endpoints per client IP.
//
// WHY this route needs its own budget: starting a talk is not a read — it calls the
// media plane (a LiveKit UpdateSubscriptions) and writes the target's session
// events. A teacher's console can legitimately poll the GET, but the POST/DELETE
// pair is human-paced, so a per-minute budget that is an order of magnitude above
// human pacing is enough to stop a loop without ever touching a real teacher.
func RateLimitPrivateTalk(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return keyedRateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopePrivateTalk, metrics.DimensionIP, privateTalkKeyPrefix)
}

// RateLimitMediaToken limits the teacher media-token endpoint per client IP.
//
// Minting a token is minting a credential (§44/§63): it must be cheap for the one
// teacher who needs it and expensive for anything that mints in a loop. The media
// plane is also the expensive dependency behind it, so an unlimited loop here is a
// LiveKit outage, not just a slow API.
func RateLimitMediaToken(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return keyedRateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeMediaToken, metrics.DimensionIP, mediaTokenKeyPrefix)
}

// RateLimitJoin limits the student join endpoint per client IP.
//
// WHY a NAT-ed classroom does not break: the limit is per ADDRESS, and a school's
// egress address is shared by every student in it, so the default (30/minute) is
// sized for a room's worth of retries rather than for one browser. Abusing join is
// already bounded by the session rules (§43/§50 create at most one active session
// per student per run), so this limit is about protecting the database from a
// reconnect storm, not about authorization.
func RateLimitJoin(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return keyedRateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeJoin, metrics.DimensionIP, joinKeyPrefix)
}

// RateLimitWSHandshake limits WebSocket handshakes per client IP.
//
// A handshake is not free: it resolves a session against PostgreSQL and only then
// upgrades. The realistic attack is not one connection but a reconnect loop (a
// broken client, or a script), which this limit turns into a bounded rate. The
// concurrent-socket cap (WS_MAX_CONNECTIONS_PER_IP) is the other half: this bounds
// the churn, that one bounds the resource.
func RateLimitWSHandshake(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration) gin.HandlerFunc {
	return keyedRateLimit(limiter, m, resolver, limit, window, metrics.RateLimitScopeWSHandshake, metrics.DimensionIP, wsHandshakeKeyPrefix)
}

// keyedRateLimit is the shared implementation of every IP-keyed policy.
func keyedRateLimit(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration, scope, dimension, prefix string) gin.HandlerFunc {
	return rateLimit(limiter, m, resolver, limit, window, scope, dimension, func(_ *gin.Context, ip string) string {
		if ip == "" {
			return ""
		}
		return prefix + ip
	})
}

// rateLimit is the shared middleware.
//
// The client address is resolved ONCE, here, and the same value is used for the
// key and for the log line: attributing a request to one IP in the log while
// counting it against another is exactly how "the limiter blocked the wrong
// client" incidents become unexplainable.
//
// scope and dimension are the metric labels of a rejection. They are passed in
// rather than derived from the key, because the key contains the client address (a
// value that must never become a label) and the account name (unbounded, and a
// person's identifier).
//
// keyFn returns "" when there is nothing trustworthy to count against, in which
// case the request passes un-limited (still subject to the other limiters).
func rateLimit(limiter ratelimit.Limiter, m *metrics.Metrics, resolver *ClientIPResolver, limit int, window time.Duration, scope, dimension string, keyFn func(*gin.Context, string) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter == nil || limit <= 0 || window <= 0 {
			c.Next()
			return
		}
		ip := ""
		if resolver != nil {
			ip = resolver.ClientIP(c.Request)
		}
		key := keyFn(c, ip)
		if key == "" {
			c.Next()
			return
		}

		allowed, retryAfter, err := limiter.Allow(c.Request.Context(), key, limit, window)
		if err != nil {
			// Unreachable in production by construction: the limiter injected here
			// is a ratelimit.Fallback, which degrades to in-memory instead of
			// returning an error. If it ever happens, the request is allowed so a
			// broken limiter cannot take the whole system down, and it is logged at
			// Error so nobody can miss it.
			LoggerFrom(c).Error("rate limiter unavailable; request allowed without limiting",
				"error", err, "limit", limit, "window", window.String(), "path", c.Request.URL.Path)
			c.Next()
			return
		}
		if allowed {
			c.Next()
			return
		}

		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			// A Retry-After of 0 invites an immediate retry loop.
			seconds = 1
		}
		c.Writer.Header().Set(headerRetryAfter, strconv.Itoa(seconds))
		// The IP is logged (it is already in every access log line); the account
		// part of the key is not, because it is the identifier of a person who may
		// never have authenticated.
		LoggerFrom(c).Warn("rate limit exceeded",
			"limit", limit,
			"window", window.String(),
			"scope", scope,
			"dimension", dimension,
			"retry_after_s", seconds,
			"ip", ip,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
		)
		// The rejection is counted here, at the one place that knows both the scope
		// and the dimension: a limiter that refuses without being visible is a
		// limiter nobody can tune.
		m.IncRateLimitRejected(scope, dimension)
		RespondError(c, apperr.New(apperr.CodeRateLimited))
	}
}

// peekLoginAccount extracts the account from a login body WITHOUT consuming it.
//
// The body must be restored byte-for-byte, because the handler binds it again
// afterwards; a middleware that ate the body would turn every login into a
// confusing 400. The read is bounded: this runs before any authentication, on an
// endpoint an attacker is expected to hammer.
func peekLoginAccount(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxLoginBodyBytes))
	// Restore first, regardless of what happened above: the handler still needs a
	// readable body to produce a correct error.
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	var payload struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Account)
}
