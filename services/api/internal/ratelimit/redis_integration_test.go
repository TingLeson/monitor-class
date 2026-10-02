package ratelimit_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
)

// The Redis limiter needs a real Redis server to be meaningful: the properties
// under test are the atomicity of the Lua script and the expiry it installs,
// neither of which a fake can demonstrate. Skipped unless TEST_REDIS_ADDR is set:
//
//	TEST_REDIS_ADDR=localhost:6380 go test ./internal/ratelimit/ -v

func redisClient(t *testing.T) *goredis.Client {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is not set; skipping Redis integration test")
	}
	client := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: os.Getenv("TEST_REDIS_PASSWORD"),
		// A dedicated database keeps the test from colliding with a development
		// instance that shares the host.
		DB: 9,
	})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis at %s is not reachable: %v", addr, err)
	}
	return client
}

// uniqueKey keeps tests independent without a FLUSHDB, which would be rude to a
// shared instance.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return "test:" + strings.ReplaceAll(t.Name(), "/", "_") + ":" + time.Now().Format("150405.000000000")
}

func TestRedisLimiterAllowsUpToTheLimit(t *testing.T) {
	limiter := ratelimit.NewRedis(redisClient(t))
	ctx := context.Background()
	key := uniqueKey(t)

	for i := 0; i < 3; i++ {
		allowed, retryAfter, err := limiter.Allow(ctx, key, 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
		if !allowed {
			t.Fatalf("request %d rejected, want the first 3 allowed", i+1)
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %v on an allowed request, want 0", retryAfter)
		}
	}

	allowed, retryAfter, err := limiter.Allow(ctx, key, 3, time.Minute)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if allowed {
		t.Fatal("the 4th request was allowed")
	}
	// The TTL comes back from the same atomic script, so Retry-After is the real
	// remaining window rather than a guess.
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("retryAfter = %v, want (0, 1m]", retryAfter)
	}
}

// TestRedisLimiterKeyAlwaysHasATTLY is the regression test for the failure mode
// the Lua script exists to prevent: a counter without an expiry blocks its key
// forever.
func TestRedisLimiterKeyAlwaysHasATTL(t *testing.T) {
	client := redisClient(t)
	limiter := ratelimit.NewRedis(client)
	ctx := context.Background()
	key := uniqueKey(t)

	if _, _, err := limiter.Allow(ctx, key, 1, time.Minute); err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	// The exported key layout is part of the contract with Redis operators
	// (`SCAN MATCH rl:*`), so the test asserts the real key name.
	fullKey := ratelimit.KeyPrefix + "60:" + key
	ttl, err := client.TTL(ctx, fullKey).Result()
	if err != nil {
		t.Fatalf("TTL() failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("key %s has TTL %v, want a positive expiry", fullKey, ttl)
	}
}

func TestRedisLimiterWindowExpires(t *testing.T) {
	limiter := ratelimit.NewRedis(redisClient(t))
	ctx := context.Background()
	key := uniqueKey(t)

	if allowed, _, err := limiter.Allow(ctx, key, 1, time.Second); err != nil || !allowed {
		t.Fatalf("first request = (%v, %v), want allowed", allowed, err)
	}
	if allowed, _, err := limiter.Allow(ctx, key, 1, time.Second); err != nil || allowed {
		t.Fatalf("second request = (%v, %v), want rejected inside the window", allowed, err)
	}

	// The window is one second; Redis expires the key on its own.
	time.Sleep(1100 * time.Millisecond)
	if allowed, _, err := limiter.Allow(ctx, key, 1, time.Second); err != nil || !allowed {
		t.Fatalf("request after the window = (%v, %v), want allowed", allowed, err)
	}
}

// TestRedisLimiterIsSharedBetweenInstances is the reason Redis is the primary
// limiter: two API replicas must count into the same bucket, otherwise the
// effective limit is multiplied by the replica count.
func TestRedisLimiterIsSharedBetweenInstances(t *testing.T) {
	first := ratelimit.NewRedis(redisClient(t))
	second := ratelimit.NewRedis(redisClient(t))
	ctx := context.Background()
	key := uniqueKey(t)

	if allowed, _, err := first.Allow(ctx, key, 2, time.Minute); err != nil || !allowed {
		t.Fatalf("instance 1 first request = (%v, %v), want allowed", allowed, err)
	}
	if allowed, _, err := second.Allow(ctx, key, 2, time.Minute); err != nil || !allowed {
		t.Fatalf("instance 2 first request = (%v, %v), want allowed", allowed, err)
	}
	// The third request is over the limit regardless of which instance sees it.
	if allowed, _, err := second.Allow(ctx, key, 2, time.Minute); err != nil || allowed {
		t.Fatalf("instance 2 third request = (%v, %v), want rejected", allowed, err)
	}
}

func TestRedisLimiterSeparatesWindows(t *testing.T) {
	limiter := ratelimit.NewRedis(redisClient(t))
	ctx := context.Background()
	key := uniqueKey(t)

	// The same logical key under two different policies must not share a counter:
	// otherwise the shorter window would reset the longer one's count.
	if allowed, _, err := limiter.Allow(ctx, key, 1, time.Minute); err != nil || !allowed {
		t.Fatalf("minute window = (%v, %v), want allowed", allowed, err)
	}
	if allowed, _, err := limiter.Allow(ctx, key, 1, time.Hour); err != nil || !allowed {
		t.Fatalf("hour window = (%v, %v), want a separate bucket", allowed, err)
	}
}

func TestRedisLimiterErrorsOnUnreachableServer(t *testing.T) {
	// Port 1 is reserved and never listening. The limiter must report the failure
	// instead of pretending the request is allowed: the Fallback is what turns
	// this error into a degradation, and it can only do that if it sees the error.
	client := goredis.NewClient(&goredis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })

	limiter := ratelimit.NewRedis(client)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	allowed, _, err := limiter.Allow(ctx, "unreachable", 5, time.Minute)
	if err == nil {
		t.Fatal("Allow() reported success against an unreachable Redis")
	}
	if allowed {
		t.Error("Allow() reported allowed=true while returning an error")
	}
}
