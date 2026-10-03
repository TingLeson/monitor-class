package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this package are the specification of the exposition format: the
// format is hand-written, so it is only as correct as these assertions.

func TestCounterVecConcurrentIncrement(t *testing.T) {
	registry := NewRegistry()
	counter := registry.NewCounterVec("test_events_total", "test counter", "kind")

	const goroutines = 32
	const perGoroutine = 500

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle := counter.With("screen")
			for j := 0; j < perGoroutine; j++ {
				handle.Inc()
			}
		}()
	}
	wg.Wait()

	if got, want := counter.With("screen").Value(), uint64(goroutines*perGoroutine); got != want {
		t.Fatalf("counter = %d, want %d", got, want)
	}
	if got := counter.With("camera").Value(); got != 0 {
		t.Fatalf("unrelated series = %d, want 0 (labels must not share storage)", got)
	}
}

func TestGaugeSetAddDec(t *testing.T) {
	registry := NewRegistry()
	gauge := registry.NewGaugeVec("test_gauge", "test gauge", "role")

	handle := gauge.With("teacher")
	handle.Add(3)
	handle.Dec()
	if got := handle.Value(); got != 2 {
		t.Fatalf("gauge = %d, want 2", got)
	}
	handle.Set(-1)
	if got := handle.Value(); got != -1 {
		t.Fatalf("gauge = %d, want -1 (gauges may go negative)", got)
	}
	// A second handle for the same labels must observe the same series.
	if got := gauge.With("teacher").Value(); got != -1 {
		t.Fatalf("re-resolved gauge = %d, want -1", got)
	}
}

func TestHistogramBucketsSumAndCount(t *testing.T) {
	registry := NewRegistry()
	histogram := registry.NewHistogramVec("test_duration_seconds", "test histogram",
		[]float64{0.1, 0.5, 1}, "route")

	handle := histogram.With("/api/v1/x")
	handle.Observe(0.05)                    // bucket 0.1
	handle.Observe(0.1)                     // exactly on the boundary: <= is the bucket rule
	handle.Observe(0.4)                     // bucket 0.5
	handle.Observe(1.0)                     // bucket 1
	handle.Observe(3.5)                     // +Inf
	handle.ObserveDuration(2 * time.Second) // +Inf

	if got, want := handle.Count(), uint64(6); got != want {
		t.Fatalf("count = %d, want %d", got, want)
	}
	for index, want := range []uint64{2, 1, 1, 2} {
		if got := handle.BucketCount(index); got != want {
			t.Errorf("bucket %d = %d, want %d", index, got, want)
		}
	}
	if got, want := handle.Sum(), 0.05+0.1+0.4+1.0+3.5+2.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("sum = %v, want %v", got, want)
	}
}

func TestExpositionRendersHistogramWithInfBucket(t *testing.T) {
	registry := NewRegistry()
	histogram := registry.NewHistogramVec("test_duration_seconds", "a \"quoted\" help", []float64{0.5, 1}, "route")
	histogram.With("/x").Observe(0.25)
	histogram.With("/x").Observe(0.75)
	histogram.With("/x").Observe(4)

	text := render(t, registry)

	for _, want := range []string{
		// Quotes are NOT special in HELP text: only backslash and newline are.
		`# HELP test_duration_seconds a "quoted" help`,
		`# TYPE test_duration_seconds histogram`,
		`test_duration_seconds_bucket{route="/x",le="0.5"} 1`,
		`test_duration_seconds_bucket{route="/x",le="1"} 2`,
		`test_duration_seconds_bucket{route="/x",le="+Inf"} 3`,
		`test_duration_seconds_sum{route="/x"} 5`,
		`test_duration_seconds_count{route="/x"} 3`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("exposition is missing %q\n---\n%s", want, text)
		}
	}
}

func TestExpositionEscapesLabelValues(t *testing.T) {
	registry := NewRegistry()
	counter := registry.NewCounterVec("test_escape_total", "escaping", "value")
	// Every character the exposition format treats specially, in one value.
	counter.With("back\\slash \"quote\" new\nline").Inc()

	text := render(t, registry)
	const want = `test_escape_total{value="back\\slash \"quote\" new\nline"} 1`
	if !strings.Contains(text, want) {
		t.Fatalf("escaped series missing\nwant: %s\ngot:\n%s", want, text)
	}
	// The raw newline must not survive: it would split one series across two
	// lines and make the whole scrape unparseable.
	if strings.Count(text, "\n") != len(strings.Split(strings.TrimSuffix(text, "\n"), "\n")) {
		t.Fatalf("exposition contains a raw newline inside a series:\n%s", text)
	}
}

