package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
)

// The metrics sampler is the only part of cmd/api with logic worth testing: it turns
// a census query and two library stats into gauges, and it must never panic or zero a
// gauge when a dependency is missing. Everything else in this package is wiring that
// the httpapi integration suite already exercises end to end.

func samplerTestConfig() *config.Config {
	return &config.Config{MetricsSessionRefreshInterval: time.Second}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func exposition(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestMetricsSamplerPublishesEveryGauge runs the real census query against a migrated
// database and asserts that all SIX statuses are published.
//
// WHY all six and not only the non-zero ones: a status that disappears from the
// exposition (because it has no rows) is indistinguishable from a broken sampler, and a
// gauge that is set only when non-zero keeps its last value forever — the classic
// "gauge that cannot go down" bug this assertion exists to prevent.
func TestMetricsSamplerPublishesEveryGauge(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	// Force at least one connection so the pool statistics are non-trivial.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	m := metrics.New()
	sessions := session.NewService(nil, nil, nil, session.Config{}).WithMetrics(m)
	sampler := newMetricsSampler(discardLogger(), samplerTestConfig(), m, pool, sessions)
	if sampler == nil {
		t.Fatal("newMetricsSampler returned nil with a valid metrics registry")
	}
	sampler.sample(ctx)

	text := exposition(t, m)
	for _, status := range metrics.SessionStatuses {
		want := `classwatch_session_status{status="` + status + `"}`
		if !strings.Contains(text, want) {
			t.Errorf("the census did not publish %s:\n%s", want, text)
		}
	}
	for _, state := range []string{"acquired", "idle", "total"} {
		want := `classwatch_db_pool_connections{state="` + state + `"}`
		if !strings.Contains(text, want) {
			t.Errorf("the sampler did not publish %s:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `classwatch_private_talk_active 0`) {
		t.Errorf("the sampler did not publish the private-talk gauge:\n%s", text)
	}

	// The loop must stop when its context is cancelled: a goroutine that outlives the
	// pool it reads is a shutdown hang waiting to happen.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampler.run(runCtx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the sampler did not stop when its context was cancelled")
	}
}

// TestMetricsSamplerToleratesMissingDependencies is the degraded-deployment test: an
// API started with STARTUP_REQUIRE_DEPENDENCIES=false has no pool and no session
// service, and the sampler must still run (and still serve /metrics) instead of
// panicking on a nil dereference.
func TestMetricsSamplerToleratesMissingDependencies(t *testing.T) {
	m := metrics.New()
	sampler := newMetricsSampler(discardLogger(), samplerTestConfig(), m, nil, nil)
	sampler.sample(context.Background())

	text := exposition(t, m)
	// The gauges keep their initialised values (0) rather than disappearing.
	for _, status := range metrics.SessionStatuses {
		if !strings.Contains(text, `classwatch_session_status{status="`+status+`"} 0`) {
			t.Errorf("a missing database removed the session gauge %s:\n%s", status, text)
		}
	}

	// A nil registry must disable the sampler entirely rather than half-run it.
	if sampler := newMetricsSampler(discardLogger(), samplerTestConfig(), nil, nil, nil); sampler != nil {
		t.Fatal("a nil metrics registry must produce no sampler")
	}
	var nilSampler *metricsSampler
	nilSampler.sample(context.Background())
	nilSampler.run(context.Background())
}
