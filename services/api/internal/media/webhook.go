package media

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
)

// WebhookVerifier verifies the signature of an incoming LiveKit webhook and parses
// it (§45).
//
// WHY the LiveKit SDK does the verification and this project does not: the header
// carries a signed JWT whose claims include a SHA-256 of the exact request body.
// Re-implementing that check means re-implementing constant-time comparison, body
// hashing and key selection — three places to get it subtly wrong, in the one
// endpoint that is reachable from the public internet and has no session, no CSRF
// token and no rate limit in front of it. `webhook.ReceiveWebhookEvent` is the
// vendor's implementation of exactly this contract, and the signature check is the
// whole of the endpoint's authentication (§63).
//
// The type lives in this package because internal/media is the entire Media Plane
// boundary: the LiveKit key and secret exist only here and in the wiring, and no
// other package parses a LiveKit protocol message.
type WebhookVerifier struct {
	provider auth.KeyProvider
}

// apiKeyProvider maps the key id carried in a webhook's Authorization header onto
// the one secret this deployment knows.
//
// WHY a provider and not "compare against the configured key": the SDK asks for the
// secret OF THE KEY THE SENDER CLAIMED. Answering only for our own key id means a
// token signed with a different key id is rejected before any signature comparison —
// i.e. a forged webhook cannot even choose the secret we validate against.
type apiKeyProvider struct {
	key    string
	secret string
}

func (p apiKeyProvider) GetSecret(key string) string {
	if key != p.key {
		// Empty = "no such key". The SDK turns that into ErrSecretNotFound, which the
		// handler answers with 401 and a Warn line.
		return ""
	}
	return p.secret
}

func (p apiKeyProvider) NumKeys() int {
	if p.key == "" || p.secret == "" {
		return 0
	}
	return 1
}

// NewWebhookVerifier builds a verifier from the LiveKit API key pair.
//
// An empty key or secret is a configuration error and not a verifier that accepts
// everything: a process that cannot check signatures must not expose the endpoint
// at all (main.go simply does not register the route), because an unverified
// webhook endpoint lets anybody mark any student ONLINE.
func NewWebhookVerifier(apiKey, apiSecret string) (*WebhookVerifier, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("livekit: api key is required to verify webhooks")
	}
	if strings.TrimSpace(apiSecret) == "" {
		// Presence, never the value (§59).
		return nil, fmt.Errorf("livekit: api secret is required to verify webhooks")
	}
	return &WebhookVerifier{provider: apiKeyProvider{key: apiKey, secret: apiSecret}}, nil
}

// Receive verifies the request and returns the parsed event.
//
// Every failure (missing header, unknown key id, bad signature, body that is not
// the JSON the signature covers) is returned as an error and is NOT distinguished
// for the caller: the HTTP layer answers 401 for all of them, because telling an
// attacker which half of a forgery was wrong is free help.
func (v *WebhookVerifier) Receive(r *http.Request) (*livekit.WebhookEvent, error) {
	if v == nil || v.provider == nil {
		return nil, fmt.Errorf("livekit: webhook verifier is not configured")
	}
	event, err := webhook.ReceiveWebhookEvent(r, v.provider)
	if err != nil {
		// The error text is the SDK's own taxonomy (no such header, unknown secret,
		// checksum mismatch) and is safe to log: it contains no header value, no
		// token and no body.
		return nil, fmt.Errorf("livekit: webhook verification failed: %w", err)
	}
	return event, nil
}
