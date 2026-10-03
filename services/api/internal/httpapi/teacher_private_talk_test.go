package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/session"
)

// These tests pin the TRANSPORT contract of the three §31 endpoints: which middleware chain
// they sit behind, which status and code a rejection gets, and exactly what the JSON looks
// like. The frontend is written against THIS file's assertions while the state machine
// moves, which is why the response shapes are asserted member by member.
//
// The rules live in internal/session; the end-to-end version (real PostgreSQL, real roster,
// real media subscription state) is teacher_private_talk_integration_test.go.

// ---------------------------------------------------------------------------
// Fake private-talk service
// ---------------------------------------------------------------------------

type fakePrivateTalk struct {
	startResult *session.PrivateTalkView
	startErr    error
	stopErr     error
	getResult   *session.PrivateTalkView
	getErr      error

	lastStart session.StartPrivateTalkInput
	lastStop  session.StopPrivateTalkInput
	lastGetID uuid.UUID
	lastGetBy uuid.UUID

	calls []string
}

func (f *fakePrivateTalk) StartPrivateTalk(_ context.Context, in session.StartPrivateTalkInput) (*session.PrivateTalkView, error) {
	f.calls = append(f.calls, "start")
	f.lastStart = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.startResult, nil
}

func (f *fakePrivateTalk) StopPrivateTalk(_ context.Context, in session.StopPrivateTalkInput) error {
	f.calls = append(f.calls, "stop")
	f.lastStop = in
	return f.stopErr
}

func (f *fakePrivateTalk) PrivateTalk(_ context.Context, classroomID, teacherID uuid.UUID) (*session.PrivateTalkView, error) {
	f.calls = append(f.calls, "get")
	f.lastGetID, f.lastGetBy = classroomID, teacherID
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type privateTalkHarness struct {
	*sessionHarness
	talk *fakePrivateTalk
}

func newPrivateTalkHarness(t *testing.T, cfg *config.Config) *privateTalkHarness {
	t.Helper()
	if cfg == nil {
		cfg = authTestConfig(t)
	}
	base := newSessionHarness(t, cfg, ratelimit.NewMemory())
	talk := &fakePrivateTalk{}
	// Rebuild the router with the third service wired: the harness above exists to test the
	// session surface, and this one adds the §31 routes to the same middleware chain.
	base.routerHarness.router = NewRouter(Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:      cfg,
		Auth:        base.auth,
		Session:     base.sessions,
		PrivateTalk: talk,
		Limiter:     ratelimit.NewMemory(),
	})
	return &privateTalkHarness{sessionHarness: base, talk: talk}
}

// privateTalkPath is one classroom's private-talk URL.
func privateTalkPath(classroomID uuid.UUID) string {
	return "/api/v1/teacher/classrooms/" + classroomID.String() + "/private-talk"
}

// ---------------------------------------------------------------------------
// POST
// ---------------------------------------------------------------------------

