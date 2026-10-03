package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
)

// This file holds the Phase 11 transport hardening of §63/§77: the security
// response headers, the global body cap and the shutdown drain gate. They are
// middlewares rather than per-handler code because each one must apply to EVERY
// response — including the ones a handler never reaches (404, 405, a refused
// origin, a panic) — and a rule enforced at one call site is a rule that the next
// endpoint will not have.

// Security response headers.
//
// The API only ever returns JSON (or, on /metrics, text), so these are the
// strictest values that are still correct:
//
//   - X-Content-Type-Options: nosniff — a browser must never guess that a JSON
//     body is HTML. Without it, an uploaded or echoed string that looks like HTML
//     can be rendered as one in an old browser.
//   - X-Frame-Options: DENY — an API response is never a document to frame, and
//     framing is the delivery mechanism for clickjacking.
//   - Referrer-Policy: no-referrer — a URL in this API can carry an identifier,
//     and a referrer leaks it to whatever a user clicks next.
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none' — the
//     belt to nosniff's braces: nothing in this response may load anything. The
//     FRONTEND's CSP is a different policy, set by the reverse proxy for the HTML
//     it serves (see docs/architecture/observability.md and the Caddy config); a
//     strict CSP here cannot break the apps, because the API serves no HTML.
//
// Strict-Transport-Security is NOT in this list: see requestIsHTTPS.
const (
	headerContentTypeOptions = "X-Content-Type-Options"
	headerFrameOptions       = "X-Frame-Options"
	headerReferrerPolicy     = "Referrer-Policy"
	headerCSP                = "Content-Security-Policy"
	headerHSTS               = "Strict-Transport-Security"
	headerForwardedProto     = "X-Forwarded-Proto"
)

// SecurityHeadersMiddleware sets the API's security response headers.
func SecurityHeadersMiddleware(resolver *ClientIPResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set(headerContentTypeOptions, "nosniff")
		h.Set(headerFrameOptions, "DENY")
		h.Set(headerReferrerPolicy, "no-referrer")
		h.Set(headerCSP, "default-src 'none'; frame-ancestors 'none'")

		if requestIsHTTPS(c.Request, resolver) {
			// HSTS is only sent over HTTPS, and this is not a style choice: a
			// browser that receives HSTS over plain HTTP (or over a response it
			// believes is plain HTTP) pins the host to HTTPS, and on a local
			// development setup — http://localhost:8090 — that means the developer's
			// browser refuses the next plain-HTTP request for max-age, with no way
			// back except clearing the HSTS state. A one-year max-age belongs to a
			// real certificate, not to `go run`.
			//
			// includeSubDomains is deliberately NOT set: the api host is one of
			// several (student/teacher/admin/rtc), and pinning the parent domain
			// from here would be a decision about infrastructure this service does
			// not own.
			h.Set(headerHSTS, "max-age=31536000")
		}
		c.Next()
	}
}

// requestIsHTTPS reports whether the CLIENT reached us over TLS.
//
// Behind the production reverse proxy (§62) this process sees plain HTTP on the
// loopback/LAN: the TLS session ends at Caddy. The only evidence of the client's
// scheme is X-Forwarded-Proto, and believing that header from an untrusted peer
// would let a caller choose whether HSTS is emitted. So the header is consulted
// ONLY when the direct peer is a configured trusted proxy — the same rule, and the
// same resolver, that decides which X-Forwarded-For hop to believe.
//
// The failure mode of getting this wrong is asymmetric and that is why the default
// is "not HTTPS": a missing HSTS header is a hardening gap an operator notices in
// a scan, while an HSTS header on a local HTTP setup locks a developer out of
// their own machine.
func requestIsHTTPS(req *http.Request, resolver *ClientIPResolver) bool {
	if req == nil {
		return false
	}
	if req.TLS != nil {
		return true
	}
	_, peer := remoteHost(req.RemoteAddr)
	if peer == nil || resolver == nil || !resolver.TrustsProxy(*peer) {
		return false
	}
	// The leftmost value is the client's own claim, so use the LAST hop: the one
	// the trusted proxy appended.
	proto := lastForwardedProto(req.Header.Get(headerForwardedProto))
	return proto == "https"
}

