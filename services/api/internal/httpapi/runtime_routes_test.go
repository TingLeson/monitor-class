package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/realtime"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The Phase 8 HTTP surface without PostgreSQL: the webhook's failure policy and the two
// socket routes' authorization chain.
//
// WHAT is asserted here and not in the integration test: the DECISIONS — 401 for an
// unverified sender, 200 for a verified one, 500 when the control plane could not record
// what it saw, and 401/403 before a socket is ever upgraded. The integration test proves
// the same paths against real signatures, a real database and real sockets.

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeVerifier struct {
	event *livekit.WebhookEvent
	err   error
	// calls counts verifications, so "the processor must not run" is assertable.
	calls int
	// lastBody is what the verifier was handed, which is how the test proves the
	// handler does not consume the body before verification.
	lastBody string
}

func (f *fakeVerifier) Receive(r *http.Request) (*livekit.WebhookEvent, error) {
	f.calls++
	if r.Body != nil {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		f.lastBody = string(buf[:n])
	}
	return f.event, f.err
}

type fakeProcessor struct {
	calls   int
	last    *livekit.WebhookEvent
	err     error
	handled bool
}

func (f *fakeProcessor) ProcessWebhook(_ context.Context, event *livekit.WebhookEvent) error {
	f.calls++
	f.last = event
	if f.err != nil {
		return f.err
	}
	f.handled = true
	return nil
}

type fakeSocket struct {
	calls []struct {
		role   realtime.Role
		userID uuid.UUID
	}
	// refusals records the close codes handed to a rejected handshake.
	refusals []struct {
		code   int
		reason string
	}
	err error
}

func (f *fakeSocket) Serve(_ http.ResponseWriter, _ *http.Request, role realtime.Role, userID uuid.UUID) error {
	f.calls = append(f.calls, struct {
		role   realtime.Role
		userID uuid.UUID
	}{role: role, userID: userID})
	return f.err
}

func (f *fakeSocket) Refuse(_ http.ResponseWriter, _ *http.Request, code int, reason string) error {
	f.refusals = append(f.refusals, struct {
		code   int
		reason string
	}{code: code, reason: reason})
	return nil
}

// runtimeConfig is the configuration the Phase 8 routes need.
func runtimeConfig() *config.Config {
	return &config.Config{
		AppEnv:                           config.EnvTest,
		SessionCookieName:                "classwatch_session",
		SessionTTL:                       time.Hour,
		SessionCookieSecure:              false,
		RateLimitLoginPerMinute:          1000,
		RateLimitLoginPerAccountPer10Min: 1000,
		RateLimitAPIPerMinute:            1, // deliberately tiny: see the limiter test
		PasswordMinLength:                12,
	}
}

type runtimeHarness struct {
	router    *gin.Engine
	auth      *fakeAuth
	verifier  *fakeVerifier
	processor *fakeProcessor
	socket    *fakeSocket
}

func newRuntimeHarness(t *testing.T, cfg *config.Config, withWebhook bool) *runtimeHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if cfg == nil {
		cfg = runtimeConfig()
	}
	auth := newFakeAuth()
	verifier := &fakeVerifier{event: &livekit.WebhookEvent{Event: "room_started", Id: uuid.NewString()}}
	processor := &fakeProcessor{}
	socket := &fakeSocket{}

	deps := Deps{
		Logger:  discardLogger(),
		Config:  cfg,
		Auth:    auth,
		Limiter: ratelimit.NewMemory(),
		Socket:  socket,
	}
	if withWebhook {
		deps.Webhook = &WebhookDeps{Verifier: verifier, Processor: processor}
	}
	return &runtimeHarness{
		router:    NewRouter(deps),
		auth:      auth,
		verifier:  verifier,
		processor: processor,
		socket:    socket,
	}
}

// account registers an ACTIVE account on the fake auth service. The password is only
// needed because the fake's password login path expects one; these tests authenticate by
// session token.
func (h *runtimeHarness) account(role user.Role, account string) *user.User {
	u := &user.User{
		ID:          uuid.New(),
		Account:     account,
		DisplayName: "Test " + account,
		Role:        role,
		Status:      user.StatusActive,
		CreatedAt:   time.Now().UTC(),
	}
	password := ""
	if role != user.RoleStudent {
		hash := "hash-not-used-by-the-fake"
		u.PasswordHash = &hash
		password = "correct-horse-battery"
	}
	h.auth.addUser(u, password)
	return u
}

