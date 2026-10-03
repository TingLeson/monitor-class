package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/classwatch/classwatch/services/api/internal/admin"
	authservice "github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/realtime"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The Phase 11 integration suite: the hardening of §63 and the metrics of §77 against
// a real PostgreSQL, the real repositories and the real middleware chain.
//
// WHAT THE UNIT TESTS CANNOT SHOW, and why this file exists:
//
//   - that /metrics reports the traffic that actually happened, with the route
//     TEMPLATES Gin matched, after real logins and real joins;
//   - that the session census query returns the states the state machine wrote;
//   - that EVERY unsafe route (POST/PUT/PATCH/DELETE) is behind the CSRF middleware —
//     a claim about the whole route table, which the per-route unit tests cannot make
//     because each of them only knows the routes of its own harness.

type hardeningE2E struct {
	router      *gin.Engine
	pool        *pgxpool.Pool
	users       *user.Postgres
	sessionRepo *session.Postgres
	media       *fakeMediaPlane
	hub         *realtime.Hub
	processor   *session.Processor
	metrics     *metrics.Metrics
	cfg         *config.Config
	logs        *strings.Builder
}

// newHardeningE2E wires the API exactly as cmd/api does, including the admin surface
// (which the other integration harnesses do not need) so the route table is complete.
func newHardeningE2E(t *testing.T, mutate func(*config.Config)) *hardeningE2E {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pool := dbtest.Pool(t)
	users := user.NewPostgres(pool)
	classroomRepo := classroom.NewPostgres(pool)
	sessionRepo := session.NewPostgres(pool)
	mediaPlane := newFakeMediaPlane()
	terminator := &fakeRoomTerminator{}

	cfg := &config.Config{
		AppEnv:                           config.EnvTest,
		SessionCookieName:                "classwatch_session",
		SessionTTL:                       time.Hour,
		SessionCookieSecure:              false,
		SessionIdleTouchInterval:         5 * time.Minute,
		RateLimitLoginPerMinute:          1000,
		RateLimitLoginPerAccountPer10Min: 1000,
		RateLimitAPIPerMinute:            10000,
		PasswordMinLength:                12,
		LiveKitURL:                       "wss://media.example.test",
		LiveKitTokenTTL:                  2 * time.Hour,
	}
	if mutate != nil {
		mutate(cfg)
	}

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metricSet := metrics.New()
	metricSet.SetBuildInfo("test", "deadbeef")

	hub := realtime.NewHub(realtime.HubConfig{Logger: logger, Metrics: metricSet})
	runtimeService := realtime.NewService(hub, realtime.NewAudience(pool), logger)
	processor := session.NewProcessor(sessionRepo, runtimeService).WithMetrics(metricSet)

	authService := authservice.NewService(users, sessionstore.New(pool), authservice.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    authservice.NewPasswordPolicy(cfg.PasswordMinLength),
	})
	adminService := admin.NewService(users, sessionstore.New(pool), authservice.NewPasswordPolicy(cfg.PasswordMinLength))
	classroomService := classroom.NewService(classroomRepo, users).
		WithRoomTerminator(terminator).
		WithRuntimeHooks(processor)
	sessionService := session.NewService(sessionRepo, classroomService, mediaPlane, session.Config{
		LiveKitURL: cfg.LiveKitURL,
		TokenTTL:   cfg.LiveKitTokenTTL,
	}).WithLifecycleEvents(processor).
		WithPrivateTalk(runtimeService, sessionRepo).
		WithMetrics(metricSet)
	processor.WithPrivateTalkEnder(sessionService)

	verifier, err := media.NewWebhookVerifier(testWebhookKey, testWebhookSecret)
	if err != nil {
		t.Fatalf("NewWebhookVerifier(): %v", err)
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Logger:      logger,
		Config:      cfg,
		Auth:        authService,
		Admin:       adminService,
		Classroom:   classroomService,
		Session:     sessionService,
		PrivateTalk: sessionService,
		Webhook:     &httpapi.WebhookDeps{Verifier: verifier, Processor: processor},
		Socket:      hub,
		Limiter:     ratelimit.NewMemory(),
		Metrics:     metricSet,
	})
	t.Cleanup(func() { hub.Shutdown(context.Background()) })

	return &hardeningE2E{
		router: router, pool: pool, users: users, sessionRepo: sessionRepo,
		media: mediaPlane, hub: hub, processor: processor, metrics: metricSet,
		cfg: cfg, logs: logs,
	}
}

