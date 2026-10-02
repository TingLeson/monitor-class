package ratelimit

import (
	"context"
	"sync"
	"time"
)

// maxEntries bounds the memory limiter's map.
//
// WHY a bound at all: keys are derived from client-controlled input (an IP
// address, an account name), so an attacker can mint unlimited distinct keys.
// Without a cap, the fallback that exists to protect availability would become
// the thing that exhausts memory.
const maxEntries = 100_000

// sweepInterval amortises cleanup: sweeping on every call would make the lock
// hold time grow with the map size on a hot path.
const sweepInterval = 30 * time.Second

// Memory is an in-process fixed-window limiter.
//
// It is NOT the primary limiter: with more than one API replica each process
// would count separately, so the effective limit multiplies by the replica count.
// It exists as the degradation target when Redis is unreachable (see Fallback)
// and as the implementation unit tests use.
type Memory struct {
	mu      sync.Mutex
	entries map[string]*memoryEntry
	lastGC  time.Time
	// now is injectable so window expiry can be tested without sleeping.
	now func() time.Time
}

type memoryEntry struct {
	count   int
	started time.Time
	window  time.Duration
}

// NewMemory creates an empty in-memory limiter.
func NewMemory() *Memory {
	return &Memory{entries: make(map[string]*memoryEntry), now: time.Now}
}

// Allow implements Limiter.
//
// The window is anchored at the first request for a key ("fixed window"), which
// is the cheapest correct-enough policy: it can let 2×limit requests through
// around a boundary, and that is an accepted trade — this limiter is a speed bump
// that makes guessing impractical, not a billing meter.
func (m *Memory) Allow(_ context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if limit <= 0 || window <= 0 {
		// A misconfigured limit must not silently disable protection, and it must
		// not lock everyone out either. Treating it as "allowed" is chosen here
		// because the config layer already rejects non-positive values at boot;
		// reaching this branch means a programming error, which the caller will
		// see as "no limiting" rather than as an outage.
		return true, 0, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.sweepLocked(now)

	entry, ok := m.entries[key]
	if !ok || now.Sub(entry.started) >= entry.window {
		if !ok && len(m.entries) >= maxEntries {
			// The map is full of live entries. Dropping everything is the least
			// bad option: counting precision is already degraded, and refusing new
			// keys would turn a Redis outage into a denial of service for every
			// client that had not been seen yet.
			m.entries = make(map[string]*memoryEntry, maxEntries/10)
		}
		m.entries[key] = &memoryEntry{count: 1, started: now, window: window}
		return true, 0, nil
	}

	if entry.count >= limit {
		return false, entry.window - now.Sub(entry.started), nil
	}
	entry.count++
	return true, 0, nil
}

// sweepLocked drops expired entries, at most once per sweepInterval.
//
// Cleanup is lazy rather than timer-driven: a background goroutine would need a
// lifecycle (start, stop, leak on restart) for a task that costs microseconds
// when it runs.
func (m *Memory) sweepLocked(now time.Time) {
	if now.Sub(m.lastGC) < sweepInterval {
		return
	}
	m.lastGC = now
	for key, entry := range m.entries {
		if now.Sub(entry.started) >= entry.window {
			delete(m.entries, key)
		}
	}
}

// Len reports the number of tracked keys. Exported for tests and diagnostics.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