func TestExpositionGroupsFamiliesAndSortsByName(t *testing.T) {
	registry := NewRegistry()
	registry.NewCounterVec("zzz_total", "last", "a").With("1").Inc()
	registry.NewCounterVec("aaa_total", "first", "a").With("1").Inc()
	registry.NewGaugeVec("mmm_gauge", "middle", "a").With("1").Set(2)

	text := render(t, registry)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")

	// 3 registered families with one series each, plus the always-present
	// classwatch_http_requests_total family (which has no series in this test).
	if len(lines) != 11 {
		t.Fatalf("expected 11 lines, got %d:\n%s", len(lines), text)
	}
	// Families must be contiguous and in name order.
	wantOrder := []string{
		"# HELP aaa_total", "# TYPE aaa_total", "aaa_total{",
		"# HELP classwatch_http_requests_total", "# TYPE classwatch_http_requests_total",
		"# HELP mmm_gauge", "# TYPE mmm_gauge", "mmm_gauge{",
		"# HELP zzz_total", "# TYPE zzz_total", "zzz_total{",
	}
	for i, want := range wantOrder {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d = %q, want prefix %q\n---\n%s", i, lines[i], want, text)
		}
	}
}

func TestExpositionEmptyFamilyStillHasHelpAndType(t *testing.T) {
	registry := NewRegistry()
	registry.NewCounterVec("test_empty_total", "no series yet", "a")

	text := render(t, registry)
	if !strings.Contains(text, "# HELP test_empty_total no series yet\n") {
		t.Fatalf("HELP missing:\n%s", text)
	}
	if !strings.Contains(text, "# TYPE test_empty_total counter\n") {
		t.Fatalf("TYPE missing:\n%s", text)
	}
	if strings.Contains(text, "test_empty_total{") {
		t.Fatalf("no series should be rendered:\n%s", text)
	}
}

func TestHelpEscapesBackslashAndNewline(t *testing.T) {
	registry := NewRegistry()
	registry.NewCounterVec("test_help_total", "line one\nline \\ two", "a")

	text := render(t, registry)
	if !strings.Contains(text, `# HELP test_help_total line one\nline \\ two`) {
		t.Fatalf("HELP escaping is wrong:\n%s", text)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{math.NaN(), "NaN"},
		{0.005, "0.005"},
		{0.1, "0.1"},
		{1, "1"},
		{2.5, "2.5"},
		{10, "10"},
	}
	for _, tc := range cases {
		if got := formatFloat(tc.value); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestInvalidRegistrationPanics(t *testing.T) {
	registry := NewRegistry()
	registry.NewCounterVec("test_ok_total", "ok", "a")

	cases := []struct {
		name string
		call func()
	}{
		{"duplicate name", func() { registry.NewCounterVec("test_ok_total", "dup", "a") }},
		{"invalid metric name", func() { registry.NewCounterVec("1bad", "bad", "a") }},
		{"invalid label name", func() { registry.NewCounterVec("test_label_total", "bad", "1bad") }},
		{"reserved le label", func() { registry.NewHistogramVec("test_le_seconds", "bad", []float64{1}, "le") }},
		{"missing help", func() { registry.NewCounterVec("test_nohelp_total", "  ", "a") }},
		{"no bucket bounds", func() { registry.NewHistogramVec("test_nobuckets_seconds", "bad", nil, "a") }},
		{"unsorted bucket bounds", func() { registry.NewHistogramVec("test_unsorted_seconds", "bad", []float64{2, 1}, "a") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", tc.name)
				}
			}()
			tc.call()
		})
	}

	t.Run("wrong label arity", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("a label arity mismatch did not panic")
			}
		}()
		registry.NewCounterVec("test_arity_total", "arity", "a", "b").With("only-one")
	})
}

