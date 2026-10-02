// Package redis owns the Redis client.
//
// Phase 1 onwards builds several things on this single client:
//
//   - Session store: server-side sessions referenced by an opaque HttpOnly
//     cookie, so "log out" and "disable account" take effect immediately instead
//     of when a stateless token happens to expire.
//   - Rate limit: login attempts and join requests.
//   - Short-lived presence: who is currently connected, expiring on its own so a
//     crashed browser cannot leave a student "online" forever.
//   - WebSocket fan-out: routing teacher-console events to the API instance that
//     holds a given connection.
//
// Phase 0 intentionally implements none of that logic — it only establishes the
// connection and the health check. Redis is never the Source of Truth: everything
// durable lives in PostgreSQL, so losing Redis degrades freshness, not correctness.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config is the minimal connection description this package needs.
//
// It is a local struct rather than *config.Config so the package stays testable
// and so a future second Redis (cache vs. pub/sub) can be added without
// reworking the call sites.
type Config struct {
	Addr     string
	Password string
	DB       int
}

// Client wraps *goredis.Client.
type Client struct {
	client *goredis.Client
}

// Connect builds the client and verifies reachability.
//
// go-redis dials lazily, so without this Ping a typo in REDIS_ADDR would only
// surface on the first user request. Pinging here keeps the failure at boot,
// where it is readable.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		// Timeouts are short on purpose: Redis is an accelerator here, and a slow
		// Redis must never hold an HTTP handler open. Callers still pass their own
		// context deadline for finer control.
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     10,
	})

	wrapped := &Client{client: client}
	if err := wrapped.Ping(ctx); err != nil {
		// Close immediately: a failed client still owns pool resources.
		_ = client.Close()
		return nil, fmt.Errorf("ping redis at %s: %w", cfg.Addr, err)
	}
	return wrapped, nil
}

// Client exposes the raw client so Phase 1+ code can use any command.
func (c *Client) Client() *goredis.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Ping verifies connectivity with a caller-controlled timeout.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("redis: not connected")
	}
	return c.client.Ping(ctx).Err()
}

// Close releases the pool. Safe to call on a nil receiver or twice.
func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	err := c.client.Close()
	c.client = nil
	return err
}
