package media

import (
	"time"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// This file is the §77 instrumentation of the media-plane boundary.
//
// # Why the media client is instrumented and not the session service
//
// Everything the control plane knows about LiveKit goes through this type (see the
// package doc), so counting here counts every call exactly once, whatever the
// caller. Counting in internal/session would measure the session endpoints only and
// would silently miss the classroom close path, the webhook path and any future
// caller — and a metric that only covers part of a dependency is worse than none,
// because it looks complete.
//
// # What the labels answer
//
// `operation` says which call (ensure_room, observe_room, update_subscriptions, …)
// and `result` says whether the control plane had to give up. The duration
// histogram next to it is what turns "加入课堂 is slow" into "ObserveRoom is the
// slow one, and it is slow at p95, not just at the tail".

// WithMetrics attaches the metric set. A nil value is supported: every recording
// then becomes a no-op, which is what the unit tests rely on.
func (c *Client) WithMetrics(m *metrics.Metrics) *Client {
	if c == nil {
		return c
	}
	c.metrics = m
	return c
}

// observeCall records one finished media-plane call.
//
// It is called from a deferred closure with a POINTER to the named return value, so
// it observes the error every return path produced, including the ones that wrap
// (fmt.Errorf("livekit: create room: %w", err)) and the ones that translate
// ("already exists" is success).
func (c *Client) observeCall(operation string, start time.Time, err error) {
	if c == nil {
		return
	}
	result := metrics.MediaResultOK
	if err != nil {
		result = metrics.MediaResultError
	}
	c.metrics.ObserveMediaCall(operation, result, time.Since(start).Seconds())
}
