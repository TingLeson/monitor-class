package ratelimit

import (
	"context"
	"log/slog"
	"time"
)

// Fallback keeps rate limiting working when its primary backend fails.
//
// The trade-off, stated explicitly because both alternatives are tempting:
//
//   - Fail OPEN (treat an error as "allowed") makes an outage of Redis into an
//     outage of the only protection a password-less student login has. An
//     attacker who can make the primary fail — or who simply waits for it to —
//     gets unlimited guesses.
//   - Fail CLOSED (treat an error as "blocked") turns a Redis blip into a total
//     login outage for the whole school.
//
// Degrading to a per-process in-memory limiter keeps both properties that matter:
// the service stays up, and the attempt rate per client stays bounded (the limit
// is only multiplied by the number of replicas, not removed). The event is logged
// at Warn so the degradation is visible in operations rather than silent.
type Fallback struct {
	primary   Limiter
	secondary Limiter
	logger    *slog.Logger
}

// NewFallback builds a degrading limiter. A nil logger falls back to
// slog.Default, and a nil secondary to a fresh in-memory limiter, so a
// misconstructed Fallback still protects rather than panics.
func NewFallback(primary, secondary Limiter, logger *slog.Logger) *Fallback {
	if secondary == nil {
		secondary = NewMemory()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Fallback{primary: primary, secondary: secondary, logger: logger}
}

// Allow implements Limiter.
//
// The returned error is always nil for a healthy secondary: once a decision has
// been made from in-memory state, there is nothing left to fail. The primary's
// error is reported through the log instead, so the HTTP layer never has to
// choose between "500" and "allow".
func (f *Fallback) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if f.primary == nil {
		return f.secondary.Allow(ctx, key, limit, window)
	}
	allowed, retryAfter, err := f.primary.Allow(ctx, key, limit, window)
	if err == nil {
		return allowed, retryAfter, nil
	}
	f.logger.Warn("rate limiter degraded to in-memory fallback",
		"error", err,
		"key", key,
		"limit", limit,
		"window", window.String(),
	)
	return f.secondary.Allow(ctx, key, limit, window)
}

// Secondary exposes the degradation limiter (tests assert against it).
func (f *Fallback) Secondary() Limiter { return f.secondary }
