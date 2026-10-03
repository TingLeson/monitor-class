package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/livekit/protocol/livekit"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
)

// LiveKitVerifier verifies the signature of a LiveKit webhook and parses it (§45).
//
// The interface is declared here, next to the handler that needs it, because it is the
// handler's requirement and not the media package's design: the implementation
// (internal/media.WebhookVerifier) does not know it is being used behind HTTP, and the
// unit tests substitute a fake that always fails — which is how "a forged webhook is a
// 401" can be tested without forging a JWT.
type LiveKitVerifier interface {
	Receive(r *http.Request) (*livekit.WebhookEvent, error)
}

// MediaEventProcessor applies one VERIFIED webhook to the control plane (§45/§74).
//
// It receives the livekit event because the translation from the vendor's vocabulary to
// this project's state machine is session domain logic (track source → SCREEN_SHARE,
// participant identity → session row); doing it in the HTTP layer would put a state
// decision in a transport adapter.
type MediaEventProcessor interface {
	ProcessWebhook(ctx context.Context, event *livekit.WebhookEvent) error
}

// webhookHandler serves POST /internal/livekit/webhook (§45).
//
// # Why this route has no session, no CSRF token and no rate limit
//
// The caller is LiveKit's own infrastructure, not a browser: it has no cookie, cannot
// read a CSRF token and cannot solve a per-IP budget without the whole fleet sharing
// one bucket (LiveKit Cloud calls from a small set of egress addresses, so an IP limiter
// would throttle every school at once and, worse, drop state transitions that nothing
// retries forever). The authentication of this endpoint is the SIGNATURE — an HMAC over
// the exact request body, verified against a secret that only LiveKit and this process
// hold (§63) — and that is exactly why the route is mounted OUTSIDE /api/v1: the coarse
// per-IP limiter lives on that group, and this path must not be behind it. A caller
// without the secret is rejected before any state is touched.
//
// # Failure policy
//
//   - Signature failure → 401. Deliberately NOT 200: a 200 would make a forged or
//     misconfigured sender believe its events are being applied, and the log line is the
//     only place the difference is visible. The response body is the standard error
//     envelope and says nothing about which half of the check failed.
//   - Unknown event, unknown identity, duplicate delivery, out-of-order delivery → 200.
//     These are normal (a teacher participant, an event type this phase ignores,
//     at-least-once delivery) and a 4xx would make LiveKit retry an event that can never
//     succeed — a retry storm against our own endpoint (§45).
//   - The control plane could not record what it observed (database down) → 500, so
//     LiveKit retries. Every transition is a conditional update, so a retry is harmless,
//     and silently dropping the observation would leave a session row the teacher's wall
//     trusts in a state the media plane already left.
func webhookHandler(verifier LiveKitVerifier, processor MediaEventProcessor, resolver *ClientIPResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		event, err := verifier.Receive(c.Request)
		if err != nil {
			// Warn, with the remote address and NOTHING else: the request body is
			// unauthenticated input, and the Authorization header is a credential
			// (§59). The remote address is what an operator needs to tell a
			// misconfigured sender from an actual attack.
			LoggerFrom(c).Warn("livekit webhook rejected",
				"action", "livekit_webhook_rejected",
				"remote_ip", clientIPOf(c, resolver),
				"path", c.Request.URL.Path,
				"error", err,
				"note", "signature verification failed; the body and the header are not logged",
			)
			RespondError(c, apperr.New(apperr.CodeAuthRequired))
			return
		}

		if err := processor.ProcessWebhook(c.Request.Context(), event); err != nil {
			// Logged at Error with the internal cause, answered as 500 so LiveKit
			// retries. The client never sees the cause (it can contain SQL).
			LoggerFrom(c).Error("livekit webhook could not be applied",
				"action", "livekit_webhook_failed",
				"remote_ip", clientIPOf(c, resolver),
				"livekit_event", event.GetEvent(),
				"webhook_event_id", event.GetId(),
				"error", err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{"status": "retry"})
			return
		}

		// The body is deliberately three tokens: this endpoint is called by a machine
		// and must not do work whose result nobody reads.
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// clientIPOf returns the resolved client address for a log line.
//
// The resolver is the same one the access log and the limiters use (trusted proxies
// applied once), so an investigation cannot see two different "remote" addresses for one
// request. A nil resolver (a test that built a handler directly) falls back to the raw
// value, which is what the field would have contained anyway.
func clientIPOf(c *gin.Context, resolver *ClientIPResolver) string {
	if c.Request == nil {
		return ""
	}
	if resolver == nil {
		return c.Request.RemoteAddr
	}
	return resolver.ClientIP(c.Request)
}

// registerWebhookRoute mounts POST /internal/livekit/webhook (§45).
//
// The router installs no authentication middleware on it — no session, no CSRF token, no
// rate limit and no CSRF cookie — because none of them can apply to a machine caller; the
// signature check inside the handler is the whole of its authentication, and the handler
// rejects an unsigned request with 401 before touching any state. See webhookHandler for
// the full reasoning about the limiter.
func registerWebhookRoute(router *gin.Engine, deps Deps, resolver *ClientIPResolver) {
	if deps.Webhook == nil || deps.Webhook.Verifier == nil || deps.Webhook.Processor == nil {
		// Partially wired: the route is not registered at all, so a request answers 404
		// instead of reaching an endpoint that would either accept unverified events or
		// drop verified ones.
		return
	}
	router.POST("/internal/livekit/webhook",
		webhookHandler(deps.Webhook.Verifier, deps.Webhook.Processor, resolver))
}
