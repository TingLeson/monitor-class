package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The transport contract of the four Phase 6 endpoints: which middleware chain they sit
// behind, which status and code a rejected request gets, and exactly what the JSON looks
// like. The frontend is written against THIS file's assertions while the backend is
// still moving, which is why the response shapes are pinned member by member.
//
// The rules live in internal/session; the end-to-end version (real PostgreSQL, real
// roster, real sessions) is student_sessions_integration_test.go.

// ---------------------------------------------------------------------------
// Fake session service
// ---------------------------------------------------------------------------

type fakeSession struct {
	joinResult    *session.JoinResult
	joinErr       error
	leaveResult   *session.StudentSession
	leaveErr      error
	tokenResult   *session.TeacherTokenResult
	tokenErr      error
	monitorResult *session.MonitorView
	monitorErr    error

	lastJoin      session.JoinInput
	lastLeave     [2]uuid.UUID
	lastToken     session.TeacherTokenInput
	lastMonitorID uuid.UUID
	lastMonitorBy uuid.UUID

	calls []string
}

func (f *fakeSession) Join(_ context.Context, in session.JoinInput) (*session.JoinResult, error) {
	f.calls = append(f.calls, "join")
	f.lastJoin = in
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	return f.joinResult, nil
}

func (f *fakeSession) Leave(_ context.Context, sessionID, studentID uuid.UUID) (*session.StudentSession, error) {
	f.calls = append(f.calls, "leave")
	f.lastLeave = [2]uuid.UUID{sessionID, studentID}
	if f.leaveErr != nil {
		return nil, f.leaveErr
	}
	return f.leaveResult, nil
}

func (f *fakeSession) TeacherToken(_ context.Context, in session.TeacherTokenInput) (*session.TeacherTokenResult, error) {
	f.calls = append(f.calls, "teacherToken")
	f.lastToken = in
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return f.tokenResult, nil
}

func (f *fakeSession) Monitor(_ context.Context, classroomID, teacherID uuid.UUID) (*session.MonitorView, error) {
	f.calls = append(f.calls, "monitor")
	f.lastMonitorID, f.lastMonitorBy = classroomID, teacherID
	if f.monitorErr != nil {
		return nil, f.monitorErr
	}
	return f.monitorResult, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type sessionHarness struct {
	*routerHarness
	sessions *fakeSession
}

func newSessionHarness(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter) *sessionHarness {
	t.Helper()
	if cfg == nil {
		cfg = authTestConfig(t)
	}
	fakeAuthSvc := newFakeAuth()
	fakeSessionSvc := &fakeSession{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	router := NewRouter(Deps{
		Logger:  logger,
		Config:  cfg,
		Auth:    fakeAuthSvc,
		Session: fakeSessionSvc,
		Limiter: limiter,
	})
	return &sessionHarness{
		routerHarness: &routerHarness{router: router, auth: fakeAuthSvc, cfg: cfg, logs: logs},
		sessions:      fakeSessionSvc,
	}
}

// studentSession registers an ACTIVE student with a live session on the student entry
// and returns the account plus the two cookies that entry issues.
func (h *sessionHarness) studentSession(t *testing.T, account string) (*user.User, map[string]string) {
	t.Helper()
	u := h.student(t, account)
	token := "student-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_student":      token,
		"classwatch_session_student_csrf": principal.CSRFToken,
	}
}

// teacherSession registers an ACTIVE teacher with a live session on the teacher entry.
func (h *sessionHarness) teacherSession(t *testing.T, account string) (*user.User, map[string]string, uuid.UUID) {
	t.Helper()
	u := h.staff(t, user.RoleTeacher, account, "a-teacher-passphrase")
	token := "teacher-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_teacher":      token,
		"classwatch_session_teacher_csrf": principal.CSRFToken,
	}, principal.SessionID
}

// adminSession registers an ACTIVE administrator on the admin entry.
func (h *sessionHarness) adminSession(t *testing.T, account string) (*user.User, map[string]string) {
	t.Helper()
	u := h.staff(t, user.RoleAdmin, account, "an-admin-passphrase")
	token := "admin-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_admin":      token,
		"classwatch_session_admin_csrf": principal.CSRFToken,
	}
}