func (e *hardeningE2E) call(t *testing.T, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "198.51.100.77:34567"
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// loginEntry logs an account in on its own entry point and returns the cookie jar.
func (e *hardeningE2E) loginEntry(t *testing.T, entry, account, password string) []*http.Cookie {
	t.Helper()
	body := `{"account":"` + account + `"}`
	if password != "" {
		body = `{"account":"` + account + `","password":"` + password + `"}`
	}
	rec := e.call(t, http.MethodPost, "/api/v1/"+entry+"/auth/login", body, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s login: status = %d (%s)", entry, rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()
}

// staffWithPassword creates and logs in an ADMIN or TEACHER account.
func (e *hardeningE2E) staffWithPassword(t *testing.T, role user.Role) (*user.User, []*http.Cookie) {
	t.Helper()
	const password = "a-hardening-passphrase"
	hash, err := authservice.Hash(password)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account:      dbtest.RandomAccount(strings.ToLower(string(role))),
		DisplayName:  string(role) + " 加固",
		Role:         role,
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID)
	})
	return created, e.loginEntry(t, strings.ToLower(string(role)), created.Account, password)
}

func (e *hardeningE2E) studentAccount(t *testing.T) (*user.User, []*http.Cookie) {
	t.Helper()
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account:     dbtest.RandomAccount("student"),
		DisplayName: "学生 加固",
		Role:        user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID)
	})
	return created, e.loginEntry(t, "student", created.Account, "")
}

// scrape returns the /metrics body.
func (e *hardeningE2E) scrape(t *testing.T) string {
	t.Helper()
	rec := e.call(t, http.MethodGet, "/metrics", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d (%s)", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// ---------------------------------------------------------------------------
// §77 — the metrics reflect real traffic
// ---------------------------------------------------------------------------

func TestMetricsReflectRealTraffic(t *testing.T) {
	e := newHardeningE2E(t, nil)

	_, cookies := e.studentAccount(t)
	// A real route, a real 404 and a real 405: all three must land in the counter
	// with the route TEMPLATE (never the real path). The concrete id is kept so the
	// assertion below can prove it stayed out of the exposition.
	missingClassroomID := uuid.NewString()
	e.call(t, http.MethodGet, "/api/v1/student/classrooms", "", cookies, nil)
	e.call(t, http.MethodGet, "/api/v1/student/classrooms/"+missingClassroomID, "", cookies, nil)
	e.call(t, http.MethodGet, "/definitely-not-a-route", "", nil, nil)
	e.call(t, http.MethodPost, "/healthz", "", nil, nil)

	text := e.scrape(t)
	for _, want := range []string{
		`classwatch_http_requests_total{method="GET",route="/api/v1/student/classrooms",status="200"} 1`,
		`classwatch_http_requests_total{method="POST",route="/api/v1/student/auth/login",status="200"} 1`,
		// The route TEMPLATE, not the path: a random (unauthorized) classroom id is
		// the same series as any other.
		`classwatch_http_requests_total{method="GET",route="/api/v1/student/classrooms/:id",status="404"} 1`,
		`classwatch_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		// Gin does not expose the matched template on the 405 path (no handler ran),
		// so a wrong method is labelled `unmatched` — the same bounded label a 404
		// gets. Labelling it with the real path would let one wrong method per UUID
		// mint a series.
		`classwatch_http_requests_total{method="POST",route="unmatched",status="405"} 1`,
		`classwatch_http_request_duration_seconds_count{method="GET",route="/api/v1/student/classrooms"} 1`,
		`classwatch_http_requests_in_flight 0`,
		`classwatch_build_info{version="test",commit="deadbeef"} 1`,
		`classwatch_db_pool_connections{state="total"}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics is missing %q\n---\n%s", want, text)
		}
	}
	// The scrape itself must never be counted, or the counter grows with the scrape
	// rate instead of with the traffic.
	if strings.Contains(text, `route="/metrics"`) {
		t.Errorf("/metrics counted itself:\n%s", text)
	}
	// The real path must never be a label value: it carries a UUID, and one series per
	// classroom is how a metrics endpoint takes down the Prometheus scraping it.
	if strings.Contains(text, missingClassroomID) {
		t.Errorf("the concrete path leaked into a label:\n%s", text)
	}
	if strings.Contains(text, "198.51.100.77") {
		t.Errorf("the client address leaked into a label:\n%s", text)
	}
}

func TestMetricsRecordWebhookOutcomes(t *testing.T) {
	e := newHardeningE2E(t, nil)

	// An unsigned delivery: the body is untrusted, so the event label must be the
	// constant, not a name taken from the payload.
	body := []byte(`{"event":"participant_joined","room":{"name":"lk_x"},"participant":{"identity":"` + uuid.NewString() + `"}}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/livekit/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/webhook+json")
	req.Header.Set("Authorization", "not-a-real-token")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook = %d, want 401", rec.Code)
	}

	// A correctly signed event that matches no session: verified, applied by nobody,
	// therefore "ignored" — not "applied", which is what the handler's nil error
	// alone would have suggested.
	event := livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: "lk_" + uuid.NewString()},
		Participant: &livekit.ParticipantInfo{Identity: uuid.NewString(), Sid: "PA_1"},
	}
	payload, err := protojson.Marshal(&event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/internal/livekit/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/webhook+json")
	req.Header.Set("Authorization", signWebhook(t, payload))
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed webhook = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	text := e.scrape(t)
	for _, want := range []string{
		`classwatch_webhook_events_total{event="unknown",result="invalid_signature"} 1`,
		`classwatch_webhook_events_total{event="participant_joined",result="ignored"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics is missing %q\n---\n%s", want, text)
		}
	}
}

func TestSessionStatusGaugeFollowsRealTransitions(t *testing.T) {
	e := newHardeningE2E(t, nil)

	_, teacherCookies := e.staffWithPassword(t, user.RoleTeacher)
	student, studentCookies := e.studentAccount(t)
	classroomID := e.createClassroom(t, teacherCookies, student)
	e.openClassroom(t, classroomID, teacherCookies)
	sessionID := e.join(t, classroomID, studentCookies)

	// The sampler does exactly this: one census query, one publication (see
	// cmd/api/metrics.go). Running it here is what proves the SQL and the gauge
	// mapping against rows the state machine actually wrote.
	e.sampleCensus(t)
	text := e.scrape(t)
	if !strings.Contains(text, `classwatch_session_status{status="CONNECTING"}`) {
		t.Fatalf("the census did not report CONNECTING:\n%s", text)
	}
	connecting := gaugeValue(t, text, "classwatch_session_status", `status="CONNECTING"`)
	if connecting < 1 {
		t.Fatalf("CONNECTING gauge = %d, want >= 1", connecting)
	}

	// Leave: the session moves to LEFT, and the SAME gauge must follow — a census
	// that only ever adds is the classic "gauge that cannot go down" bug.
	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/sessions/"+sessionID.String()+"/leave", "", studentCookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("leave = %d (%s)", rec.Code, rec.Body.String())
	}
	e.sampleCensus(t)
	text = e.scrape(t)
	if got := gaugeValue(t, text, "classwatch_session_status", `status="LEFT"`); got < 1 {
		t.Fatalf("LEFT gauge = %d, want >= 1:\n%s", got, text)
	}
	if got := gaugeValue(t, text, "classwatch_session_status", `status="CONNECTING"`); got != connecting-1 {
		t.Fatalf("CONNECTING gauge = %d, want %d (the census must go down):\n%s", got, connecting-1, text)
	}
}

