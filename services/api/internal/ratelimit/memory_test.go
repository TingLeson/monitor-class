package ratelimit

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryAllowsUpToTheLimit(t *testing.T) {
	m := NewMemory()
	now := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, retryAfter, err := m.Allow(ctx, "key", 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
		if !allowed {
			t.Fatalf("request %d was rejected, want the first 3 allowed", i+1)
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %v on an allowed request, want 0", retryAfter)
		}
	}

	allowed, retryAfter, err := m.Allow(ctx, "key", 3, time.Minute)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if allowed {
		t.Fatal("the 4th request was allowed, want it rejected")
	}
	// The client needs a usable number: a zero Retry-After would invite an
	// immediate retry loop.
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("retryAfter = %v, want (0, 1m]", retryAfter)
	}
}

func TestMemoryWindowExpires(t *testing.T) {
	m := NewMemory()
	now := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx := context.Background()

	if allowed, _, _ := m.Allow(ctx, "key", 1, time.Minute); !allowed {
		t.Fatal("first request rejected")
	}
	if allowed, _, _ := m.Allow(ctx, "key", 1, time.Minute); allowed {
		t.Fatal("second request allowed inside the window")
	}

	// One second before the window ends the counter must still hold...
	now = now.Add(59 * time.Second)
	if allowed, _, _ := m.Allow(ctx, "key", 1, time.Minute); allowed {
		t.Fatal("request allowed 59s into a 60s window")
	}
	// ...and at the boundary it must be a fresh window.
	now = now.Add(2 * time.Second)
	if allowed, _, _ := m.Allow(ctx, "key", 1, time.Minute); !allowed {
		t.Fatal("request rejected after the window expired")
	}
}

func TestMemoryKeysAreIndependent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	if allowed, _, _ := m.Allow(ctx, "ip:1.2.3.4", 1, time.Minute); !allowed {
		t.Fatal("first key rejected")
	}
	if allowed, _, _ := m.Allow(ctx, "ip:1.2.3.4", 1, time.Minute); allowed {
		t.Fatal("second request for the same key allowed")
	}
	// A different client (or a different policy bucket) must not inherit the
	// exhausted count.
	if allowed, _, _ := m.Allow(ctx, "ip:5.6.7.8", 1, time.Minute); !allowed {
		t.Fatal("an unrelated key was rejected")
	}
}

func TestMemorySweepsExpiredEntries(t *testing.T) {
	m := NewMemory()
	now := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		if _, _, err := m.Allow(ctx, string(rune('a'+i%26))+string(rune('0'+i/26)), 5, time.Minute); err != nil {
			t.Fatalf("Allow() failed: %v", err)
		}
	}
	if m.Len() == 0 {
		t.Fatal("no entries were tracked")
	}

	// Past the window AND past the sweep interval, the next call must clean up:
	// this is the lazy cleanup that keeps a long-running process from growing its
	// map forever.
	now = now.Add(2 * time.Minute)
	if _, _, err := m.Allow(ctx, "fresh", 5, time.Minute); err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	if got := m.Len(); got != 1 {
		t.Errorf("Len() = %d after the sweep, want 1 (only the new key)", got)
	}
}

func TestMemoryCapStopsUnboundedGrowth(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	// Fill the map to the cap with live entries, which is what an attacker
	// generating unique keys would do.
	for i := 0; i < maxEntries; i++ {
		m.entries["attacker-"+strconv.Itoa(i)] = &memoryEntry{count: 1, started: time.Now(), window: time.Hour}
	}
	if m.Len() < maxEntries {
		t.Fatalf("setup failed: Len() = %d", m.Len())
	}

	allowed, _, err := m.Allow(ctx, "one-more-key", 5, time.Hour)
	if err != nil {
		t.Fatalf("Allow() failed: %v", err)
	}
	// Availability wins over counting precision at the cap: the request is still
	// allowed, and the map is bounded.
	if !allowed {
		t.Error("request rejected at the cap; a Redis outage must not become a denial of service")
	}
	if got := m.Len(); got > maxEntries {
		t.Errorf("Len() = %d, want it bounded by %d", got, maxEntries)
	}
}

func TestMemoryNonPositiveLimitAllows(t *testing.T) {
	m := NewMemory()
	// A zero limit is a programming error, and config.Load rejects it at boot.
	// The behaviour here is documented as "allow" so a misconfiguration cannot
	// lock every user out; the test pins that decision.
	if allowed, _, err := m.Allow(context.Background(), "key", 0, time.Minute); !allowed || err != nil {
		t.Errorf("Allow(limit=0) = (%v, %v), want (true, nil)", allowed, err)
	}
	if allowed, _, err := m.Allow(context.Background(), "key", 5, 0); !allowed || err != nil {
		t.Errorf("Allow(window=0) = (%v, %v), want (true, nil)", allowed, err)
	}
}

func TestMemoryIsConcurrencySafe(t *testing.T) {
	m := NewMemory()
	const limit = 10
	var allowedCount int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _, err := m.Allow(context.Background(), "shared-key", limit, time.Minute)
			if err != nil {
				t.Errorf("Allow() failed: %v", err)
				return
			}
			if allowed {
				atomic.AddInt64(&allowedCount, 1)
			}
		}()
	}
	wg.Wait()

	// Exactly `limit` requests may pass: the mutex must make the check-and-
	// increment atomic, or a burst would slip past the limit.
	if allowedCount != limit {
		t.Errorf("allowed = %d, want exactly %d", allowedCount, limit)
	}
}
