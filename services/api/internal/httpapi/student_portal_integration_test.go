package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The student portal end to end (§14/§26/§70): the real router, the real middleware
// chain, real sessions in PostgreSQL, the real classroom service and its SQL.
//
// This is the layer the manual walkthrough of the phase exercises with curl, in the
// form that runs in CI: student login → empty list → teacher creates a classroom and
// grants access → the classroom appears CLOSED → teacher opens it → OPEN and first →
// an unauthorized student is refused with the same code an unknown id gets → teacher
// and admin sessions are refused by the group middleware.

// studentCall performs a session-authenticated student request.
//
// No CSRF header is passed and none is expected: the student group of Phase 4 is
// read-only, so requiring a token here would be a bug the frontend would have to work
// around rather than a protection.
func (e *classroomE2E) studentCall(t *testing.T, method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return e.call(t, method, path, "", cookies, nil)
}

// studentClassroomsOf decodes the student list response.
func studentClassroomsOf(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	body := jsonBody(t, rec)
	raw, ok := body["classrooms"].([]any)
	if !ok {
		t.Fatalf("response has no classrooms array: %s", rec.Body.String())
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("classrooms entry is not an object: %v", entry)
		}
		out = append(out, obj)
	}
	return out
}

// studentClassroomOf decodes the student detail response.
func studentClassroomOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	obj, ok := jsonBody(t, rec)["classroom"].(map[string]any)
	if !ok {
		t.Fatalf("response has no classroom object: %s", rec.Body.String())
	}
	return obj
}