func TestHTTPRouteInstrumentsAreCachedAndRendered(t *testing.T) {
	m := New()
	first := m.HTTPRoute(http.MethodGet, "/api/v1/classrooms/:id")
	second := m.HTTPRoute(http.MethodGet, "/api/v1/classrooms/:id")
	if first != second {
		t.Fatal("HTTPRoute must return the same instrument bundle for the same route")
	}

	first.Observe(200, 0.004)
	first.Observe(200, 0.02)
	first.Observe(404, 0.001)
	// A status outside the valid range must be ignored rather than panic.
	first.Observe(99, 0.001)
	first.Observe(600, 0.001)

	text := renderMetrics(t, m)
	for _, want := range []string{
		`# TYPE classwatch_http_requests_total counter`,
		`classwatch_http_requests_total{method="GET",route="/api/v1/classrooms/:id",status="200"} 2`,
		`classwatch_http_requests_total{method="GET",route="/api/v1/classrooms/:id",status="404"} 1`,
		`classwatch_http_request_duration_seconds_bucket{method="GET",route="/api/v1/classrooms/:id",le="0.005"} 4`,
		`classwatch_http_request_duration_seconds_bucket{method="GET",route="/api/v1/classrooms/:id",le="+Inf"} 5`,
		`classwatch_http_request_duration_seconds_count{method="GET",route="/api/v1/classrooms/:id"} 5`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("exposition is missing %q\n---\n%s", want, text)
		}
	}
	if strings.Contains(text, `status="99"`) || strings.Contains(text, `status="600"`) {
		t.Errorf("an out-of-range status was rendered:\n%s", text)
	}
}

func TestHandlerExpositionEndpoint(t *testing.T) {
	m := New()
	m.HTTPRequestDuration.With("GET", "/x").Observe(0.002)
	m.SetBuildInfo("1.2.3", "abcdef1")
	m.SetDBPoolConnections(2, 3, 5)
	m.SetSessionStatus(map[string]int64{"ONLINE": 4, "LEFT": 1})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Fatalf("content type = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`classwatch_build_info{version="1.2.3",commit="abcdef1"} 1`,
		`classwatch_db_pool_connections{state="acquired"} 2`,
		`classwatch_db_pool_connections{state="idle"} 3`,
		`classwatch_db_pool_connections{state="total"} 5`,
		`classwatch_session_status{status="ONLINE"} 4`,
		`classwatch_session_status{status="SCREEN_LOST"} 0`,
		`classwatch_session_status{status="LEFT"} 1`,
		`classwatch_ws_connections{role="student"} 0`,
		`classwatch_ws_connections{role="teacher"} 0`,
		`classwatch_private_talk_active 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics is missing %q\n---\n%s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics = %d, want 405", rec.Code)
	}
}

func TestNilMetricsIsUsable(t *testing.T) {
	// Every service takes metrics as an optional collaborator; a nil value must be
	// a no-op, not a panic. This test is the contract that lets the Phase 1-10
	// tests keep constructing services without a registry.
	var m *Metrics
	m.SetBuildInfo("v", "c")
	m.AddWSConnection(WSRoleStudent, 1)
	m.IncWSMessage(DirectionIn, "PING")
	m.IncWSSlowConsumerDisconnect()
	m.IncRateLimitRejected(RateLimitScopeLogin, DimensionIP)
	m.IncWebhookEvent(WebhookEventUnverified, WebhookResultInvalidSignature)
	m.ObserveMediaCall(MediaOperationEnsureRoom, MediaResultOK, 0.1)
	m.SetSessionStatus(map[string]int64{"ONLINE": 1})
	m.SetPrivateTalkActive(1)
	m.SetDBPoolConnections(1, 1, 1)
	if got := m.HTTPRoute(http.MethodGet, "/x"); got != nil {
		t.Fatalf("nil metrics returned %v, want nil", got)
	}
	var meter *RouteInstruments
	meter.Observe(200, 0.001)
	var gauge *Gauge
	gauge.Set(1)
	var counter *Counter
	counter.Inc()
	var histogram *Histogram
	histogram.Observe(1)
	if counter.Value() != 0 || gauge.Value() != 0 || histogram.Count() != 0 {
		t.Fatal("nil handles must not report values")
	}
}

func TestConcurrentExpositionWhileRecording(t *testing.T) {
	// /metrics is scraped while traffic is being served; the exposition must not
	// race with recording (run with -race).
	m := New()
	route := m.HTTPRoute(http.MethodGet, "/x")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				route.Observe(200, 0.001)
				m.HTTPRequestDuration.With("GET", "/y").Observe(0.01)
				m.WSConnections.With(WSRoleStudent).Set(int64(j))
				m.IncWSMessage(DirectionOut, "SCREEN_LOST")
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rec := httptest.NewRecorder()
			m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		}
	}()
	wg.Wait()
}

func render(t *testing.T, registry *Registry) string {
	t.Helper()
	m := &Metrics{registry: registry, routes: make(map[httpRouteKey]*RouteInstruments)}
	rec := httptest.NewRecorder()
	if err := m.WriteExposition(rec); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	return rec.Body.String()
}

func renderMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := m.WriteExposition(rec); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	return rec.Body.String()
}
