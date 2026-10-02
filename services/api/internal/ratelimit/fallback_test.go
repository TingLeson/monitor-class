package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// failingLimiter stands in for an unreachable Redis.
type failingLimiter struct {
	err   error
	calls int
}

func (f *failingLimiter) Allow(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	f.calls++
	return false, 0, f.err
}

// recordingLimiter returns a fixed decision.
type recordingLimiter struct {
	allowed    bool
	retryAfter time.Duration
	calls      int
}

func (r *recordingLimiter) Allow(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	r.calls++
	return r.allowed, r.retryAfter, nil
}

// TestFallbackDegradesInsteadOfFailingOpen is the security property that matters:
// when the primary limiter is broken the request must still be COUNTED (and
// therefore blockable), not waved through.
func TestFallbackDegradesInsteadOfFailingOpen(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	primary := &failingLimiter{err: errors.New("dial tcp: connection refused")}
	memory := NewMemory()
	fallback := NewFallback(primary, memory, logger)
	ctx := context.Background()

	// The limit is 2: the third attempt must be rejected by the in-memory
	// fallback even though the primary answers nothing but errors.
	for i := 0; i < 2; i++ {
		allowed, _, err := fallback.Allow(ctx, "login:ip:203.0.113.9", 2, time.Minute)
		if err != nil {
			t.Fatalf("Allow() returned an error despite a working fallback: %v", err)
		}
		if !allowed {
			t.Fatalf("attempt %d was rejected, want the first 2 allowed", i+1)
		}
	}
	allowed, retryAfter, err := fallback.Allow(ctx, "login:ip:203.0.113.9", 2, time.Minute)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if allowed {
		t.Fatal("the third attempt was allowed: the fallback failed open")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want a positive duration", retryAfter)
	}

	if primary.calls != 3 {
		t.Errorf("primary calls = %d, want 3 (it must be tried every time)", primary.calls)
	}
	// Degradation is an operational event, not a silent one.
	if !strings.Contains(logs.String(), "degraded") {
		t.Errorf("degradation was not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "connection refused") == false {
		t.Errorf("the underlying cause is missing from the log:\n%s", logs.String())
	}
}

func TestFallbackUsesPrimaryWhenHealthy(t *testing.T) {
	primary := &recordingLimiter{allowed: false, retryAfter: 42 * time.Second}
	secondary := NewMemory()
	fallback := NewFallback(primary, secondary, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	allowed, retryAfter, err := fallback.Allow(context.Background(), "key", 5, time.Minute)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if allowed || retryAfter != 42*time.Second {
		t.Errorf("Allow() = (%v, %v), want the primary's answer", allowed, retryAfter)
	}
	if primary.calls != 1 {
		t.Errorf("primary calls = %d, want 1", primary.calls)
	}
	if secondary.Len() != 0 {
		t.Error("the fallback was used while the primary was healthy")
	}
}

func TestFallbackWithoutPrimaryIsTheSecondary(t *testing.T) {
	// A nil primary must not panic; it degrades immediately.
	fallback := NewFallback(nil, NewMemory(), nil)
	allowed, _, err := fallback.Allow(context.Background(), "key", 1, time.Minute)
	if err != nil || !allowed {
		t.Fatalf("Allow() = (%v, %v), want (true, nil)", allowed, err)
	}
	if fallback.Secondary() == nil {
		t.Error("Secondary() = nil")
	}
}