// TestStudentPortalEndToEnd is the §70 acceptance path.
func TestStudentPortalEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)

	// --- the two students of §2.2 and the teacher who owns the classroom ---
	studentA := e.student(t)
	studentB := e.student(t)
	teacher, teacherCookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")

	// --- student A logs in with the account alone (no password exists) ---
	studentACookies := e.login(t, "student", studentA.Account, "")
	studentBCookies := e.login(t, "student", studentB.Account, "")

	// A student session cannot be used on the teacher entry and vice versa: the
	// entries are separate cookie namespaces (§5/§37).
	if rec := e.call(t, http.MethodGet, "/api/v1/teacher/classrooms", "", studentACookies, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a student session on the teacher entry: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// --- the portal starts empty: no grants, no classrooms ---
	rec := e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentACookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial list: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"classrooms":[]}` {
		t.Fatalf("initial list = %s, want an empty array", got)
	}

	// --- teacher creates the classroom and grants access to A only ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms",
		`{"name":"C++ 算法训练","description":"每周三晚自习"}`, teacherCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", rec.Code, rec.Body.String())
	}
	classroomID, err := uuid.Parse(classroomOf(t, rec)["id"].(string))
	if err != nil {
		t.Fatalf("classroom id is not a UUID: %v", err)
	}
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students",
		`{"accounts":["`+studentA.Account+`"]}`, teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("add students: status = %d (%s)", rec.Code, rec.Body.String())
	}

	// --- A now sees it, CLOSED, and the detail read succeeds ---
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentACookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after grant: status = %d (%s)", rec.Code, rec.Body.String())
	}
	listed := studentClassroomsOf(t, rec)
	if len(listed) != 1 {
		t.Fatalf("list after grant = %v, want exactly the one classroom", listed)
	}
	if listed[0]["id"] != classroomID.String() {
		t.Fatalf("listed id = %v, want %s", listed[0]["id"], classroomID)
	}
	if listed[0]["status"] != "CLOSED" {
		t.Errorf("status = %v, want CLOSED (a classroom is created closed, §7)", listed[0]["status"])
	}
	if listed[0]["currentRun"] != nil {
		t.Errorf("currentRun = %v, want null before the first open", listed[0]["currentRun"])
	}
	if teacherObj, ok := listed[0]["teacher"].(map[string]any); !ok || teacherObj["displayName"] != teacher.DisplayName {
		t.Errorf("teacher = %v, want %q", listed[0]["teacher"], teacher.DisplayName)
	}
	if listed[0]["description"] != "每周三晚自习" {
		t.Errorf("description = %v", listed[0]["description"])
	}

	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms/"+classroomID.String(), studentACookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail while CLOSED: status = %d, want 200 so the portal can render 暂不可进入 (%s)", rec.Code, rec.Body.String())
	}
	detail := studentClassroomOf(t, rec)
	if detail["status"] != "CLOSED" || detail["currentRun"] != nil {
		t.Errorf("detail while CLOSED = %v", detail)
	}
	assertNoForbiddenStudentFields(t, rec, classroomID, teacher, studentA, studentB)

	// --- teacher opens the classroom ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open: status = %d (%s)", rec.Code, rec.Body.String())
	}
	runID, err := uuid.Parse(runOf(t, rec)["id"].(string))
	if err != nil {
		t.Fatalf("run id is not a UUID: %v", err)
	}

	// --- the student sees OPEN first, with the current run ---
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentACookies)
	listed = studentClassroomsOf(t, rec)
	if len(listed) != 1 {
		t.Fatalf("list after open = %v", listed)
	}
	if listed[0]["status"] != "OPEN" {
		t.Errorf("status = %v, want OPEN", listed[0]["status"])
	}
	run, ok := listed[0]["currentRun"].(map[string]any)
	if !ok {
		t.Fatalf("currentRun = %v, want the run that was just created", listed[0]["currentRun"])
	}
	if run["id"] != runID.String() {
		t.Errorf("currentRun.id = %v, want %s", run["id"], runID)
	}
	openedAt, _ := run["openedAt"].(string)
	parsed, err := time.Parse(time.RFC3339, openedAt)
	if err != nil {
		t.Fatalf("currentRun.openedAt = %q is not RFC3339: %v", openedAt, err)
	}
	if time.Since(parsed) > 5*time.Minute || time.Until(parsed) > time.Minute {
		t.Errorf("currentRun.openedAt = %s, want roughly now", parsed)
	}

	// A SECOND classroom, granted later and left CLOSED, must sort BELOW the open one:
	// that is the §14 ordering ("已开启" on top) rather than insertion order.
	second := e.createClassroomFor(t, teacherCookies, "数据结构练习")
	e.grantStudent(t, teacherCookies, second, studentA.Account)
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentACookies)
	listed = studentClassroomsOf(t, rec)
	if len(listed) != 2 {
		t.Fatalf("list = %v, want both classrooms", listed)
	}
	if listed[0]["id"] != classroomID.String() || listed[0]["status"] != "OPEN" {
		t.Errorf("first entry = %v, want the OPEN classroom even though the CLOSED one is newer", listed[0])
	}
	if listed[1]["id"] != second.String() || listed[1]["status"] != "CLOSED" {
		t.Errorf("second entry = %v, want the CLOSED classroom (still listed, §14)", listed[1])
	}

	// --- student B was never granted access ---
	for _, target := range []string{classroomID.String(), second.String()} {
		rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms/"+target, studentBCookies)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("B reading %s: status = %d, want 404 (%s)", target, rec.Code, rec.Body.String())
		}
		if code := errorCodeOf(t, rec); code != string(apperr.CodeStudentNotAssigned) {
			t.Errorf("B reading %s: code = %s, want STUDENT_NOT_ASSIGNED", target, code)
		}
	}
	// The unknown-id case answers identically: a student cannot use the portal to
	// find out which classrooms exist in the school.
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms/"+uuid.New().String(), studentBCookies)
	if rec.Code != http.StatusNotFound || errorCodeOf(t, rec) != string(apperr.CodeStudentNotAssigned) {
		t.Errorf("an unknown id: status = %d, code = %s, want 404 STUDENT_NOT_ASSIGNED", rec.Code, errorCodeOf(t, rec))
	}
	// And B's own list is that same empty array — no name, no id of anyone else.
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentBCookies)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"classrooms":[]}` {
		t.Errorf("B's list = %s, want an empty array", got)
	}

	// --- a malformed id is a bad request, not a missing classroom ---
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms/not-a-uuid", studentACookies)
	if rec.Code != http.StatusBadRequest || errorCodeOf(t, rec) != string(apperr.CodeInvalidRequest) {
		t.Errorf("malformed id: status = %d, code = %s, want 400 INVALID_REQUEST", rec.Code, errorCodeOf(t, rec))
	}

	// --- teacher and admin sessions are refused on the student entry (§4/§37) ---
	for _, entry := range []string{"teacher", "admin"} {
		var cookies []*http.Cookie
		if entry == "teacher" {
			cookies = teacherCookies
		} else {
			_, cookies = e.staff(t, user.RoleAdmin, "an-admin-passphrase")
		}
		for _, path := range []string{"/api/v1/student/classrooms", "/api/v1/student/classrooms/" + classroomID.String()} {
			rec = e.call(t, http.MethodGet, path, "", cookies, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s session on %s: status = %d, want 403 (%s)", entry, path, rec.Code, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("%s session on %s: code = %s, want ROLE_FORBIDDEN", entry, path, code)
			}
		}
	}

	// --- and without any session at all ---
	rec = e.call(t, http.MethodGet, "/api/v1/student/classrooms", "", nil, nil)
	if rec.Code != http.StatusUnauthorized || errorCodeOf(t, rec) != string(apperr.CodeAuthRequired) {
		t.Errorf("anonymous: status = %d, code = %s, want 401 AUTH_REQUIRED", rec.Code, errorCodeOf(t, rec))
	}

	// --- closing the classroom puts it back to CLOSED without removing the card ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/close", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("close: status = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = e.studentCall(t, http.MethodGet, "/api/v1/student/classrooms", studentACookies)
	listed = studentClassroomsOf(t, rec)
	if len(listed) != 2 {
		t.Fatalf("list after close = %v, want both classrooms still listed", listed)
	}
	for _, entry := range listed {
		if entry["status"] != "CLOSED" || entry["currentRun"] != nil {
			t.Errorf("after close: %v, want CLOSED with no current run", entry)
		}
	}
}

