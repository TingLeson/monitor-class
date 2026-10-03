package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// The Phase 11 cost question: "what does the hardening and the instrumentation cost
// per request?".
//
// These benchmarks deliberately measure the WHOLE chain (request id, access log at a
// level that discards it, drain gate, metrics, recovery, security headers, body limit,
// CORS, routing) rather than one middleware in isolation, because that is what a
// production request pays. The two variants differ in exactly one thing: whether a
// metrics registry is wired in.

func benchmarkRouter(b *testing.B, withMetrics bool) {
	cfg := testConfig(&testing.T{})
	cfg.LogLevel = 127 // above every level: the access log must not dominate the numbers
	deps := Deps{Logger: discardLogger(), Config: cfg, Ready: ReadinessDeps{}}
	if withMetrics {
		deps.Metrics = metrics.New()
	}
	router := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "198.51.100.7:4000"
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Body.Reset()
		router.ServeHTTP(rec, req)
	}
}

// BenchmarkRequestWithoutMetrics is the baseline: the Phase 10 middleware chain.
func BenchmarkRequestWithoutMetrics(b *testing.B) { benchmarkRouter(b, false) }

// BenchmarkRequestWithMetrics is the same chain with the §77 instrumentation.
func BenchmarkRequestWithMetrics(b *testing.B) { benchmarkRouter(b, true) }

// BenchmarkRouteInstrumentsObserve is the instrumentation's own hot path: what a
// request pays in the metrics middleware after the route cache is warm. It must stay
// allocation-free (0 B/op, 0 allocs/op) or the middleware would add GC pressure to
// every request in the system.
func BenchmarkRouteInstrumentsObserve(b *testing.B) {
	m := metrics.New()
	meter := m.HTTPRoute(http.MethodGet, "/api/v1/teacher/classrooms/:id/monitor")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		meter.Observe(200, 0.003)
	}
}

// BenchmarkMetricsExposition measures the scrape itself: it walks every family and
// renders the text. It is on no request path, so it only has to be cheap enough that a
// 15-second scrape interval is irrelevant.
func BenchmarkMetricsExposition(b *testing.B) {
	m := metrics.New()
	for i := 0; i < 50; i++ {
		m.HTTPRoute(http.MethodGet, "/api/v1/route/"+string(rune('a'+i%26))+"/:id").Observe(200, 0.002)
	}
	m.SetSessionStatus(map[string]int64{"ONLINE": 12, "LEFT": 3})

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		if err := m.WriteExposition(rec); err != nil {
			b.Fatalf("WriteExposition: %v", err)
		}
	}
}

// TestMiddlewareOverheadIsBounded is the guard the benchmarks document: it fails if the
// instrumentation ever becomes a significant fraction of a request. The bound is
// deliberately loose (3x) because a shared CI machine is noisy; the point is to catch a
// change that makes the metrics path allocate per request or take a lock that
// serialises the process, not to police microseconds.
func TestMiddlewareOverheadIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped in -short mode")
	}
	const requests = 1000

	measure := func(withMetrics bool) time.Duration {
		cfg := testConfig(t)
		cfg.LogLevel = 127
		deps := Deps{Logger: discardLogger(), Config: cfg, Ready: ReadinessDeps{}}
		if withMetrics {
			deps.Metrics = metrics.New()
		}
		router := NewRouter(deps)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "198.51.100.7:4000"
		rec := httptest.NewRecorder()

		// Warm up: the first request of a route resolves its instrument bundle.
		router.ServeHTTP(rec, req)
		rec.Body.Reset()

		start := time.Now()
		for i := 0; i < requests; i++ {
			rec.Body.Reset()
			router.ServeHTTP(rec, req)
		}
		return time.Since(start)
	}

	baseline := measure(false)
	withMetrics := measure(true)
	t.Logf("%d requests: baseline=%s (%.2fµs/req) with_metrics=%s (%.2fµs/req) overhead=%.1f%%",
		requests, baseline, float64(baseline.Nanoseconds())/requests/1000,
		withMetrics, float64(withMetrics.Nanoseconds())/requests/1000,
		(float64(withMetrics-baseline)/float64(baseline))*100,
	)

	if withMetrics > 3*baseline {
		t.Fatalf("the metrics middleware tripled the request cost: baseline=%s with_metrics=%s", baseline, withMetrics)
	}
}
