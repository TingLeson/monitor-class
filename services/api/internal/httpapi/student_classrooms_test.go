package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
)

// The transport contract of the two student-portal endpoints (§14/§42/§70): which
// middleware chain they sit behind, which status and code a rejected request gets,
// and exactly what the JSON looks like. Those are the parts the student frontend is
// built against in parallel with this phase.
//
// The rules live in internal/classroom; the end-to-end version (real PostgreSQL,
// real sessions, real roster) is student_portal_integration_test.go.

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type studentClassroomHarness struct {
	*classroomHarness
}

func newStudentClassroomHarness(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter) *studentClassroomHarness {
	t.Helper()
	return &studentClassroomHarness{classroomHarness: newClassroomHarness(t, cfg, limiter)}
}

// studentClassroomRoutes is the whole student surface of Phase 4. It is a function
// (not a var) so every call gets fresh UUIDs and one test cannot pin a path another
// test depends on.
func studentClassroomRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/student/classrooms"},
		{http.MethodGet, "/api/v1/student/classrooms/" + uuid.New().String()},
	}
}

// sampleStudentClassroom builds one student-view row.
func sampleStudentClassroom(t *testing.T, status classroom.Status, withRun bool, withDescription bool) classroom.StudentClassroom {
	t.Helper()
	view := classroom.StudentClassroom{
		ID:                 uuid.New(),
		Name:               "C++ 算法训练",
		Status:             status,
		TeacherDisplayName: "王老师",
		CreatedAt:          time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
	}
	if withDescription {
		description := "每周三晚自习"
		view.Description = &description
	}
	if withRun {
		view.CurrentRun = &classroom.StudentCurrentRun{
			ID:       uuid.New(),
			OpenedAt: time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC),
		}
	}
	return view
}

// ---------------------------------------------------------------------------
// Middleware chain
// ---------------------------------------------------------------------------