// TestStudentPortalNoSessionMeans401 checks the middleware chain before any tenant
// data could be touched, for both routes of the group.
func TestStudentPortalNoSessionMeans401(t *testing.T) {
	e := newClassroomE2E(t)
	for _, path := range []string{"/api/v1/student/classrooms", "/api/v1/student/classrooms/" + uuid.New().String()} {
		rec := e.call(t, http.MethodGet, path, "", nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 (%s)", path, rec.Code, rec.Body.String())
		}
		if code := errorCodeOf(t, rec); code != string(apperr.CodeAuthRequired) {
			t.Errorf("%s: code = %s, want AUTH_REQUIRED", path, code)
		}
	}
}

// createClassroomFor creates a classroom through the teacher API and returns its id.
func (e *classroomE2E) createClassroomFor(t *testing.T, teacherCookies []*http.Cookie, name string) uuid.UUID {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"`+name+`"}`, teacherCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q: status = %d (%s)", name, rec.Code, rec.Body.String())
	}
	id, err := uuid.Parse(classroomOf(t, rec)["id"].(string))
	if err != nil {
		t.Fatalf("classroom id is not a UUID: %v", err)
	}
	return id
}

// grantStudent adds one account to a classroom's roster through the teacher API.
func (e *classroomE2E) grantStudent(t *testing.T, teacherCookies []*http.Cookie, classroomID uuid.UUID, account string) {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students",
		`{"accounts":["`+account+`"]}`, teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant %s: status = %d (%s)", account, rec.Code, rec.Body.String())
	}
}

// errorCodeOf returns error.code from the standard envelope.
func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard error envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

// assertNoForbiddenStudentFields is §26 over the wire: whatever the handler happens
// to build, a student response may not name another student, count the roster, or
// carry a media-plane handle.
//
// `owner` is the teacher whose display name the card legitimately shows; `others`
// are the accounts that must not appear in any form.
func assertNoForbiddenStudentFields(
	t *testing.T,
	rec *httptest.ResponseRecorder,
	classroomID uuid.UUID,
	owner *user.User,
	others ...*user.User,
) {
	t.Helper()
	body := rec.Body.String()
	// Positive control first: the assertions below are only meaningful if the
	// response really is this classroom's.
	if !strings.Contains(body, classroomID.String()) {
		t.Fatalf("the response does not contain the classroom id; nothing was asserted: %s", body)
	}
	forbidden := []string{"studentCount", "livekitRoomName", "lk_"}
	for _, other := range others {
		forbidden = append(forbidden, other.ID.String(), other.Account, other.DisplayName)
	}
	for _, value := range forbidden {
		if strings.Contains(body, value) {
			t.Errorf("the student response contains %q, which §26/§33 forbid: %s", value, body)
		}
	}
	// The owner's account must not travel either: the card shows a display name. Only
	// the name is expected, and the response is checked to contain it so this is not
	// a vacuous assertion.
	if !strings.Contains(body, owner.DisplayName) {
		t.Errorf("the response does not carry the teacher's display name: %s", body)
	}
	if strings.Contains(body, owner.Account) {
		t.Errorf("the student response contains the teacher's account name: %s", body)
	}
}
