package httpapi

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// This file is the §63/§77 hardening test suite: the security headers, the request
// body cap, the shutdown drain gate, the metrics middleware and the trusted-proxy
// rule for HSTS.

// ---------------------------------------------------------------------------
// Security response headers
// ---------------------------------------------------------------------------

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{}, "https://teacher.example.com")

	cases := []struct {
		name   string
		method string
		path   string
		header map[string]string
		want   int
	}{
		{"success", http.MethodGet, "/api/v1/meta", nil, http.StatusOK},
		{"not found", http.MethodGet, "/does-not-exist", nil, http.StatusNotFound},
		{"method not allowed", http.MethodPost, "/healthz", nil, http.StatusMethodNotAllowed},
		{"refused origin", http.MethodPost, "/api/v1/meta", map[string]string{
			"Origin": "https://evil.example.com",
		}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, router, tc.method, tc.path, tc.header)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			h := rec.Header()
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := h.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q", got)
			}
			if got := h.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q", got)
			}
			if got := h.Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
				t.Errorf("Content-Security-Policy = %q", got)
			}
			// Plain HTTP (httptest sends no TLS): HSTS must be absent or a local
			// developer's browser pins localhost to HTTPS for a year.
			if got := h.Get("Strict-Transport-Security"); got != "" {
				t.Errorf("Strict-Transport-Security = %q on a plain HTTP response", got)
			}
		})
	}
}