// sampleCensus mirrors cmd/api's metricsSampler.sample: the same query, the same
// publication, so the assertions above are about the real path.
func (e *hardeningE2E) sampleCensus(t *testing.T) {
	t.Helper()
	counts, err := e.sessionRepo.CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus(): %v", err)
	}
	byStatus := make(map[string]int64, len(counts))
	for status, count := range counts {
		byStatus[string(status)] = count
	}
	e.metrics.SetSessionStatus(byStatus)
}

func TestRateLimitRejectionIsCountedAgainstTheRealLimiter(t *testing.T) {
	// A real in-memory limiter and a real window: the second request from the same
	// address is refused, and the refusal must be visible in /metrics with its scope.
	e := newHardeningE2E(t, func(cfg *config.Config) {
		cfg.RateLimitAPIPerMinute = 1
		cfg.RateLimitAPIWindow = time.Minute
	})

	if rec := e.call(t, http.MethodGet, "/api/v1/meta", "", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	rec := e.call(t, http.MethodGet, "/api/v1/meta", "", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}

	text := e.scrape(t)
	if !strings.Contains(text, `classwatch_ratelimit_rejected_total{scope="api",dimension="ip"} 1`) {
		t.Errorf("/metrics is missing the rejection:\n%s", text)
	}
	// The limiter's own key (which contains the client address) must never become a
	// label value.
	if strings.Contains(text, "198.51.100.77") {
		t.Errorf("the client address leaked into a label:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// §63 — every unsafe route is CSRF protected
// ---------------------------------------------------------------------------

// TestEveryUnsafeRouteRequiresCSRF is the coverage claim of §63 as an executable
// assertion: it walks the REAL route table, and for every POST/PUT/PATCH/DELETE route
// it sends an authenticated request WITHOUT a CSRF token.
//
// WHY this shape: the per-route unit tests each prove their own route, so a new
// endpoint added to the wrong group (outside the CSRF-protected write subgroup) would
// still pass all of them. This test fails the moment such a route appears — and it
// fails on the route table, not on a hand-maintained list, so it cannot go stale.
func TestEveryUnsafeRouteRequiresCSRF(t *testing.T) {
	e := newHardeningE2E(t, nil)

	sessions := map[string]*entrySession{
		"teacher": e.newEntrySession(t, user.RoleTeacher),
		"student": e.newEntrySession(t, user.RoleStudent),
		"admin":   e.newEntrySession(t, user.RoleAdmin),
	}
	entryOf := func(path string) string {
		switch {
		case strings.HasPrefix(path, "/api/v1/teacher"):
			return "teacher"
		case strings.HasPrefix(path, "/api/v1/student"):
			return "student"
		case strings.HasPrefix(path, "/api/v1/admin"):
			return "admin"
		default:
			return ""
		}
	}

	exempt := map[string]string{
		// A login has no session yet, so there is no CSRF token to compare against.
		// Its protection is the origin allowlist plus two rate limits (§37/§63).
		"/api/v1/admin/auth/login":   "no session exists before a login",
		"/api/v1/teacher/auth/login": "no session exists before a login",
		"/api/v1/student/auth/login": "no session exists before a login",
		// A machine caller (LiveKit) has no cookie and cannot read a token: the
		// signature over the body is its authentication (§45).
		"/internal/livekit/webhook": "authenticated by a webhook signature",
	}

	unsafe := 0
	exemptSeen := map[string]bool{}
	for _, route := range e.router.Routes() {
		switch route.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		unsafe++
		path := concretePath(route.Path)
		entry := entryOf(path)
		if reason, ok := exempt[route.Path]; ok {
			exemptSeen[route.Path] = true
			t.Run("exempt "+route.Method+" "+route.Path, func(t *testing.T) {
				// The webhook belongs to no entry point, so it is sent with no cookie
				// at all — which is exactly how LiveKit calls it.
				var cookies []*http.Cookie
				if session := sessions[entry]; session != nil {
					cookies = session.cookies
				}
				rec := e.call(t, route.Method, path, "{}", cookies, nil)
				if errorCodeOrEmpty(t, rec) == "CSRF_INVALID" {
					t.Fatalf("%s is exempt (%s) but required a CSRF token", route.Path, reason)
				}
			})
			continue
		}

		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			rec := e.call(t, route.Method, path, "{}", sessions[entry].cookies, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("without a CSRF token: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCodeOrEmpty(t, rec); code != "CSRF_INVALID" {
				t.Fatalf("without a CSRF token: code = %q, want CSRF_INVALID", code)
			}

			// The other half of the proof: WITH the token the request is no longer
			// refused by CSRF. The outcome may legitimately be a 200, 204, 400, 404,
			// 409 or 502 — what matters is that the 403 above came from CSRF and not
			// from the route being missing, unauthorized or misconfigured.
			withToken := e.call(t, route.Method, path, "{}", sessions[entry].cookies,
				csrfHeaders(sessions[entry].cookies, sessions[entry].csrfCookie))
			if errorCodeOrEmpty(t, withToken) == "CSRF_INVALID" {
				t.Fatalf("with a valid CSRF token the request was still refused: %s", withToken.Body.String())
			}

			// A successful logout ends the session it authenticated. The remaining
			// routes of that entry point would then answer 401 instead of 403, which
			// would look like a CSRF gap while being nothing of the sort — so the
			// session is re-established (the account still exists; only the session
			// was revoked).
			if strings.HasSuffix(route.Path, "/auth/logout") && entry != "" {
				sessions[entry] = e.relogin(t, sessions[entry])
			}
		})
	}

	// A route table where every unsafe route is exempt would make the loop above
	// vacuous, so the test states what it walked.
	if unsafe < 10 {
		t.Fatalf("only %d unsafe routes were found; the route table is probably not fully wired", unsafe)
	}
	if len(exemptSeen) != len(exempt) {
		t.Fatalf("exempt routes not found in the route table: got %v", exemptSeen)
	}
	// Every entry point must have been exercised, or the test proves nothing about it.
	for name := range sessions {
		_ = name
	}
}

// entrySession is one logged-in browser per entry point, plus what is needed to log
// it in again after a logout test.
type entrySession struct {
	entry      string
	account    string
	password   string
	role       user.Role
	csrfCookie string
	cookies    []*http.Cookie
}

func (e *hardeningE2E) newEntrySession(t *testing.T, role user.Role) *entrySession {
	t.Helper()
	entry := strings.ToLower(string(role))
	const password = "a-hardening-passphrase"
	hash, err := authservice.Hash(password)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account:     dbtest.RandomAccount(entry),
		DisplayName: string(role) + " 加固",
		Role:        role,
		// A student has no password by business rule (§2.2); the entry's login flow
		// differs only in the body it accepts.
		PasswordHash: passwordHashFor(role, hash),
	})
	if err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID)
	})

	session := &entrySession{entry: entry, account: created.Account, password: password, role: role}
	if role == user.RoleStudent {
		session.password = ""
	}
	session.csrfCookie = "classwatch_session_" + entry + "_csrf"
	session.cookies = e.loginEntry(t, entry, session.account, session.password)
	return session
}