func (h *runtimeHarness) postWebhook(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/livekit/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "a-signed-token")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *runtimeHarness) getSocket(path string, cookies map[string]string) *httptest.ResponseRecorder {
	return h.getSocketRequest(path, cookies, false)
}

// getSocketWithUpgrade sends the headers a browser sends for a WebSocket handshake, which
// is what selects the close-code presentation of an authentication failure.
func (h *runtimeHarness) getSocketWithUpgrade(path string, cookies map[string]string) *httptest.ResponseRecorder {
	return h.getSocketRequest(path, cookies, true)
}

func (h *runtimeHarness) getSocketRequest(path string, cookies map[string]string, upgrade bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Webhook
// ---------------------------------------------------------------------------

func TestWebhookRejectsAnUnverifiedSenderWith401(t *testing.T) {
	h := newRuntimeHarness(t, nil, true)
	h.verifier.event = nil
	h.verifier.err = errors.New("livekit: webhook verification failed: checksum mismatch")

	rec := h.postWebhook(t, `{"event":"room_started"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a forged webhook must not be answered with 200", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}
	if h.processor.calls != 0 {
		t.Fatal("an unverified event reached the state machine")
	}
	// The body was handed to the verifier (the signature covers it) and consumed there;
	// what must NOT happen is the handler parsing it on its own.
	if h.verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", h.verifier.calls)
	}
}

func TestWebhookRejectionLogsWithoutTheBody(t *testing.T) {
	h := newRuntimeHarness(t, nil, true)
	h.verifier.err = errors.New("livekit: webhook verification failed: no authorization header")

	buf := &strings.Builder{}
	h.router = NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config:  runtimeConfig(),
		Auth:    h.auth,
		Limiter: ratelimit.NewMemory(),
		Socket:  h.socket,
		Webhook: &WebhookDeps{Verifier: h.verifier, Processor: h.processor},
	})

	secretLooking := `{"event":"track_published","token":"super-secret-value"}`
	h.postWebhook(t, secretLooking)

	logs := buf.String()
	if !strings.Contains(logs, "livekit webhook rejected") {
		t.Fatalf("rejection was not logged: %s", logs)
	}
	if strings.Contains(logs, "super-secret-value") {
		t.Fatal("the request body was logged: it is unauthenticated input and may carry a credential")
	}
	if strings.Contains(logs, "a-signed-token") {
		t.Fatal("the Authorization header was logged (§59)")
	}
	if !strings.Contains(logs, "remote_ip") {
		t.Fatalf("the remote address is missing from the rejection line: %s", logs)
	}
}

func TestVerifiedWebhookIsAppliedAndAnsweredWith200(t *testing.T) {
	h := newRuntimeHarness(t, nil, true)

	rec := h.postWebhook(t, `{"event":"room_started"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("body = %v, want a three-token acknowledgement", body)
	}
	if h.processor.calls != 1 || !h.processor.handled {
		t.Fatalf("processor calls = %d", h.processor.calls)
	}
	if h.processor.last != h.verifier.event {
		t.Fatal("the processor received a different event than the verifier returned")
	}
}

func TestWebhookAsksLiveKitToRetryWhenTheStateCannotBeRecorded(t *testing.T) {
	h := newRuntimeHarness(t, nil, true)
	h.processor.err = errors.New("session: database is down")

	rec := h.postWebhook(t, `{"event":"track_published"}`)

	// 500, not 200: dropping the observation would leave the teacher's wall in a state
	// the media plane already left. Every transition is idempotent, so the retry is safe.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWebhookRouteIsAbsentWhenNotWired(t *testing.T) {
	h := newRuntimeHarness(t, nil, false)
	rec := h.postWebhook(t, `{"event":"room_started"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: an unverifiable endpoint must not exist", rec.Code)
	}
}

func TestWebhookIsNotSubjectToTheAPIRateLimit(t *testing.T) {
	// The API budget is ONE request per minute in this configuration. The webhook lives
	// outside /api/v1 precisely so LiveKit's delivery cannot be throttled by a browser's
	// budget — and so a burst of events is never dropped for a reason the sender cannot
	// act on (the signature is this endpoint's authentication, §63).
	h := newRuntimeHarness(t, nil, true)
	for i := 0; i < 5; i++ {
		if rec := h.postWebhook(t, `{"event":"room_started"}`); rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i+1, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// WebSocket routes
// ---------------------------------------------------------------------------

func TestStudentSocketRequiresASession(t *testing.T) {
	// A plain GET (no Upgrade header) is not a WebSocket handshake, so it is answered with
	// the standard envelope — which is what an ops probe or a health check sees.
	h := newRuntimeHarness(t, nil, false)
	rec := h.getSocket("/ws/student", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}
	if len(h.socket.calls) != 0 {
		t.Fatal("a socket was served for an unauthenticated caller")
	}
	if len(h.socket.refusals) != 0 {
		t.Fatal("a non-WebSocket request must not be upgraded just to be refused")
	}
}

func TestSocketHandshakeWithoutASessionIsRefusedWithACloseCode(t *testing.T) {
	// A real WebSocket handshake cannot read an HTTP status, so it is accepted and closed
	// with 4401 instead — the frontend's only way to tell "log in again" from "retry".
	h := newRuntimeHarness(t, nil, false)
	rec := h.getSocketWithUpgrade("/ws/student", nil)
	if len(h.socket.refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly one", h.socket.refusals)
	}
	if got := h.socket.refusals[0]; got.code != realtime.CloseSessionInvalid {
		t.Fatalf("close code = %d, want %d", got.code, realtime.CloseSessionInvalid)
	}
	if len(h.socket.calls) != 0 {
		t.Fatal("a socket was served for an unauthenticated caller")
	}
	_ = rec
}

func TestStudentSocketRefusesATeacherSession(t *testing.T) {
	h := newRuntimeHarness(t, nil, false)
	teacher := h.account(user.RoleTeacher, "teacher-01")
	principal := h.auth.addSession("teacher-token", teacher)

	rec := h.getSocket("/ws/student", map[string]string{
		"classwatch_session_teacher": "teacher-token",
	})
	// The teacher's cookie is presented to the STUDENT entry: the caller is authenticated
	// but on the wrong entry, which is 403 and never a socket.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (principal=%v)", rec.Code, principal != nil)
	}
	if len(h.socket.calls) != 0 {
		t.Fatal("a socket was served across entry points")
	}

	// The same rejection over a real handshake is a close code the browser can read.
	h.socket.refusals = nil
	h.getSocketWithUpgrade("/ws/student", map[string]string{
		"classwatch_session_teacher": "teacher-token",
	})
	if len(h.socket.refusals) != 1 || h.socket.refusals[0].code != realtime.CloseRoleForbidden {
		t.Fatalf("refusals = %+v, want one %d", h.socket.refusals, realtime.CloseRoleForbidden)
	}
}

func TestStudentSocketIsServedForTheAuthenticatedPrincipal(t *testing.T) {
	h := newRuntimeHarness(t, nil, false)
	student := h.account(user.RoleStudent, "S10086")
	h.auth.addSession("student-token", student)

	rec := h.getSocket("/ws/student", map[string]string{
		"classwatch_session_student": "student-token",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the fake's 200", rec.Code)
	}
	if len(h.socket.calls) != 1 {
		t.Fatalf("socket calls = %d, want 1", len(h.socket.calls))
	}
	if got := h.socket.calls[0]; got.role != realtime.RoleStudent || got.userID != student.ID {
		t.Fatalf("served %+v, want the student role and the session's user id", got)
	}
}

func TestTeacherSocketIsServedForTheAuthenticatedPrincipal(t *testing.T) {
	h := newRuntimeHarness(t, nil, false)
	teacher := h.account(user.RoleTeacher, "teacher-01")
	h.auth.addSession("teacher-token", teacher)

	rec := h.getSocket("/ws/teacher", map[string]string{
		"classwatch_session_teacher": "teacher-token",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the fake's 200", rec.Code)
	}
	if len(h.socket.calls) != 1 || h.socket.calls[0].role != realtime.RoleTeacher {
		t.Fatalf("socket calls = %+v", h.socket.calls)
	}
	if h.socket.calls[0].userID != teacher.ID {
		t.Fatalf("served user %s, want %s", h.socket.calls[0].userID, teacher.ID)
	}
}

func TestSocketRoutesAreAbsentWithoutAHub(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(Deps{Logger: discardLogger(), Config: runtimeConfig(), Auth: newFakeAuth()})
	req := httptest.NewRequest(http.MethodGet, "/ws/student", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