func TestHSTSOnlyOnHTTPS(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}

	cases := []struct {
		name     string
		tls      bool
		peer     string
		proto    string
		trusted  []*net.IPNet
		wantHSTS bool
	}{
		{"direct TLS", true, "203.0.113.9:1234", "", nil, true},
		{"plain http, no header", false, "203.0.113.9:1234", "", nil, false},
		{
			name: "plain http, trusted proxy says https", tls: false,
			peer: "10.1.2.3:1234", proto: "https", trusted: trusted, wantHSTS: true,
		},
		{
			name: "plain http, UNTRUSTED peer claims https", tls: false,
			peer: "203.0.113.9:1234", proto: "https", trusted: trusted, wantHSTS: false,
		},
		{
			name: "plain http, trusted peer says the client used http", tls: false,
			peer: "10.1.2.3:1234", proto: "http", trusted: trusted, wantHSTS: false,
		},
		{
			name: "multi-hop X-Forwarded-Proto uses the last hop", tls: false,
			peer: "10.1.2.3:1234", proto: "https, http", trusted: trusted, wantHSTS: false,
		},
		{
			name: "a peer outside the trusted list is not believed", tls: false,
			peer: "192.0.2.7:1234", proto: "https", trusted: trusted, wantHSTS: false,
		},
		{
			name: "trusted proxy with no X-Forwarded-Proto at all", tls: false,
			peer: "10.1.2.3:1234", proto: "", trusted: trusted, wantHSTS: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewClientIPResolver(tc.trusted)
			router := gin.New()
			router.Use(SecurityHeadersMiddleware(resolver))
			router.GET("/x", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tc.peer
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				req.TLS = &tlsState
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			got := rec.Header().Get("Strict-Transport-Security")
			if tc.wantHSTS && got == "" {
				t.Errorf("Strict-Transport-Security is missing, want it present")
			}
			if !tc.wantHSTS && got != "" {
				t.Errorf("Strict-Transport-Security = %q, want it absent", got)
			}
			if tc.wantHSTS && !strings.Contains(got, "max-age=") {
				t.Errorf("Strict-Transport-Security = %q, want a max-age", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Request body cap
// ---------------------------------------------------------------------------

func TestBodyLimitRejectsDeclaredOversizeBody(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTPMaxBodyBytes = 64
	router := NewRouter(Deps{Logger: discardLogger(), Config: cfg})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/student/auth/login", strings.NewReader(strings.Repeat("x", 200)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != string(apperr.CodePayloadTooLarge) {
		t.Errorf("code = %q, want PAYLOAD_TOO_LARGE", code)
	}
}

func TestBodyLimitBoundsAStreamedBody(t *testing.T) {
	// No Content-Length (a chunked upload, or a client that lies): the cap must be
	// enforced by the reader, not by the header.
	router := gin.New()
	router.Use(BodyLimitMiddleware(32))
	router.POST("/x", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if isBodyTooLarge(err) {
			RespondError(c, payloadTooLarge())
			return
		}
		if err != nil {
			RespondError(c, apperr.New(apperr.CodeInvalidRequest))
			return
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("y", 4096)))
	req.ContentLength = -1 // chunked: no declared length
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
}

func TestBodyLimitLetsSmallBodiesThrough(t *testing.T) {
	router := gin.New()
	router.Use(BodyLimitMiddleware(1024))
	router.POST("/x", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("reading a small body failed: %v", err)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"account":"S1"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Shutdown drain gate
// ---------------------------------------------------------------------------

func TestDrainGateRefusesNewRequestsWith503(t *testing.T) {
	drain := NewDrainGate()
	cfg := testConfig(t)
	router := NewRouter(Deps{Logger: discardLogger(), Config: cfg, Ready: ReadinessDeps{}, Drain: drain})

	if rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", nil); rec.Code != http.StatusOK {
		t.Fatalf("before draining: status = %d, want 200", rec.Code)
	}

	drain.BeginDraining()
	if !drain.Draining() {
		t.Fatal("BeginDraining did not flip the gate")
	}
	// Idempotent: a second signal during the drain window must not undo it.
	drain.BeginDraining()

	rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("during drain: status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != string(apperr.CodeServiceUnavailable) {
		t.Errorf("code = %q, want SERVICE_UNAVAILABLE", code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("Retry-After is missing; a client would retry immediately")
	}

	// Liveness and the scrape must survive the drain: an orchestrator that sees a
	// failing liveness probe during a rollout sends SIGKILL instead of waiting.
	if rec := doRequest(t, router, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz during drain = %d, want 200", rec.Code)
	}
	// Readiness must NOT survive: 503 is how a load balancer is told to stop
	// routing here. The body must still be the readyz contract.
	if rec := doRequest(t, router, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz during drain = %d, want 503", rec.Code)
	}

	// A fresh router with no gate must never report draining.
	var noGate *DrainGate
	if noGate.Draining() {
		t.Fatal("a nil gate must report not draining")
	}
	if rec := doRequest(t, NewRouter(Deps{Logger: discardLogger(), Config: testConfig(t)}), http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("a router without a gate: status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Metrics middleware
// ---------------------------------------------------------------------------

func TestMetricsMiddlewareRecordsRouteTemplatesAndSkipsItself(t *testing.T) {
	m := metrics.New()
	cfg := testConfig(t)
	router := NewRouter(Deps{Logger: discardLogger(), Config: cfg, Metrics: m, Ready: ReadinessDeps{}})

	doRequest(t, router, http.MethodGet, "/healthz", nil)
	doRequest(t, router, http.MethodGet, "/api/v1/nope", nil)
	doRequest(t, router, http.MethodPost, "/healthz", nil)
	// /metrics is scraped repeatedly in production; recording the scrape would make
	// the counter grow with the scrape rate.
	doRequest(t, router, http.MethodGet, "/metrics", nil)

	text := expositionOf(t, m)
	for _, want := range []string{
		`classwatch_http_requests_total{method="GET",route="/healthz",status="200"} 1`,
		`classwatch_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`classwatch_http_requests_total{method="POST",route="unmatched",status="405"} 1`,
		`classwatch_http_request_duration_seconds_count{method="GET",route="/healthz"} 1`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("exposition is missing %q\n---\n%s", want, text)
		}
	}
	if strings.Contains(text, `route="/metrics"`) {
		t.Errorf("/metrics was counted:\n%s", text)
	}
	if !strings.Contains(text, "classwatch_http_requests_in_flight 0") {
		t.Errorf("the in-flight gauge is not back to 0:\n%s", text)
	}
}

func TestMetricsMiddlewareBoundsTheMethodLabel(t *testing.T) {
	m := metrics.New()
	router := NewRouter(Deps{Logger: discardLogger(), Config: testConfig(t), Metrics: m})

	// A scanner can invent method tokens; each one must NOT become a time series.
	for _, method := range []string{"PROPFIND", "FROBNICATE", "X1", "X2"} {
		doRequest(t, router, method, "/whatever", nil)
	}

	text := expositionOf(t, m)
	if strings.Contains(text, `method="PROPFIND"`) || strings.Contains(text, `method="X1"`) {
		t.Errorf("an unknown method produced a label value:\n%s", text)
	}
	if !strings.Contains(text, `method="other"`) {
		t.Errorf("unknown methods were not collapsed into \"other\":\n%s", text)
	}
}

func TestMetricsMiddlewareObservesPanics(t *testing.T) {
	m := metrics.New()
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := NewRouter(Deps{Logger: logger, Config: testConfig(t), Metrics: m})
	router.GET("/boom", func(*gin.Context) { panic("boom-detail") })

	rec := doRequest(t, router, http.MethodGet, "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Metrics sits OUTSIDE Recovery, so the panic is observed as the 500 the client
	// actually received (see MetricsMiddleware).
	if text := expositionOf(t, m); !strings.Contains(text, `classwatch_http_requests_total{method="GET",route="/boom",status="500"} 1`) {
		t.Errorf("the panic was not observed as a 500:\n%s", text)
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("the panic was not logged:\n%s", logs.String())
	}
	// §59: the recovery log carries the stack (server-side), the response does not
	// carry the panic value.
	if !strings.Contains(logs.String(), "stack=") {
		t.Errorf("the recovery log has no stack:\n%s", logs.String())
	}
	if strings.Contains(rec.Body.String(), "boom-detail") {
		t.Errorf("the panic value leaked into the response: %s", rec.Body.String())
	}
}

func TestMetricsEndpointIsAvailableWithoutASession(t *testing.T) {
	m := metrics.New()
	router := NewRouter(Deps{Logger: discardLogger(), Config: testConfig(t), Metrics: m})

	rec := doRequest(t, router, http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "# HELP classwatch_http_requests_total") {
		t.Errorf("the scrape is missing HELP lines:\n%s", rec.Body.String())
	}
	// No metrics configured: the endpoint must not exist rather than answer an empty
	// body that looks like a working scrape.
	rec = doRequest(t, NewRouter(Deps{Logger: discardLogger(), Config: testConfig(t)}), http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("without metrics: status = %d, want 404", rec.Code)
	}
}

func TestRateLimitRejectionIsCounted(t *testing.T) {
	m := metrics.New()
	cfg := testConfig(t)
	// One API call per window: the second is refused.
	cfg.RateLimitAPIPerMinute = 1
	cfg.RateLimitAPIWindow = time.Minute
	router := NewRouter(Deps{
		Logger:  discardLogger(),
		Config:  cfg,
		Metrics: m,
		Limiter: newAlwaysLimitedAfterFirst(),
	})

	if rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}

	text := expositionOf(t, m)
	want := `classwatch_ratelimit_rejected_total{scope="api",dimension="ip"} 1`
	if !strings.Contains(text, want+"\n") {
		t.Errorf("exposition is missing %q\n---\n%s", want, text)
	}
}

func TestSocketCapBoundsConcurrentConnectionsPerAddress(t *testing.T) {
	cap := newSocketCap(2)

	release1, ok := cap.acquire("10.0.0.1")
	if !ok {
		t.Fatal("the first connection was refused")
	}
	release2, ok := cap.acquire("10.0.0.1")
	if !ok {
		t.Fatal("the second connection was refused")
	}
	if _, ok := cap.acquire("10.0.0.1"); ok {
		t.Fatal("the cap was exceeded")
	}
	// A different address is unaffected: the cap is per client, not global.
	releaseOther, ok := cap.acquire("10.0.0.2")
	if !ok {
		t.Fatal("a different address was refused")
	}

	release1()
	release1() // idempotent: a double release must not free somebody else's slot
	if _, ok := cap.acquire("10.0.0.1"); !ok {
		t.Fatal("a released slot was not reusable")
	}
	release2()
	releaseOther()

	if got := cap.liveCount("10.0.0.1"); got != 1 {
		t.Fatalf("live count = %d, want 1", got)
	}
	// A disabled cap (limit 0) must not refuse anything: it is what the configuration
	// layer prevents, but the type must stay total.
	var disabled *socketCap
	if _, ok := disabled.acquire("10.0.0.3"); !ok {
		t.Fatal("a nil cap refused a connection")
	}
}

// TestRouterWithoutAConfigStillWorks pins the supported "probe-only" deployment:
// NewRouter accepts a nil Config (it then has no entry points, no origin allowlist
// and no limiters), and the Phase 11 middlewares must not be the thing that turns
// that into a panic.
func TestRouterWithoutAConfigStillWorks(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	for _, probe := range []string{"/healthz", "/readyz"} {
		if rec := doRequest(t, router, http.MethodGet, probe, nil); rec.Code == http.StatusInternalServerError {
			t.Errorf("%s = 500 with a nil Config; the router must stay usable", probe)
		}
	}
	// No metrics registry either: the endpoint simply does not exist.
	if rec := doRequest(t, router, http.MethodGet, "/metrics", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/metrics = %d, want 404", rec.Code)
	}
	// The hardening headers must still be present: they do not depend on config.
	rec := doRequest(t, router, http.MethodGet, "/healthz", nil)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers are missing without a Config")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var tlsState = tls.ConnectionState{}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %s", rec.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

func expositionOf(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// alwaysLimitedAfterFirst is a limiter whose first call is allowed and every later
// call is refused. It exists so the counting test does not depend on a real window
// elapsing.
type alwaysLimitedAfterFirst struct {
	calls int
}

func newAlwaysLimitedAfterFirst() *alwaysLimitedAfterFirst { return &alwaysLimitedAfterFirst{} }

func (l *alwaysLimitedAfterFirst) Allow(_ context.Context, _ string, _ int, _ time.Duration) (bool, time.Duration, error) {
	l.calls++
	if l.calls == 1 {
		return true, 0, nil
	}
	return false, time.Second, nil
}