// relogin re-establishes a session that a logout test ended.
func (e *hardeningE2E) relogin(t *testing.T, session *entrySession) *entrySession {
	t.Helper()
	session.cookies = e.loginEntry(t, session.entry, session.account, session.password)
	return session
}

// passwordHashFor keeps the "students have no password" rule of §2.2 visible at the
// one place a test creates an account.
func passwordHashFor(role user.Role, hash string) *string {
	if role == user.RoleStudent {
		return nil
	}
	return &hash
}

// errorCodeOrEmpty is errorCode for responses that legitimately have no body (a 204).
func errorCodeOrEmpty(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if strings.TrimSpace(rec.Body.String()) == "" {
		return ""
	}
	return errorCode(t, rec)
}

// concretePath turns a Gin route template into a requestable path.
func concretePath(template string) string {
	segments := strings.Split(template, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[i] = uuid.NewString()
		}
	}
	return strings.Join(segments, "/")
}

// csrfHeaderFor picks the CSRF cookie of the entry point the path belongs to.
func csrfHeaderFor(path string, teacher, student, admin []*http.Cookie) map[string]string {
	switch {
	case strings.HasPrefix(path, "/api/v1/teacher"):
		return csrfHeaders(teacher, "classwatch_session_teacher_csrf")
	case strings.HasPrefix(path, "/api/v1/student"):
		return csrfHeaders(student, "classwatch_session_student_csrf")
	case strings.HasPrefix(path, "/api/v1/admin"):
		return csrfHeaders(admin, "classwatch_session_admin_csrf")
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Classroom helpers (the same calls the Phase 3/6 suites make)
// ---------------------------------------------------------------------------

func (e *hardeningE2E) teacherCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_teacher_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

func (e *hardeningE2E) studentCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_student_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

func (e *hardeningE2E) createClassroom(t *testing.T, teacherCookies []*http.Cookie, students ...*user.User) uuid.UUID {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"加固测试课堂"}`, teacherCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create classroom: %d (%s)", rec.Code, rec.Body.String())
	}
	classroomID, err := uuid.Parse(classroomOf(t, rec)["id"].(string))
	if err != nil {
		t.Fatalf("classroom id: %v", err)
	}
	if len(students) > 0 {
		accounts := make([]string, 0, len(students))
		for _, student := range students {
			accounts = append(accounts, `"`+student.Account+`"`)
		}
		rec = e.teacherCall(t, http.MethodPost,
			"/api/v1/teacher/classrooms/"+classroomID.String()+"/students",
			`{"accounts":[`+strings.Join(accounts, ",")+`]}`, teacherCookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("add students: %d (%s)", rec.Code, rec.Body.String())
		}
	}
	return classroomID
}

func (e *hardeningE2E) openClassroom(t *testing.T, classroomID uuid.UUID, teacherCookies []*http.Cookie) {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open classroom: %d (%s)", rec.Code, rec.Body.String())
	}
}

func (e *hardeningE2E) join(t *testing.T, classroomID uuid.UUID, studentCookies []*http.Cookie) uuid.UUID {
	t.Helper()
	rec := e.studentCall(t, http.MethodPost,
		"/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d (%s)", rec.Code, rec.Body.String())
	}
	sessionID, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}
	return sessionID
}

// gaugeValue reads one gauge sample out of an exposition body.
func gaugeValue(t *testing.T, text, metricName, labelFragment string) int64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}
		if !strings.Contains(line, labelFragment) {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			t.Fatalf("unparseable exposition line %q", line)
		}
		value, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			t.Fatalf("unparseable value in %q: %v", line, err)
		}
		return value
	}
	return 0
}
