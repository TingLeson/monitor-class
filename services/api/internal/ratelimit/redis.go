package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// incrementScript is the whole fixed-window algorithm, executed atomically.
//
// WHY Lua instead of INCR followed by EXPIRE:
//
//   - Two round trips leave a window in which the process can die between them.
//     The counter then has no TTL, and that key is blocked forever — an outage
//     caused by the code that exists to prevent abuse.
//   - Redis runs a script to completion without interleaving, so the counter and
//     its expiry always move together.
//
// Returning the TTL in the same call also removes a second round trip for
// Retry-After, which is the number the client actually needs.
const incrementScript = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
    -- Defensive: a key without a TTL (a leftover from a manual SET, or a Redis
    -- restart with a partially restored dataset) would otherwise never expire.
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
    ttl = tonumber(ARGV[1])
end
return {current, ttl}`

// Redis is a fixed-window limiter backed by Redis.
//
// It is the primary limiter because it is shared: all API replicas increment the
// same counter, so the limit means what it says regardless of how many processes
// serve the traffic. Redis is deliberately NOT the Source of Truth for anything
// in this system — losing it degrades this feature to the in-memory fallback
// (see Fallback), it does not change what is true about accounts or classrooms.
type Redis struct {
	client goredis.UniversalClient
	prefix string
	// windowSeconds is derived from the requested window for the key name.
}

// NewRedis wraps a Redis client.
func NewRedis(client goredis.UniversalClient) *Redis {
	return &Redis{client: client, prefix: KeyPrefix}
}

// Allow implements Limiter with an INCR + PEXPIRE script.
func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if r == nil || r.client == nil {
		return false, 0, errors.New("ratelimit: redis limiter is not connected")
	}
	if limit <= 0 || window <= 0 {
		return true, 0, nil
	}

	fullKey := r.fullKey(key, window)
	windowMillis := window.Milliseconds()
	if windowMillis <= 0 {
		windowMillis = 1
	}

	raw, err := r.client.Eval(ctx, incrementScript, []string{fullKey}, windowMillis).Result()
	if err != nil {
		// The caller (Fallback) turns this into a degradation, never into an
		// allow. Returning the error is the honest answer.
		return false, 0, fmt.Errorf("ratelimit: redis eval: %w", err)
	}

	count, ttlMillis, err := parseScriptResult(raw)
	if err != nil {
		return false, 0, err
	}
	retryAfter := time.Duration(ttlMillis) * time.Millisecond
	if int(count) > limit {
		if retryAfter <= 0 {
			// A zero Retry-After would tell the client to retry immediately and
			// hammer the endpoint it was just blocked from.
			retryAfter = window
		}
		return false, retryAfter, nil
	}
	return true, 0, nil
}

// fullKey namespaces the counter by window length.
//
// WHY the window is part of the key: the same logical key (say, one IP) may be
// limited by two policies with different windows, and sharing one counter between
// them would let the shorter window's expiry reset the longer one's count.
func (r *Redis) fullKey(key string, window time.Duration) string {
	return fmt.Sprintf("%s%d:%s", r.prefix, int64(window.Seconds()), key)
}

// parseScriptResult converts the Lua reply ([count, ttlMillis]) into Go values.
func parseScriptResult(raw any) (int64, int64, error) {
	values, ok := raw.([]any)
	if !ok || len(values) != 2 {
		return 0, 0, fmt.Errorf("ratelimit: unexpected redis reply %T", raw)
	}
	count, ok := values[0].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("ratelimit: unexpected count type %T", values[0])
	}
	ttl, ok := values[1].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("ratelimit: unexpected ttl type %T", values[1])
	}
	return count, ttl, nil
}
