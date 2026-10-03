package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/config"
)

// The tests in this package must run without PostgreSQL, Redis or LiveKit: the
// probe dependencies are interfaces precisely so a fake can stand in. Anything
// that needs a real database belongs in an integration test behind an env var.

type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

type fakeRoomLister struct {
	err   error
	delay time.Duration
}

func (f fakeRoomLister) HealthCheck(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func testConfig(t *testing.T, origins ...string) *config.Config {
	t.Helper()
	return &config.Config{
		AppEnv:                     config.EnvTest,
		APIAddr:                    ":8080",
		LogLevel:                   slog.LevelError + 1, // silence access logs during tests
		CORSAllowedOrigins:         origins,
		StartupRequireDependencies: false,
		SessionCookieName:          "classwatch_session",
		SessionTTL:                 time.Hour,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRouter(t *testing.T, deps ReadinessDeps, origins ...string) *gin.Engine {
	t.Helper()
	return NewRouter(Deps{Logger: discardLogger(), Config: testConfig(t, origins...), Ready: deps})
}

func doRequest(t *testing.T, router *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

func TestHealthzDoesNotTouchDependencies(t *testing.T) {
	// Every dependency is broken on purpose: liveness must still pass, otherwise
	// a database restart would make the orchestrator kill healthy API pods and
	// turn a blip into an outage.
	router := newTestRouter(t, ReadinessDeps{
		Postgres: fakePinger{err: errors.New("boom")},
		Redis:    fakePinger{err: errors.New("boom")},
		LiveKit:  fakeRoomLister{err: errors.New("boom")},
	})

	rec := doRequest(t, router, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "ok" {
		t.Errorf("body = %v, want status=ok", body)
	}
}

func TestReadyzReady(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{
		Postgres: fakePinger{},
		Redis:    fakePinger{},
		LiveKit:  fakeRoomLister{},
	})

	rec := doRequest(t, router, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)
	if body["status"] != "ready" {
		t.Errorf("status = %v, want ready", body["status"])
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks missing or not an object: %v", body["checks"])
	}
	for _, name := range []string{"postgres", "redis", "livekit"} {
		if checks[name] != "ok" {
			t.Errorf("checks[%q] = %v, want ok", name, checks[name])
		}
	}
}

func TestReadyzDegraded(t *testing.T) {
	// A connection-refused error is the realistic failure; it must collapse to a
	// category, not become part of the response.
	refused := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	router := newTestRouter(t, ReadinessDeps{
		Postgres: fakePinger{err: refused},
		Redis:    fakePinger{},
		LiveKit:  fakeRoomLister{err: context.DeadlineExceeded},
	})

	rec := doRequest(t, router, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	body := decodeBody(t, rec)
	if body["status"] != "degraded" {
		t.Errorf("status = %v, want degraded", body["status"])
	}
	checks := body["checks"].(map[string]any)
	if got := checks["postgres"]; got != "error: unreachable" {
		t.Errorf("checks[postgres] = %v, want %q", got, "error: unreachable")
	}
	if got := checks["redis"]; got != "ok" {
		t.Errorf("checks[redis] = %v, want ok", got)
	}
	if got := checks["livekit"]; got != "error: timeout" {
		t.Errorf("checks[livekit] = %v, want %q", got, "error: timeout")
	}
}

func TestReadyzNeverLeaksSecretLikeDetails(t *testing.T) {
	secret := "postgres://classwatch:sup3r-s3cret@db:5432/classwatch"
	router := newTestRouter(t, ReadinessDeps{
		Postgres: fakePinger{err: errors.New("failed to connect: " + secret)},
		Redis:    fakePinger{},
		LiveKit:  fakeRoomLister{},
	})

	rec := doRequest(t, router, http.MethodGet, "/readyz", nil)
	body := rec.Body.String()
	for _, forbidden := range []string{"sup3r-s3cret", "postgres://", "classwatch:sup3r"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("readiness body leaked %q: %s", forbidden, body)
		}
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestReadyzNilDependenciesReportNotConfigured(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when dependencies are not connected", rec.Code)
	}
	checks := decodeBody(t, rec)["checks"].(map[string]any)
	for _, name := range []string{"postgres", "redis", "livekit"} {
		if checks[name] != "error: not configured" {
			t.Errorf("checks[%q] = %v, want %q", name, checks[name], "error: not configured")
		}
	}
}

func TestReadyzRunsChecksConcurrently(t *testing.T) {
	// Three slow-but-successful dependencies: if the checks were sequential this
	// would take ~600ms, concurrently ~200ms. The margin keeps it stable on a
	// loaded CI machine while still failing if someone serialises the loop.
	delay := 200 * time.Millisecond
	router := newTestRouter(t, ReadinessDeps{
		Postgres: fakePinger{delay: delay},
		Redis:    fakePinger{delay: delay},
		LiveKit:  fakeRoomLister{delay: delay},
	})

	start := time.Now()
	rec := doRequest(t, router, http.MethodGet, "/readyz", nil)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > 2*delay {
		t.Errorf("readiness took %v for 3 checks of %v each; checks are not concurrent", elapsed, delay)
	}
}

func TestMetaResponseFields(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := decodeBody(t, rec)
	for key, want := range map[string]string{
		"service": "classwatch-api",
		"version": Version,
		"commit":  Commit,
		"env":     config.EnvTest,
		"phase":   "phase-0",
	} {
		got, ok := body[key].(string)
		if !ok || got == "" {
			t.Errorf("meta[%q] is missing or empty: %v", key, body[key])
			continue
		}
		if got != want {
			t.Errorf("meta[%q] = %q, want %q", key, got, want)
		}
	}

	ts, ok := body["time"].(string)
	if !ok {
		t.Fatalf("meta[time] missing: %v", body["time"])
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("meta[time] = %q is not RFC3339: %v", ts, err)
	}
	if time.Since(parsed) > time.Minute {
		t.Errorf("meta[time] = %v, want a current timestamp", parsed)
	}

	// The descriptor must not become a configuration dump: version and env are
	// public, database URLs and LiveKit secrets never are.
	for _, forbidden := range []string{"secret", "password", "database_url", "token"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), forbidden) {
			t.Errorf("meta response mentions %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestUnknownRouteReturnsUnifiedError(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodGet, "/api/v1/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	body := decodeBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %s", rec.Body.String())
	}
	// Phase 11 splits the transport-level codes: an unmatched path is NOT_FOUND
	// (404), a known path with an unknown method is METHOD_NOT_ALLOWED (405).
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("error.code = %v, want NOT_FOUND", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Error("error.message is empty; clients would have nothing to show")
	}
	if body["requestId"] == "" || body["requestId"] == nil {
		t.Error("requestId missing from the error envelope; support cannot correlate the report")
	}
}

func TestMethodNotAllowedReturnsUnifiedError(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodPost, "/healthz", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow header = %q, want it to list GET", allow)
	}
	errObj, ok := decodeBody(t, rec)["error"].(map[string]any)
	if !ok {
		t.Fatalf("405 body is not the unified envelope: %s", rec.Body.String())
	}
	if errObj["code"] != "METHOD_NOT_ALLOWED" {
		t.Errorf("405 error.code = %v, want METHOD_NOT_ALLOWED", errObj["code"])
	}
}

func TestRequestIDIsGeneratedAndPropagated(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodGet, "/healthz", nil)
	generated := rec.Header().Get("X-Request-Id")
	if generated == "" {
		t.Fatal("X-Request-Id missing from the response")
	}

	// A caller-supplied id must survive round-trip: that is what makes a support
	// ticket and a server log line the same trace.
	const given = "trace-from-the-frontend-123"
	rec = doRequest(t, router, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": given})
	if got := rec.Header().Get("X-Request-Id"); got != given {
		t.Errorf("X-Request-Id = %q, want the caller value %q", got, given)
	}

	// The same id must appear in the error envelope, since that is what the user
	// will quote.
	rec = doRequest(t, router, http.MethodGet, "/nope", map[string]string{"X-Request-Id": given})
	if got := decodeBody(t, rec)["requestId"]; got != given {
		t.Errorf("error envelope requestId = %v, want %q", got, given)
	}
}

func TestRequestIDRejectsHeaderInjection(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{})

	// A CRLF in the echoed header would let a caller forge log lines or split the
	// response. The value must be replaced by a generated id, not echoed.
	for _, hostile := range []string{"evil\r\nX-Injected: 1", "with space", strings.Repeat("a", 200)} {
		rec := doRequest(t, router, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": hostile})
		got := rec.Header().Get("X-Request-Id")
		if got == hostile {
			t.Errorf("hostile X-Request-Id %q was echoed back verbatim", hostile)
		}
		if got == "" {
			t.Errorf("hostile X-Request-Id %q produced no replacement id", hostile)
		}
		if rec.Header().Get("X-Injected") != "" {
			t.Error("header injection succeeded")
		}
	}
}

func TestCORSAllowedOrigin(t *testing.T) {
	const origin = "http://localhost:5173"
	router := newTestRouter(t, ReadinessDeps{}, origin)

	rec := doRequest(t, router, http.MethodGet, "/api/v1/meta", map[string]string{"Origin": origin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	// Without Vary a shared cache could serve this response to another origin.
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin", rec.Header().Get("Vary"))
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("wildcard origin is not allowed with credentials")
	}
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	const origin = "https://teacher.example.com"
	router := newTestRouter(t, ReadinessDeps{}, origin)

	rec := doRequest(t, router, http.MethodOptions, "/api/v1/meta", map[string]string{
		"Origin":                         origin,
		"Access-Control-Request-Method":  http.MethodGet,
		"Access-Control-Request-Headers": "Content-Type, X-Request-Id",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, http.MethodGet) {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to include GET", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Request-Id") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to include X-Request-Id", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	router := newTestRouter(t, ReadinessDeps{}, "https://teacher.example.com")

	const hostile = "https://evil.example.com"

	// Preflight from a non-allowlisted origin is refused outright: answering it
	// would tell the browser the real request may proceed.
	rec := doRequest(t, router, http.MethodOptions, "/api/v1/meta", map[string]string{
		"Origin":                        hostile,
		"Access-Control-Request-Method": http.MethodGet,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want it absent for a rejected origin", got)
	}
	if _, ok := decodeBody(t, rec)["error"].(map[string]any); !ok {
		t.Errorf("403 body is not the unified envelope: %s", rec.Body.String())
	}

	// A simple GET still reaches the handler (the browser blocks the response),
	// but no CORS grant is emitted.
	rec = doRequest(t, router, http.MethodGet, "/api/v1/meta", map[string]string{"Origin": hostile})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want it absent", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Error("Vary: Origin must be set on rejected responses too, or caches can cross-contaminate")
	}
}

func TestCORSDisallowedOriginCannotMutateState(t *testing.T) {
	// CORS alone would let a hostile page trigger a state-changing request while
	// only hiding the response. Any non-safe method from an unknown origin is
	// therefore rejected before a handler runs.
	router := newTestRouter(t, ReadinessDeps{}, "https://teacher.example.com")

	rec := doRequest(t, router, http.MethodPost, "/healthz", map[string]string{
		"Origin": "https://evil.example.com",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCORSNoOriginHeaderIsUnaffected(t *testing.T) {
	// Health probes and server-to-server calls send no Origin and must keep
	// working with an empty allowlist.
	router := newTestRouter(t, ReadinessDeps{})

	rec := doRequest(t, router, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want it absent for a non-CORS request", got)
	}
}

func TestPanicBecomesInternalErrorEnvelope(t *testing.T) {
	logger := discardLogger()
	router := NewRouter(Deps{Logger: logger, Config: testConfig(t), Ready: ReadinessDeps{}})
	router.GET("/boom", func(*gin.Context) { panic("secret-internal-detail") })

	rec := doRequest(t, router, http.MethodGet, "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	body := decodeBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("500 body is not the unified envelope: %s", rec.Body.String())
	}
	if errObj["code"] != "INTERNAL" {
		t.Errorf("error.code = %v, want INTERNAL", errObj["code"])
	}
	// The panic value is internal detail: it belongs in the server log, not in a
	// response a student can read.
	if strings.Contains(rec.Body.String(), "secret-internal-detail") {
		t.Errorf("panic value leaked into the response: %s", rec.Body.String())
	}
}