// TestStartPrivateTalkResponseShapeAndIdentity pins the frozen contract and the §44/§25
// identity rules in one test: the teacher comes from the Principal (id, LOGIN session id and
// display name), the classroom from the path, and the student from the body — nothing else.
func TestStartPrivateTalkResponseShapeAndIdentity(t *testing.T) {
	h := newPrivateTalkHarness(t, nil)
	teacher, cookies, loginSessionID := h.teacherSession(t, "teacher-01")
	classroomID, studentID, sessionID := uuid.New(), uuid.New(), uuid.New()
	h.talk.startResult = &session.PrivateTalkView{Target: &session.PrivateTalkTarget{
		StudentID: studentID, SessionID: sessionID, DisplayName: "张三",
	}}

	rec := h.request(http.MethodPost, privateTalkPath(classroomID),
		`{"studentId":"`+studentID.String()+`"}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if h.talk.lastStart.TeacherID != teacher.ID {
		t.Errorf("service asked for teacher %s, want the session's %s", h.talk.lastStart.TeacherID, teacher.ID)
	}
	if h.talk.lastStart.TeacherSessionID != loginSessionID {
		t.Errorf("media identity = %s, want the login session %s (§44)", h.talk.lastStart.TeacherSessionID, loginSessionID)
	}
	if h.talk.lastStart.TeacherDisplayName != teacher.DisplayName {
		t.Errorf("display name = %q, want the authenticated one %q", h.talk.lastStart.TeacherDisplayName, teacher.DisplayName)
	}
	if h.talk.lastStart.ClassroomID != classroomID || h.talk.lastStart.StudentID != studentID {
		t.Errorf("service asked for %+v", h.talk.lastStart)
	}

	body := jsonBodyOf(t, rec)
	if len(body) != 1 {
		t.Fatalf("body = %v, want exactly the target member", body)
	}
	target, ok := body["target"].(map[string]any)
	if !ok {
		t.Fatalf("target = %#v, want an object", body["target"])
	}
	if len(target) != 3 {
		t.Fatalf("target = %v, want exactly studentId, displayName and sessionId", target)
	}
	if target["studentId"] != studentID.String() || target["displayName"] != "张三" || target["sessionId"] != sessionID.String() {
		t.Fatalf("target = %v", target)
	}
}

// TestStartPrivateTalkRejectsABodyWithoutAStudent: the one field is required, and a
// malformed one is a 400 that never reaches the service.
func TestStartPrivateTalkRejectsABodyWithoutAStudent(t *testing.T) {
	for name, body := range map[string]string{
		"missing":       `{}`,
		"empty":         `{"studentId":""}`,
		"not a uuid":    `{"studentId":"zhangsan"}`,
		"wrong type":    `{"studentId":12345}`,
		"not an object": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newPrivateTalkHarness(t, nil)
			_, cookies, _ := h.teacherSession(t, "teacher-01")

			rec := h.request(http.MethodPost, privateTalkPath(uuid.New()), body, cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
				t.Errorf("code = %s, want INVALID_REQUEST", code)
			}
			if len(h.talk.calls) != 0 {
				t.Errorf("the service was called with an unusable body: %v", h.talk.calls)
			}
		})
	}
}

// TestStartPrivateTalkErrorMapping pins the refusals of §31/§58: each one is a different
// thing the teacher has to do next.
func TestStartPrivateTalkErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   apperr.Code
	}{
		{"microphone not published", session.ErrTeacherMicRequired, http.StatusConflict, apperr.CodeTeacherMicRequired},
		{"student unavailable", session.ErrPrivateTalkUnavailable, http.StatusConflict, apperr.CodePrivateTalkUnavailable},
		{"classroom closed", session.ErrClassroomClosed, http.StatusConflict, apperr.CodeClassroomClosed},
		{"not the owner", classroom.ErrNotOwner, http.StatusForbidden, apperr.CodeClassroomNotOwner},
		{"unknown classroom", classroom.ErrNotFound, http.StatusNotFound, apperr.CodeClassroomNotFound},
		{"student not on the roster", classroom.ErrStudentNotAssigned, http.StatusNotFound, apperr.CodeStudentNotAssigned},
		{"media plane down", session.ErrMediaUnavailable, http.StatusBadGateway, apperr.CodeMediaTokenFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newPrivateTalkHarness(t, nil)
			_, cookies, _ := h.teacherSession(t, "teacher-01")
			h.talk.startErr = tc.err

			rec := h.request(http.MethodPost, privateTalkPath(uuid.New()),
				`{"studentId":"`+uuid.New().String()+`"}`, cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(tc.wantCode) {
				t.Errorf("code = %s, want %s", code, tc.wantCode)
			}
			// The internal cause must never be echoed.
			if strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("the response leaked the internal cause: %s", rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET
// ---------------------------------------------------------------------------

// TestShowPrivateTalkRendersIdleAsAnExplicitNull: the IDLE state of §31 has to be
// distinguishable from a missing field, because the console renders a button from it.
func TestShowPrivateTalkRendersIdleAsAnExplicitNull(t *testing.T) {
	h := newPrivateTalkHarness(t, nil)
	_, cookies, _ := h.teacherSession(t, "teacher-01")
	classroomID := uuid.New()
	h.talk.getResult = &session.PrivateTalkView{}

	rec := h.request(http.MethodGet, privateTalkPath(classroomID), "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"target":null}` {
		t.Fatalf("body = %s, want {\"target\":null}", got)
	}
	if h.talk.lastGetID != classroomID || h.talk.lastGetBy == uuid.Nil {
		t.Fatalf("service asked for %s by %s", h.talk.lastGetID, h.talk.lastGetBy)
	}
}

// TestShowPrivateTalkRendersTheTarget covers the other half of the contract.
func TestShowPrivateTalkRendersTheTarget(t *testing.T) {
	h := newPrivateTalkHarness(t, nil)
	_, cookies, _ := h.teacherSession(t, "teacher-01")
	studentID, sessionID := uuid.New(), uuid.New()
	h.talk.getResult = &session.PrivateTalkView{Target: &session.PrivateTalkTarget{
		StudentID: studentID, SessionID: sessionID, DisplayName: "李四",
	}}

	rec := h.request(http.MethodGet, privateTalkPath(uuid.New()), "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBodyOf(t, rec)
	target, ok := body["target"].(map[string]any)
	if !ok {
		t.Fatalf("target = %#v", body["target"])
	}
	if target["studentId"] != studentID.String() || target["sessionId"] != sessionID.String() || target["displayName"] != "李四" {
		t.Fatalf("target = %v", target)
	}
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

// TestStopPrivateTalkReturnsNoContent: 204 and an empty body, so no client depends on the
// shape of a response about an ended talk.
func TestStopPrivateTalkReturnsNoContent(t *testing.T) {
	h := newPrivateTalkHarness(t, nil)
	_, cookies, loginSessionID := h.teacherSession(t, "teacher-01")
	classroomID := uuid.New()

	rec := h.request(http.MethodDelete, privateTalkPath(classroomID), "", cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if h.talk.lastStop.ClassroomID != classroomID || h.talk.lastStop.TeacherSessionID != loginSessionID {
		t.Errorf("service asked for %+v", h.talk.lastStop)
	}
}

// TestStopPrivateTalkErrorMapping: the stop path maps the same authorization refusals as the
// start path — it is not a way to probe somebody else's classroom.
func TestStopPrivateTalkErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantStatus int
		wantCode   apperr.Code
	}{
		"not the owner": {classroom.ErrNotOwner, http.StatusForbidden, apperr.CodeClassroomNotOwner},
		"not found":     {classroom.ErrNotFound, http.StatusNotFound, apperr.CodeClassroomNotFound},
		"media is down": {session.ErrMediaUnavailable, http.StatusBadGateway, apperr.CodeMediaTokenFailed},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPrivateTalkHarness(t, nil)
			_, cookies, _ := h.teacherSession(t, "teacher-01")
			h.talk.stopErr = tc.err

			rec := h.request(http.MethodDelete, privateTalkPath(uuid.New()), "", cookies, csrf(cookies), "203.0.113.9:44444")
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
// Middleware chain
// ---------------------------------------------------------------------------

// privateTalkRoutes is every §31 route, with whether it writes.
func privateTalkRoutes() []struct {
	method string
	path   string
	body   string
	write  bool
} {
	base := "/api/v1/teacher/classrooms/" + uuid.New().String() + "/private-talk"
	return []struct {
		method string
		path   string
		body   string
		write  bool
	}{
		{http.MethodGet, base, "", false},
		{http.MethodPost, base, `{"studentId":"` + uuid.New().String() + `"}`, true},
		{http.MethodDelete, base, "", true},
	}
}

// TestPrivateTalkRoutesRequireSessionRoleAndCSRF is the §37/§63 test for the new routes: an
// anonymous caller is 401, a student session is 403 ROLE_FORBIDDEN, and a write without the
// CSRF token is 403 — with the service never reached in any of them.
func TestPrivateTalkRoutesRequireSessionRoleAndCSRF(t *testing.T) {
	for _, route := range privateTalkRoutes() {
		t.Run(route.method, func(t *testing.T) {
			anonymous := newPrivateTalkHarness(t, nil)
			rec := anonymous.request(route.method, route.path, route.body, nil, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeAuthRequired) {
				t.Errorf("anonymous: code = %s, want AUTH_REQUIRED", code)
			}
			if len(anonymous.talk.calls) != 0 {
				t.Errorf("anonymous: the service was called (%v)", anonymous.talk.calls)
			}

			// A student's session cannot satisfy the teacher entry, whatever it sends.
			studentHarness := newPrivateTalkHarness(t, nil)
			_, studentCookies := studentHarness.studentSession(t, "s10086")
			rec = studentHarness.request(route.method, route.path, route.body, studentCookies,
				csrf(studentCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("student: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("student: code = %s, want ROLE_FORBIDDEN", code)
			}
			if len(studentHarness.talk.calls) != 0 {
				t.Errorf("student: the service was called (%v)", studentHarness.talk.calls)
			}

			// Without the CSRF token a cross-site request could start or stop a talk.
			if !route.write {
				return
			}
			teacherHarness := newPrivateTalkHarness(t, nil)
			_, teacherCookies, _ := teacherHarness.teacherSession(t, "teacher-01")
			rec = teacherHarness.request(route.method, route.path, route.body, teacherCookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("without CSRF: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeCSRFInvalid) {
				t.Errorf("without CSRF: code = %s, want CSRF_INVALID", code)
			}
			if len(teacherHarness.talk.calls) != 0 {
				t.Errorf("without CSRF: the service was called (%v)", teacherHarness.talk.calls)
			}
		})
	}
}

// TestPrivateTalkRoutesAreNotRegisteredWithoutAService keeps the router honest: a
// deployment that cannot run the state machine answers 404 instead of exposing an endpoint
// with no authorization behind it.
func TestPrivateTalkRoutesAreNotRegisteredWithoutAService(t *testing.T) {
	h := newSessionHarness(t, nil, ratelimit.NewMemory())
	_, cookies, _ := h.teacherSession(t, "teacher-01")

	rec := h.request(http.MethodGet, privateTalkPath(uuid.New()), "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

// TestPrivateTalkResponseIsJSONWithTheFrozenContentType is a small contract check: the
// frontend parses these bodies, so an accidental HTML error page would be a crash.
func TestPrivateTalkResponseIsJSONWithTheFrozenContentType(t *testing.T) {
	h := newPrivateTalkHarness(t, nil)
	_, cookies, _ := h.teacherSession(t, "teacher-01")
	h.talk.getResult = &session.PrivateTalkView{}

	rec := h.request(http.MethodGet, privateTalkPath(uuid.New()), "", cookies, nil, "203.0.113.9:44444")
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content type = %q, want JSON", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
}