// sampleSession is a stored session for the fake service to return.
func sampleSession(t *testing.T, status session.Status) *session.StudentSession {
	t.Helper()
	id := uuid.New()
	now := time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC)
	return &session.StudentSession{
		ID:              id,
		ClassroomRunID:  uuid.New(),
		StudentID:       uuid.New(),
		LiveKitIdentity: id.String(),
		Status:          status,
		ConnectedAt:     &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// ---------------------------------------------------------------------------
// Middleware chain
// ---------------------------------------------------------------------------

// studentSessionRoutes is every student session route, with whether it writes.
func studentSessionRoutes() []struct {
	method string
	path   string
	body   string
	write  bool
} {
	return []struct {
		method string
		path   string
		body   string
		write  bool
	}{
		{http.MethodPost, "/api/v1/student/classrooms/" + uuid.New().String() + "/join", `{"capture":{"displaySurface":"monitor"}}`, true},
		{http.MethodPost, "/api/v1/student/sessions/" + uuid.New().String() + "/leave", "", true},
	}
}

// teacherSessionRoutes is every teacher media route, with whether it writes.
func teacherSessionRoutes() []struct {
	method string
	path   string
	body   string
	write  bool
} {
	return []struct {
		method string
		path   string
		body   string
		write  bool
	}{
		{http.MethodPost, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/media-token", "", true},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/monitor", "", false},
	}
}

// TestStudentSessionRoutesRequireSessionRoleAndCSRF is the §37/§63 test for the
// student group: anonymous is 401, a teacher or admin session is 403, and a write
// without the CSRF token is 403 — with the service never reached in any of them.
func TestStudentSessionRoutesRequireSessionRoleAndCSRF(t *testing.T) {
	for _, route := range studentSessionRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			anonymous := newSessionHarness(t, nil, nil)
			rec := anonymous.request(route.method, route.path, route.body, nil, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeAuthRequired) {
				t.Errorf("anonymous: code = %s, want AUTH_REQUIRED", code)
			}
			if len(anonymous.sessions.calls) != 0 {
				t.Errorf("anonymous: the service was called (%v)", anonymous.sessions.calls)
			}

			teacherHarness := newSessionHarness(t, nil, nil)
			_, teacherCookies, _ := teacherHarness.teacherSession(t, "teacher-01")
			rec = teacherHarness.request(route.method, route.path, route.body, teacherCookies,
				csrf(teacherCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("teacher: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("teacher: code = %s, want ROLE_FORBIDDEN", code)
			}
			if len(teacherHarness.sessions.calls) != 0 {
				t.Errorf("teacher: the service was called (%v)", teacherHarness.sessions.calls)
			}

			adminHarness := newSessionHarness(t, nil, nil)
			_, adminCookies := adminHarness.adminSession(t, "admin-01")
			rec = adminHarness.request(route.method, route.path, route.body, adminCookies,
				csrf(adminCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("admin: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}

			// A student session WITHOUT the CSRF header: authenticated, and still
			// refused. This is the check that stops any website from starting or
			// ending a media session with the student's cookie attached.
			studentHarness := newSessionHarness(t, nil, nil)
			_, studentCookies := studentHarness.studentSession(t, "s10086")
			rec = studentHarness.request(route.method, route.path, route.body, studentCookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("missing csrf: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeCSRFInvalid) {
				t.Errorf("missing csrf: code = %s, want CSRF_INVALID", code)
			}
			if len(studentHarness.sessions.calls) != 0 {
				t.Errorf("missing csrf: the service was called (%v)", studentHarness.sessions.calls)
			}
		})
	}
}

// TestTeacherSessionRoutesRequireSessionRoleAndCSRF is the same test for the teacher
// group, including §4: an administrator is not a super-teacher.
func TestTeacherSessionRoutesRequireSessionRoleAndCSRF(t *testing.T) {
	for _, route := range teacherSessionRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			anonymous := newSessionHarness(t, nil, nil)
			rec := anonymous.request(route.method, route.path, route.body, nil, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}

			studentHarness := newSessionHarness(t, nil, nil)
			_, studentCookies := studentHarness.studentSession(t, "s10086")
			rec = studentHarness.request(route.method, route.path, route.body, studentCookies,
				csrf(studentCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("student: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if len(studentHarness.sessions.calls) != 0 {
				t.Errorf("student: the service was called (%v)", studentHarness.sessions.calls)
			}

			adminHarness := newSessionHarness(t, nil, nil)
			_, adminCookies := adminHarness.adminSession(t, "admin-01")
			rec = adminHarness.request(route.method, route.path, route.body, adminCookies,
				csrf(adminCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("admin: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTeacherMediaTokenRequiresCSRF: minting a media credential is a write, and it is
// protected like one.
func TestTeacherMediaTokenRequiresCSRF(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies, _ := h.teacherSession(t, "teacher-01")
	path := "/api/v1/teacher/classrooms/" + uuid.New().String() + "/media-token"

	rec := h.request(http.MethodPost, path, "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeCSRFInvalid) {
		t.Errorf("code = %s, want CSRF_INVALID", code)
	}
	if len(h.sessions.calls) != 0 {
		t.Errorf("the service was called without a CSRF token (%v)", h.sessions.calls)
	}
}

// ---------------------------------------------------------------------------
// POST /student/classrooms/:id/join
// ---------------------------------------------------------------------------

// TestJoinResponseShape pins the frozen join contract: exactly sessionId, livekitUrl
// and token — no room name, no permissions echo, no session status.
func TestJoinResponseShape(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	student, cookies := h.studentSession(t, "s10086")
	stored := sampleSession(t, session.StatusConnecting)
	h.sessions.joinResult = &session.JoinResult{
		Session:    stored,
		LiveKitURL: "wss://media.example.test",
		Token:      "header.payload.signature",
	}

	classroomID := uuid.New()
	body := `{"capture":{"displaySurface":"monitor","width":1920,"height":1080}}`
	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", body,
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// The student id and the classroom id must come from the session and the path —
	// never from the body, which the client controls.
	if h.sessions.lastJoin.StudentID != student.ID {
		t.Errorf("service asked for student %s, want the session's %s", h.sessions.lastJoin.StudentID, student.ID)
	}
	if h.sessions.lastJoin.ClassroomID != classroomID {
		t.Errorf("service asked for classroom %s, want %s", h.sessions.lastJoin.ClassroomID, classroomID)
	}
	if h.sessions.lastJoin.Capture == nil || h.sessions.lastJoin.Capture.DisplaySurface != "monitor" ||
		h.sessions.lastJoin.Capture.Width != 1920 || h.sessions.lastJoin.Capture.Height != 1080 {
		t.Errorf("capture = %+v, want the diagnostics the client sent", h.sessions.lastJoin.Capture)
	}

	parsed := jsonBodyOf(t, rec)
	if len(parsed) != 3 {
		t.Errorf("response has %d members (%v), want exactly sessionId, livekitUrl, token", len(parsed), parsed)
	}
	if parsed["sessionId"] != stored.ID.String() {
		t.Errorf("sessionId = %v, want %s", parsed["sessionId"], stored.ID)
	}
	if parsed["livekitUrl"] != "wss://media.example.test" {
		t.Errorf("livekitUrl = %v", parsed["livekitUrl"])
	}
	if parsed["token"] != "header.payload.signature" {
		t.Errorf("token = %v", parsed["token"])
	}
	// The room name is a media-plane handle: it must not appear anywhere in the body.
	if strings.Contains(rec.Body.String(), "lk_") {
		t.Errorf("the response leaked the LiveKit room name: %s", rec.Body.String())
	}
}

// TestJoinAcceptsAnEmptyBody: every member of the join body is optional (§43), because
// the gate has already run in the browser and a client without diagnostics must still
// be able to enter.
func TestJoinAcceptsAnEmptyBody(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")
	h.sessions.joinResult = &session.JoinResult{Session: sampleSession(t, session.StatusConnecting), LiveKitURL: "wss://x", Token: "t"}

	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if h.sessions.lastJoin.Capture != nil {
		t.Errorf("capture = %+v, want nil for an empty body", h.sessions.lastJoin.Capture)
	}
}

// TestJoinRejectsUnusableDiagnostics: the diagnostics block is bounded, but a value the
// server disagrees with never decides the join (that would make it a gate, §19/§43).
func TestJoinRejectsUnusableDiagnostics(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")
	headers := csrf(cookies)
	path := "/api/v1/student/classrooms/" + uuid.New().String() + "/join"

	tests := map[string]string{
		"unknown member":   `{"capture":{"displaySurface":"monitor"},"device":"cam"}`,
		"surface too long": `{"capture":{"displaySurface":"` + strings.Repeat("m", 65) + `"}}`,
		"negative pixels":  `{"capture":{"displaySurface":"monitor","width":-1}}`,
		"absurd pixels":    `{"capture":{"displaySurface":"monitor","height":100001}}`,
		"wrong type":       `{"capture":{"displaySurface":42}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rec := h.request(http.MethodPost, path, body, cookies, headers, "203.0.113.9:44444")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
				t.Errorf("code = %s, want INVALID_REQUEST", code)
			}
		})
	}
	if len(h.sessions.calls) != 0 {
		t.Errorf("the service was called for a malformed body (%v)", h.sessions.calls)
	}
}

// TestJoinTransportsTheDiagnosticsVerbatim: the value is forwarded as data and never
// validated against "monitor" — a client that says "browser" still gets in, because the
// server cannot prove otherwise and §43 forbids pretending it can.
func TestJoinTransportsTheDiagnosticsVerbatim(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")
	h.sessions.joinResult = &session.JoinResult{Session: sampleSession(t, session.StatusConnecting), LiveKitURL: "wss://x", Token: "t"}

	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join",
		`{"capture":{"displaySurface":"browser","width":800,"height":600}}`,
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: diagnostics must not gate the join (%s)", rec.Code, rec.Body.String())
	}
	if h.sessions.lastJoin.Capture == nil || h.sessions.lastJoin.Capture.DisplaySurface != "browser" {
		t.Errorf("capture = %+v, want the client's own value", h.sessions.lastJoin.Capture)
	}
}

// TestJoinErrorMapping pins the four refusals of §43/§58 by code and status.
func TestJoinErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   apperr.Code
	}{
		{"not assigned", classroom.ErrStudentNotAssigned, http.StatusNotFound, apperr.CodeStudentNotAssigned},
		{"unknown classroom", classroom.ErrNotFound, http.StatusNotFound, apperr.CodeClassroomNotFound},
		{"closed", session.ErrClassroomClosed, http.StatusConflict, apperr.CodeClassroomClosed},
		{"media down", session.ErrMediaUnavailable, http.StatusBadGateway, apperr.CodeMediaTokenFailed},
		{"unexpected", context.DeadlineExceeded, http.StatusInternalServerError, apperr.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionHarness(t, nil, nil)
			_, cookies := h.studentSession(t, "s10086")
			h.sessions.joinErr = tc.err

			rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join", "",
				cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(tc.wantCode) {
				t.Errorf("code = %s, want %s", code, tc.wantCode)
			}
			// The internal cause must never be rendered to the client (§58).
			if strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Errorf("the response echoed the internal cause: %s", rec.Body.String())
			}
		})
	}
}

// TestJoinRejectsMalformedClassroomID: a value that is not an identifier is a bad
// request, not a missing classroom.
func TestJoinRejectsMalformedClassroomID(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")

	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/not-a-uuid/join", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
		t.Errorf("code = %s, want INVALID_REQUEST", code)
	}
	if len(h.sessions.calls) != 0 {
		t.Errorf("the service was called for a malformed id (%v)", h.sessions.calls)
	}
}

// TestJoinNeverLogsTheToken is §59 at the transport level: the token is in the body and
// nowhere in the access log, the error log or the response headers.
//
// The configuration carries a real (fake) LiveKit secret so the assertion has something
// to look for: a test that scans for an empty string would pass forever.
func TestJoinNeverLogsTheToken(t *testing.T) {
	const apiSecret = "super-secret-livekit-api-secret"
	cfg := authTestConfig(t)
	cfg.LiveKitAPISecret = apiSecret
	cfg.LiveKitURL = "wss://media.example.test"
	h := newSessionHarness(t, cfg, nil)
	_, cookies := h.studentSession(t, "s10086")
	const token = "eyJhbGciOiJIUzI1NiJ9.classwatch-secret-token.signature"
	h.sessions.joinResult = &session.JoinResult{
		Session: sampleSession(t, session.StatusConnecting), LiveKitURL: "wss://media.example.test", Token: token,
	}

	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Fatal("the token is missing from the response body")
	}
	if strings.Contains(h.logs.String(), token) {
		t.Fatalf("the media token was logged:\n%s", h.logs.String())
	}
	// The API secret is never part of any response or log line, on any path (§44/§59).
	if strings.Contains(rec.Body.String(), apiSecret) {
		t.Fatal("the API secret appeared in a response body")
	}
	if strings.Contains(h.logs.String(), apiSecret) {
		t.Fatalf("the API secret was logged:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// POST /student/sessions/:id/leave
// ---------------------------------------------------------------------------

// TestLeaveReturnsNoContent: 204 and an empty body, so no client can start depending on
// the shape of a response about a finished session.
func TestLeaveReturnsNoContent(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	student, cookies := h.studentSession(t, "s10086")
	stored := sampleSession(t, session.StatusLeft)
	h.sessions.leaveResult = stored

	rec := h.request(http.MethodPost, "/api/v1/student/sessions/"+stored.ID.String()+"/leave", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if h.sessions.lastLeave[0] != stored.ID || h.sessions.lastLeave[1] != student.ID {
		t.Errorf("service called with %v, want (%s, %s)", h.sessions.lastLeave, stored.ID, student.ID)
	}
}

// TestLeaveNotFoundDoesNotLeakOtherSessions is §58: another student's session and a
// nonexistent one are the same answer.
func TestLeaveNotFoundDoesNotLeakOtherSessions(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")
	h.sessions.leaveErr = session.ErrSessionNotFound

	rec := h.request(http.MethodPost, "/api/v1/student/sessions/"+uuid.New().String()+"/leave", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeSessionNotFound) {
		t.Errorf("code = %s, want SESSION_NOT_FOUND", code)
	}
}

// TestLeaveRejectsMalformedSessionID keeps a non-identifier out of the database.
func TestLeaveRejectsMalformedSessionID(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")

	rec := h.request(http.MethodPost, "/api/v1/student/sessions/12345/leave", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(h.sessions.calls) != 0 {
		t.Errorf("the service was called for a malformed id (%v)", h.sessions.calls)
	}
}

// ---------------------------------------------------------------------------
// POST /teacher/classrooms/:id/media-token
// ---------------------------------------------------------------------------

// TestTeacherMediaTokenResponseShape pins the frozen contract and the identity rule of
// §44: the participant identity is the LOGIN session id, taken from the session.
func TestTeacherMediaTokenResponseShape(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	teacher, cookies, loginSessionID := h.teacherSession(t, "teacher-01")
	h.sessions.tokenResult = &session.TeacherTokenResult{LiveKitURL: "wss://media.example.test", Token: "header.payload.signature"}

	classroomID := uuid.New()
	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "",
		cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if h.sessions.lastToken.TeacherID != teacher.ID {
		t.Errorf("service asked for teacher %s, want the session's %s", h.sessions.lastToken.TeacherID, teacher.ID)
	}
	if h.sessions.lastToken.ClassroomID != classroomID {
		t.Errorf("service asked for classroom %s, want %s", h.sessions.lastToken.ClassroomID, classroomID)
	}
	// §44: teacher identity = teacher_session UUID, never the account or the user id.
	if h.sessions.lastToken.SessionID != loginSessionID {
		t.Errorf("session id = %s, want the login session %s", h.sessions.lastToken.SessionID, loginSessionID)
	}

	body := jsonBodyOf(t, rec)
	if len(body) != 2 {
		t.Errorf("response has %d members (%v), want exactly livekitUrl and token", len(body), body)
	}
	if body["livekitUrl"] != "wss://media.example.test" || body["token"] != "header.payload.signature" {
		t.Errorf("body = %v", body)
	}
	if strings.Contains(h.logs.String(), "header.payload.signature") {
		t.Fatal("the teacher media token was logged")
	}
}

// TestTeacherMediaTokenErrorMapping pins the two refusals of §43/§58.
func TestTeacherMediaTokenErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   apperr.Code
	}{
		{"not the owner", classroom.ErrNotOwner, http.StatusForbidden, apperr.CodeClassroomNotOwner},
		{"unknown classroom", classroom.ErrNotFound, http.StatusNotFound, apperr.CodeClassroomNotFound},
		{"closed", session.ErrClassroomClosed, http.StatusConflict, apperr.CodeClassroomClosed},
		{"media down", session.ErrMediaUnavailable, http.StatusBadGateway, apperr.CodeMediaTokenFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionHarness(t, nil, nil)
			_, cookies, _ := h.teacherSession(t, "teacher-01")
			h.sessions.tokenErr = tc.err

			rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/media-token", "",
				cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(tc.wantCode) {
				t.Errorf("code = %s, want %s", code, tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET /teacher/classrooms/:id/monitor
// ---------------------------------------------------------------------------

// TestMonitorResponseShape pins the §51/§29 DTO member by member: the frontend's
// monitoring wall is written against this exact object, including the nested media
// blocks, and including the two members that are null for a student who has not entered.
func TestMonitorResponseShape(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	teacher, cookies, _ := h.teacherSession(t, "teacher-01")

	joined := time.Date(2026, 3, 4, 19, 3, 0, 0, time.UTC)
	event := time.Date(2026, 3, 4, 19, 31, 42, 0, time.UTC)
	online := sampleSession(t, session.StatusOnline)
	online.ConnectedAt = &joined
	online.UpdatedAt = event
	lost := sampleSession(t, session.StatusScreenLost)
	lost.ConnectedAt = nil
	lost.UpdatedAt = event
	onlineStatus, lostStatus := session.StatusOnline, session.StatusScreenLost
	// A student who is authorized but has never entered: the null tile of §29.
	never := session.MonitorStudent{StudentID: uuid.New(), DisplayName: "王五", Connection: session.ConnectionUnknown}
	h.sessions.monitorResult = &session.MonitorView{
		MediaObserved: true,
		Students: []session.MonitorStudent{
			{
				StudentID: online.StudentID, DisplayName: "张三", SessionID: &online.ID,
				Status: &onlineStatus, ScreenActive: true, Connection: session.ConnectionGood,
				JoinedAt: &joined, LastEventAt: &event,
			},
			{
				StudentID: lost.StudentID, DisplayName: "李四", SessionID: &lost.ID,
				Status: &lostStatus, ScreenActive: false, CameraActive: false, MicrophoneActive: false,
				Connection: session.ConnectionUnknown, LastEventAt: &event,
			},
			never,
		},
	}

	classroomID := uuid.New()
	rec := h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "",
		cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if h.sessions.lastMonitorID != classroomID || h.sessions.lastMonitorBy != teacher.ID {
		t.Errorf("service called with (%s, %s), want (%s, %s)", h.sessions.lastMonitorID, h.sessions.lastMonitorBy, classroomID, teacher.ID)
	}

	body := jsonBodyOf(t, rec)
	students, ok := body["students"].([]any)
	if !ok || len(students) != 3 {
		t.Fatalf("students = %v, want three tiles", body["students"])
	}
	if len(body) != 1 {
		t.Errorf("response has %d members (%v), want only students", len(body), body)
	}

	first, _ := students[0].(map[string]any)
	wantMembers := []string{
		"studentId", "displayName", "sessionId", "sessionStatus",
		"screen", "camera", "microphone", "connection", "joinedAt", "lastEventAt",
	}
	if len(first) != len(wantMembers) {
		t.Errorf("tile has %d members (%v), want %d", len(first), first, len(wantMembers))
	}
	for _, member := range wantMembers {
		if _, ok := first[member]; !ok {
			t.Errorf("tile is missing %q", member)
		}
	}
	if first["studentId"] != online.StudentID.String() || first["displayName"] != "张三" || first["sessionId"] != online.ID.String() {
		t.Errorf("tile identity = %v", first)
	}
	if first["sessionStatus"] != "ONLINE" || first["connection"] != "GOOD" {
		t.Errorf("tile status = %v/%v, want ONLINE/GOOD", first["sessionStatus"], first["connection"])
	}
	if screen, ok := first["screen"].(map[string]any); !ok || screen["active"] != true {
		t.Errorf("screen = %v, want {active:true}", first["screen"])
	}
	if first["joinedAt"] != "2026-03-04T19:03:00Z" || first["lastEventAt"] != "2026-03-04T19:31:42Z" {
		t.Errorf("timestamps = %v / %v, want RFC3339", first["joinedAt"], first["lastEventAt"])
	}

	second, _ := students[1].(map[string]any)
	if second["sessionStatus"] != "SCREEN_LOST" || second["connection"] != "UNKNOWN" {
		t.Errorf("second tile = %v", second)
	}
	if second["joinedAt"] != nil {
		t.Errorf("joinedAt = %v, want null for a student never observed in the room", second["joinedAt"])
	}

	// The tile of a student who never entered: the two session members must be JSON
	// null (not "", not a zero UUID, and not a seventh status), and the media blocks must
	// be present and false so the card renders the same shape as every other card.
	third, _ := students[2].(map[string]any)
	if third["sessionId"] != nil || third["sessionStatus"] != nil {
		t.Errorf("session members = %v/%v, want null for a student with no session", third["sessionId"], third["sessionStatus"])
	}
	if third["connection"] != "UNKNOWN" || third["joinedAt"] != nil || third["lastEventAt"] != nil {
		t.Errorf("third tile = %v, want UNKNOWN connection and null timestamps", third)
	}
	for _, member := range []string{"screen", "camera", "microphone"} {
		track, ok := third[member].(map[string]any)
		if !ok || track["active"] != false {
			t.Errorf("%s = %v, want {active:false}", member, third[member])
		}
	}
}

// TestMonitorEmptyWallIsAnArray: "this classroom authorizes nobody" must serialise as []
// so the console renders its empty state instead of special-casing null.
func TestMonitorEmptyWallIsAnArray(t *testing.T) {
	h := newSessionHarness(t, nil, nil)
	_, cookies, _ := h.teacherSession(t, "teacher-01")
	h.sessions.monitorResult = &session.MonitorView{Students: []session.MonitorStudent{}}

	rec := h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/monitor", "",
		cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"students":[]`) {
		t.Errorf("body = %s, want an empty array", rec.Body.String())
	}
}

// TestMonitorErrorMapping pins the two refusals of §51.
func TestMonitorErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   apperr.Code
	}{
		{"not the owner", classroom.ErrNotOwner, http.StatusForbidden, apperr.CodeClassroomNotOwner},
		{"closed", session.ErrClassroomClosed, http.StatusConflict, apperr.CodeClassroomClosed},
		{"unknown classroom", classroom.ErrNotFound, http.StatusNotFound, apperr.CodeClassroomNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionHarness(t, nil, nil)
			_, cookies, _ := h.teacherSession(t, "teacher-01")
			h.sessions.monitorErr = tc.err

			rec := h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/monitor", "",
				cookies, nil, "203.0.113.9:44444")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(tc.wantCode) {
				t.Errorf("code = %s, want %s", code, tc.wantCode)
			}
		})
	}
}

// TestSessionRoutesAreAbsentWithoutASessionService keeps the "no service, no routes"
// rule: an API that cannot record a session must answer 404 rather than exist without
// an authorization chain behind it.
func TestSessionRoutesAreAbsentWithoutASessionService(t *testing.T) {
	h := newHarness(t, nil, nil) // auth, and no session service
	student := h.student(t, "s10086")
	principal := h.auth.addSession("student-token-s10086", student)
	cookies := map[string]string{"classwatch_session_student": "student-token-s10086"}

	rec := h.request(http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join", "",
		cookies, map[string]string{"X-CSRF-Token": principal.CSRFToken}, "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("student join: status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}

	teacher := h.staff(t, user.RoleTeacher, "teacher-01", "a-teacher-passphrase")
	teacherPrincipal := h.auth.addSession("teacher-token-teacher-01", teacher)
	rec = h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/monitor", "",
		map[string]string{"classwatch_session_teacher": "teacher-token-teacher-01"},
		map[string]string{"X-CSRF-Token": teacherPrincipal.CSRFToken}, "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("teacher monitor: status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}