// lastForwardedProto reads the rightmost entry of an X-Forwarded-Proto list.
func lastForwardedProto(value string) string {
	if value == "" {
		return ""
	}
	last := ""
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			last = strings.ToLower(trimmed)
		}
	}
	return last
}

// BodyLimitMiddleware caps every request body (§63).
//
// Two layers, because either one alone has a hole:
//
//   - A declared Content-Length above the limit is refused immediately, before the
//     body is read at all. That is the honest-client case, and refusing it early
//     means an accidental 500 MB upload costs nothing.
//   - http.MaxBytesReader bounds the actual read, which is what covers a chunked
//     request (no Content-Length) and a client that lies about its length. The
//     reader also marks the response so net/http closes the connection instead of
//     trying to drain the rest.
//
// The per-endpoint limits in the handlers (8 KiB for a login, 256 KiB for a
// classroom) nest inside this one and are tighter; this is the backstop that makes
// "a new endpoint forgot to bound its body" impossible.
func BodyLimitMiddleware(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit <= 0 || c.Request == nil {
			c.Next()
			return
		}
		if c.Request.ContentLength > limit {
			// No body is read: the client is told immediately, and the connection
			// is closed by the writer below.
			LoggerFrom(c).Warn("request body rejected",
				"action", "request_body_too_large",
				"content_length", c.Request.ContentLength,
				"limit", limit,
			)
			RespondError(c, payloadTooLarge())
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

// payloadTooLarge builds the 413 (§58's new code).
func payloadTooLarge() *apperr.Error {
	return apperr.New(apperr.CodePayloadTooLarge)
}

// isBodyTooLarge reports whether a body read failed because it hit the cap.
//
// WHY this exists instead of "any read error is a 400": a body that hit the limit
// is the client's size problem (413), while a truncated or broken stream is a
// transport problem (400) — and the two must not be one code, because the first
// one is actionable ("send less") and the second is not.
func isBodyTooLarge(err error) bool {
	var maxBytes *http.MaxBytesError
	return errors.As(err, &maxBytes)
}

// DrainGate makes "this process is going away" observable to the requests that
// arrive during the drain window.
//
// # Why a flag and not just http.Server.Shutdown
//
// Shutdown stops the LISTENERS immediately, so a brand-new connection is refused
// at the TCP level. It does not touch a connection that is already open: an
// HTTP/1.1 keep-alive connection, or an HTTP/2 stream, can still deliver requests
// to this process. Those requests would either be served normally (the process is
// about to exit mid-response) or dropped (the client sees a reset it cannot
// distinguish from a network failure). Answering 503 SERVICE_UNAVAILABLE with
// Retry-After is the only response that tells the client the truth: nothing is
// broken, try again — and it is what makes a rolling deploy invisible instead of
// flaky.
//
// The gate is checked before any handler runs but after RequestID, so the refusal
// is still logged and correlated.
type DrainGate struct {
	draining atomic.Bool
}

// NewDrainGate creates a gate in the "serving" state.
func NewDrainGate() *DrainGate { return &DrainGate{} }

// BeginDraining flips the gate. It is idempotent: a second SIGTERM during the
// drain window must not panic or un-flip anything.
func (g *DrainGate) BeginDraining() {
	if g == nil {
		return
	}
	g.draining.Store(true)
}

// Draining reports the current state.
func (g *DrainGate) Draining() bool {
	if g == nil {
		return false
	}
	return g.draining.Load()
}

// Middleware refuses new requests once the gate is open.
//
// The probes are exempt on purpose: an orchestrator must still be able to ask
// "are you alive?" while the process drains (a liveness probe that fails during a
// rollout turns a graceful deploy into a SIGKILL), and /metrics must stay
// scrapable so the last scrape before exit is a complete one. /readyz is NOT
// exempt: reporting "not ready" is exactly how a load balancer is told to stop
// sending traffic here.
func (g *DrainGate) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.Draining() {
			c.Next()
			return
		}
		switch c.Request.URL.Path {
		case "/healthz", "/metrics":
			c.Next()
			return
		}
		// Retry-After is what separates "retry now" from a retry storm during the
		// seconds the old process is still finishing its work.
		c.Writer.Header().Set(headerRetryAfter, "1")
		LoggerFrom(c).Info("request refused during shutdown drain",
			"action", "drain_refused",
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
		)
		RespondError(c, apperr.New(apperr.CodeServiceUnavailable))
	}
}
