// Package ratelimit implements the fixed-window rate limiting §2.2 and §63
// require.
//
// WHY rate limiting is not optional in this system: a student logs in with an
// account name and nothing else, so the only thing standing between "knows a
// name" and "is that student" is how expensive guessing is made. The same
// limiter also protects the password entries from brute force and the API as a
// whole from a runaway client.
//
// A Limiter answers one question — "is this key still under its limit?" — and
// nothing else. Deciding what a key is (client IP, account, route) belongs to the
// HTTP middleware, which is also where the trust decision about proxy headers
// lives.
package ratelimit

import (
	"context"
	"time"
)

// Limiter is the rate-limiting contract.
//
// Allow reports whether the call is permitted, and when it is not, how long the
// caller should wait. Implementations must be safe for concurrent use: every
// request in the process may call the same limiter.
//
// The signature takes limit and window per call rather than at construction
// because different call sites need different policies (10 logins/minute per IP
// vs 5 per account per 10 minutes vs 300 API calls/minute) and they must share
// one backend connection.
//
// An error means "the limiter itself could not answer" — it must never be
// interpreted as "allowed" by a caller. Wrap the limiter in a Fallback (see
// fallback.go) to keep protection instead of losing it when the backend fails.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// KeyPrefix namespaces every key this package writes to Redis.
//
// It keeps rate-limit counters from colliding with session data or presence keys
// in a shared database, and makes them trivially identifiable during an incident
// (`SCAN MATCH rl:*`).
const KeyPrefix = "rl:"