// TestStudentClassroomRoutesRequireSessionAndRole is the §37 test for the group: an
// anonymous request is 401 AUTH_REQUIRED, a TEACHER or ADMIN session is 403
// ROLE_FORBIDDEN, and in every one of those cases the service is never reached.
func TestStudentClassroomRoutesRequireSessionAndRole(t *testing.T) {
	for _, route := range studentClassroomRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			anonymous := newStudentClassroomHarness(t, nil, nil)
			rec := anonymous.request(route.method, route.path, "", nil, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeAuthRequired) {
				t.Errorf("anonymous: code = %s, want AUTH_REQUIRED", code)
			}
			if len(anonymous.classrooms.calls) != 0 {
				t.Errorf("anonymous: the service was called (%v)", anonymous.classrooms.calls)
			}

			// A teacher is authenticated, so 403 — not 401, which would make the
			// teacher frontend clear a perfectly good session and bounce to a login
			// page that cannot help it.
			teacherHarness := newStudentClassroomHarness(t, nil, nil)
			_, teacherCookies := teacherHarness.teacherSession(t, "teacher-01")
			rec = teacherHarness.request(route.method, route.path, "", teacherCookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("teacher: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("teacher: code = %s, want ROLE_FORBIDDEN", code)
			}
			if len(teacherHarness.classrooms.calls) != 0 {
				t.Errorf("teacher: the service was called (%v)", teacherHarness.classrooms.calls)
			}

			// And neither is an administrator a super-student (§4).
			adminHarness := newStudentClassroomHarness(t, nil, nil)
			_, adminCookies := adminHarness.adminSession(t, "admin-01")
			rec = adminHarness.request(route.method, route.path, "", adminCookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("admin: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("admin: code = %s, want ROLE_FORBIDDEN", code)
			}
			if len(adminHarness.classrooms.calls) != 0 {
				t.Errorf("admin: the service was called (%v)", adminHarness.classrooms.calls)
			}

			// A student GET needs no CSRF token: the group has no unsafe route, so an
			// inline safe-method exemption is not doing the check's work here.
			studentHarness := newStudentClassroomHarness(t, nil, nil)
			_, studentCookies := studentHarness.studentSession(t, "s10086")
			authorized := sampleStudentClassroom(t, classroom.StatusOpen, true, true)
			studentHarness.classrooms.studentGetResult = &authorized
			rec = studentHarness.request(route.method, route.path, "", studentCookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusOK {
				t.Errorf("student: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/student/classrooms
// ---------------------------------------------------------------------------

// TestListStudentClassroomsShape pins the frozen response shape: `{"classrooms":[]}`
// with the student DTO's six members and nothing else.
func TestListStudentClassroomsShape(t *testing.T) {
	h := newStudentClassroomHarness(t, nil, nil)
	student, cookies := h.studentSession(t, "s10086")
	open := sampleStudentClassroom(t, classroom.StatusOpen, true, true)
	closed := sampleStudentClassroom(t, classroom.StatusClosed, false, false)
	h.classrooms.studentListResult = []classroom.StudentClassroom{open, closed}

	rec := h.request(http.MethodGet, "/api/v1/student/classrooms", "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// The id must come from the session, never from the request.
	if h.classrooms.lastStudentListUser != student.ID {
		t.Errorf("service asked for %s, want the session's student %s", h.classrooms.lastStudentListUser, student.ID)
	}

	body := jsonBodyOf(t, rec)
	raw, ok := body["classrooms"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("classrooms = %v, want the two the service returned", body["classrooms"])
	}
	first, _ := raw[0].(map[string]any)
	if first["id"] != open.ID.String() || first["name"] != "C++ 算法训练" || first["status"] != "OPEN" {
		t.Errorf("first classroom = %v", first)
	}
	if first["createdAt"] != "2026-03-01T08:00:00Z" {
		t.Errorf("createdAt = %v, want an RFC3339 timestamp", first["createdAt"])
	}
	description, ok := first["description"].(string)
	if !ok || description != *open.Description {
		t.Errorf("description = %v, want %q", first["description"], *open.Description)
	}
	teacher, ok := first["teacher"].(map[string]any)
	if !ok || teacher["displayName"] != open.TeacherDisplayName {
		t.Errorf("teacher = %v, want {displayName: %q}", first["teacher"], open.TeacherDisplayName)
	}
	run, ok := first["currentRun"].(map[string]any)
	if !ok {
		t.Fatalf("currentRun = %v, want an object for an OPEN classroom", first["currentRun"])
	}
	if run["id"] != open.CurrentRun.ID.String() || run["openedAt"] != "2026-03-04T19:00:00Z" {
		t.Errorf("currentRun = %v", run)
	}

	second, _ := raw[1].(map[string]any)
	if second["status"] != "CLOSED" {
		t.Errorf("a CLOSED classroom must still be returned, got %v", second)
	}
	if second["description"] != nil {
		t.Errorf("description = %v, want null when there is none (never \"\")", second["description"])
	}
	if second["currentRun"] != nil {
		t.Errorf("currentRun = %v, want null for a CLOSED classroom", second["currentRun"])
	}
	assertStudentDTOIsMinimal(t, rec)
}

// TestListStudentClassroomsEmptyIsAnArray: the empty portal renders an empty-state
// card, so the payload must be `[]` and not null.
func TestListStudentClassroomsEmptyIsAnArray(t *testing.T) {
	h := newStudentClassroomHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")

	rec := h.request(http.MethodGet, "/api/v1/student/classrooms", "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"classrooms":[]}` {
		t.Errorf("body = %s, want {\"classrooms\":[]}", got)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/student/classrooms/:id
// ---------------------------------------------------------------------------

// TestGetStudentClassroomInvalidUUIDIs400: a path parameter that is not an
// identifier is a malformed request, not a missing classroom — and it never reaches
// the service.
func TestGetStudentClassroomInvalidUUIDIs400(t *testing.T) {
	for _, bad := range []string{"not-a-uuid", "12345", "00000000-0000-0000-0000-00000000000"} {
		t.Run(bad, func(t *testing.T) {
			h := newStudentClassroomHarness(t, nil, nil)
			_, cookies := h.studentSession(t, "s10086")

			rec := h.request(http.MethodGet, "/api/v1/student/classrooms/"+bad, "", cookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
				t.Errorf("code = %s, want INVALID_REQUEST", code)
			}
			if len(h.classrooms.calls) != 0 {
				t.Errorf("the service was called for a malformed id (%v)", h.classrooms.calls)
			}
		})
	}
}

// TestGetStudentClassroomNotAssignedIs404 is the §14/§26 contract: the same 404
// STUDENT_NOT_ASSIGNED for "no such classroom" and for "not on its roster", so a
// student cannot probe ids to learn which lessons exist.
func TestGetStudentClassroomNotAssignedIs404(t *testing.T) {
	h := newStudentClassroomHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10087")
	h.classrooms.studentGetErr = classroom.ErrStudentNotAssigned

	rec := h.request(http.MethodGet, "/api/v1/student/classrooms/"+uuid.New().String(), "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeStudentNotAssigned) {
		t.Errorf("code = %s, want STUDENT_NOT_ASSIGNED", code)
	}
	// The message may not distinguish "no such classroom" from "not on its roster"
	// either: a different sentence would leak exactly what the shared code hides.
	if message := errorMessage(t, rec); message != apperr.DefaultMessage(apperr.CodeStudentNotAssigned) {
		t.Errorf("message = %q, want the code's own sentence", message)
	}
	// The body carries no classroom object at all — not even a redacted one.
	if strings.Contains(rec.Body.String(), "classroom\"") {
		t.Errorf("a rejected detail read must not include a classroom object: %s", rec.Body.String())
	}
}

// TestGetStudentClassroomShape pins `{"classroom":{...}}` and that a CLOSED
// classroom is a 200: §14 needs to render "暂不可进入", which requires the read to
// succeed.
func TestGetStudentClassroomShape(t *testing.T) {
	h := newStudentClassroomHarness(t, nil, nil)
	_, cookies := h.studentSession(t, "s10086")
	closed := sampleStudentClassroom(t, classroom.StatusClosed, false, true)
	h.classrooms.studentGetResult = &closed

	rec := h.request(http.MethodGet, "/api/v1/student/classrooms/"+closed.ID.String(), "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a CLOSED classroom (%s)", rec.Code, rec.Body.String())
	}
	if h.classrooms.lastStudentGetID != closed.ID {
		t.Errorf("service asked for %s, want %s", h.classrooms.lastStudentGetID, closed.ID)
	}

	body := jsonBodyOf(t, rec)
	obj, ok := body["classroom"].(map[string]any)
	if !ok {
		t.Fatalf("response has no classroom object: %s", rec.Body.String())
	}
	if obj["id"] != closed.ID.String() || obj["status"] != "CLOSED" {
		t.Errorf("classroom = %v", obj)
	}
	if obj["currentRun"] != nil {
		t.Errorf("currentRun = %v, want null", obj["currentRun"])
	}
	if _, wrapped := body["classrooms"]; wrapped {
		t.Error("the detail response must use `classroom`, not `classrooms`")
	}
	assertStudentDTOIsMinimal(t, rec)
}

// assertStudentDTOIsMinimal is the executable form of §26 for this API surface: no
// roster, no other student, and no media-plane detail may appear anywhere in a
// student response.
//
// It checks the raw bytes as well as the decoded object. A key that is present with
// a null value is still a published field — the frontend would see it, and the next
// backend change would fill it in — so the assertion is on the text of the body.
func assertStudentDTOIsMinimal(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, forbidden := range []string{
		"studentCount",   // §26: the number of participants is information about others
		"students",       // §26: no roster, ever
		"account",        // no account name of anyone
		"ownerTeacherId", // the student card shows a display name, not an id
		"livekitRoomName",
		"lk_", // the room-name prefix: a media-plane handle
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the student response contains %q, which §26/§33 forbid: %s", forbidden, body)
		}
	}

	// And positively: the DTO's own member set is closed. `students` being absent is
	// asserted above; this catches a renamed leak such as `participants`.
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var rows []map[string]any
	switch {
	case decoded["classrooms"] != nil:
		raw, _ := decoded["classrooms"].([]any)
		for _, entry := range raw {
			if obj, ok := entry.(map[string]any); ok {
				rows = append(rows, obj)
			}
		}
	case decoded["classroom"] != nil:
		if obj, ok := decoded["classroom"].(map[string]any); ok {
			rows = append(rows, obj)
		}
	default:
		t.Fatalf("response is neither a classroom list nor a classroom: %s", rec.Body.String())
	}
	allowed := map[string]bool{
		"id": true, "name": true, "description": true, "status": true,
		"teacher": true, "currentRun": true, "createdAt": true,
	}
	for _, row := range rows {
		for key := range row {
			if !allowed[key] {
				t.Errorf("the student DTO has a member the contract does not define: %q", key)
			}
		}
		if teacher, ok := row["teacher"].(map[string]any); ok {
			for key := range teacher {
				if key != "displayName" {
					t.Errorf("teacher object has an unexpected member: %q", key)
				}
			}
		}
		if run, ok := row["currentRun"].(map[string]any); ok {
			for key := range run {
				if key != "id" && key != "openedAt" {
					t.Errorf("currentRun object has an unexpected member: %q", key)
				}
			}
		}
	}
}
