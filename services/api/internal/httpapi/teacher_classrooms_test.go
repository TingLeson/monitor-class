package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// These tests run without PostgreSQL and pin the TRANSPORT contract of the eight
// teacher classroom endpoints: which middleware chain they sit behind, which status
// and code a rejected request gets, and exactly what the JSON looks like. Those are
// the parts a frontend is built against. The rules live in internal/classroom, and
// the end-to-end version is teacher_classrooms_integration_test.go.

// ---------------------------------------------------------------------------
// Fake classroom service
// ---------------------------------------------------------------------------

type fakeClassroom struct {
	listResult   []classroom.Classroom
	createResult *classroom.Classroom
	getResult    *classroom.Classroom
	updateResult *classroom.Classroom
	students     []classroom.Student
	addResult    *classroom.AddStudentsResult
	openResult   *classroom.Classroom
	openRun      *classroom.Run
	closeResult  *classroom.Classroom
	closeRun     *classroom.Run

	// The two student-portal reads of §14/§70. The returning student id is recorded
	// so the transport test can prove the handler passed the SESSION's id and not
	// something the client supplied.
	studentListResult   []classroom.StudentClassroom
	studentGetResult    *classroom.StudentClassroom
	studentListErr      error
	studentGetErr       error
	lastStudentListUser uuid.UUID
	lastStudentGetUser  uuid.UUID
	lastStudentGetID    uuid.UUID

	listErr     error
	createErr   error
	getErr      error
	updateErr   error
	removeErr   error
	openErr     error
	closeErr    error
	studentsErr error
	addErr      error

	lastListOwner uuid.UUID
	lastCreate    classroom.CreateInput
	lastGetID     uuid.UUID
	lastGetOwner  uuid.UUID
	lastUpdate    classroom.UpdateInput
	lastAdd       classroom.AddStudentsInput
	lastRemove    [2]uuid.UUID
	lastOpen      [2]uuid.UUID
	lastClose     [2]uuid.UUID

	calls []string
}

func (f *fakeClassroom) List(_ context.Context, teacherID uuid.UUID) ([]classroom.Classroom, error) {
	f.calls = append(f.calls, "list")
	f.lastListOwner = teacherID
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResult == nil {
		return []classroom.Classroom{}, nil
	}
	return f.listResult, nil
}

func (f *fakeClassroom) Create(_ context.Context, in classroom.CreateInput) (*classroom.Classroom, error) {
	f.calls = append(f.calls, "create")
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResult, nil
}

func (f *fakeClassroom) Get(_ context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error) {
	f.calls = append(f.calls, "get")
	f.lastGetID, f.lastGetOwner = classroomID, teacherID
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeClassroom) Update(_ context.Context, in classroom.UpdateInput) (*classroom.Classroom, error) {
	f.calls = append(f.calls, "update")
	f.lastUpdate = in
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	// Mirrors the service rule so the transport test can assert the 400 without a
	// real service behind it.
	if in.Name == nil && !in.DescriptionSet {
		return nil, classroom.InvalidRequestf("at least one of name or description must be provided")
	}
	return f.updateResult, nil
}

func (f *fakeClassroom) ListStudents(_ context.Context, classroomID, teacherID uuid.UUID) ([]classroom.Student, error) {
	f.calls = append(f.calls, "listStudents")
	f.lastGetID, f.lastGetOwner = classroomID, teacherID
	if f.studentsErr != nil {
		return nil, f.studentsErr
	}
	if f.students == nil {
		return []classroom.Student{}, nil
	}
	return f.students, nil
}

func (f *fakeClassroom) AddStudents(_ context.Context, in classroom.AddStudentsInput) (*classroom.AddStudentsResult, error) {
	f.calls = append(f.calls, "addStudents")
	f.lastAdd = in
	if f.addErr != nil {
		return nil, f.addErr
	}
	if f.addResult == nil {
		return &classroom.AddStudentsResult{Students: []classroom.Student{}, Rejected: []classroom.Rejection{}}, nil
	}
	return f.addResult, nil
}

func (f *fakeClassroom) RemoveStudent(_ context.Context, classroomID, studentID, teacherID uuid.UUID) error {
	f.calls = append(f.calls, "removeStudent")
	f.lastRemove = [2]uuid.UUID{studentID, teacherID}
	if f.removeErr != nil {
		return f.removeErr
	}
	return nil
}

func (f *fakeClassroom) Open(_ context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error) {
	f.calls = append(f.calls, "open")
	f.lastOpen = [2]uuid.UUID{classroomID, teacherID}
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	return f.openResult, f.openRun, nil
}

func (f *fakeClassroom) Close(_ context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error) {
	f.calls = append(f.calls, "close")
	f.lastClose = [2]uuid.UUID{classroomID, teacherID}
	if f.closeErr != nil {
		return nil, nil, f.closeErr
	}
	return f.closeResult, f.closeRun, nil
}

// ListStudentClassrooms is the student-portal list of §14.
func (f *fakeClassroom) ListStudentClassrooms(_ context.Context, studentID uuid.UUID) ([]classroom.StudentClassroom, error) {
	f.calls = append(f.calls, "listStudentClassrooms")
	f.lastStudentListUser = studentID
	if f.studentListErr != nil {
		return nil, f.studentListErr
	}
	if f.studentListResult == nil {
		return []classroom.StudentClassroom{}, nil
	}
	return f.studentListResult, nil
}

// GetStudentClassroom is the student-portal detail read.
func (f *fakeClassroom) GetStudentClassroom(_ context.Context, studentID, classroomID uuid.UUID) (*classroom.StudentClassroom, error) {
	f.calls = append(f.calls, "getStudentClassroom")
	f.lastStudentGetUser, f.lastStudentGetID = studentID, classroomID
	if f.studentGetErr != nil {
		return nil, f.studentGetErr
	}
	return f.studentGetResult, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------
type classroomHarness struct {
	*routerHarness
	classrooms *fakeClassroom
}

func newClassroomHarness(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter) *classroomHarness {
	t.Helper()
	if cfg == nil {
		cfg = authTestConfig(t)
	}
	fakeAuthSvc := newFakeAuth()
	fakeClassroomSvc := &fakeClassroom{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	router := NewRouter(Deps{
		Logger:    logger,
		Config:    cfg,
		Auth:      fakeAuthSvc,
		Classroom: fakeClassroomSvc,
		Limiter:   limiter,
	})
	return &classroomHarness{
		routerHarness: &routerHarness{router: router, auth: fakeAuthSvc, cfg: cfg, logs: logs},
		classrooms:    fakeClassroomSvc,
	}
}

// teacherSession registers an active teacher with a live session and returns the
// account and the two cookies its entry point issues.
func (h *classroomHarness) teacherSession(t *testing.T, account string) (*user.User, map[string]string) {
	t.Helper()
	u := h.staff(t, user.RoleTeacher, account, "a-teacher-passphrase")
	token := "teacher-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_teacher":      token,
		"classwatch_session_teacher_csrf": principal.CSRFToken,
	}
}

// studentSession registers a student with a live session on the student entry.
func (h *classroomHarness) studentSession(t *testing.T, account string) (*user.User, map[string]string) {
	t.Helper()
	u := h.student(t, account)
	token := "student-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_student":      token,
		"classwatch_session_student_csrf": principal.CSRFToken,
	}
}

// adminSession registers an administrator with a live session on the admin entry.
func (h *classroomHarness) adminSession(t *testing.T, account string) (*user.User, map[string]string) {
	t.Helper()
	u := h.staff(t, user.RoleAdmin, account, "an-admin-passphrase")
	token := "admin-token-" + account
	principal := h.auth.addSession(token, u)
	return u, map[string]string{
		"classwatch_session_admin":      token,
		"classwatch_session_admin_csrf": principal.CSRFToken,
	}
}

// sampleClassroom is a fully populated row for the fake service to return.
func sampleClassroom(t *testing.T, status classroom.Status, withRun bool) *classroom.Classroom {
	t.Helper()
	openedAt := time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC)
	description := "每周三晚自习"
	c := &classroom.Classroom{
		ID:             uuid.New(),
		Name:           "C++ 晚自习",
		Description:    &description,
		OwnerTeacherID: uuid.New(),
		Status:         status,
		CreatedAt:      time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC),
		StudentCount:   2,
	}
	if withRun {
		runID := uuid.New()
		c.CurrentRunID = &runID
		c.CurrentRun = &classroom.Run{
			ID:              runID,
			ClassroomID:     c.ID,
			Status:          classroom.StatusOpen,
			LiveKitRoomName: "lk_" + runID.String(),
			OpenedAt:        openedAt,
		}
	}
	return c
}

// classroomRoutes is every route of the teacher group, with whether it writes.
func classroomRoutes() []struct {
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
		{http.MethodGet, "/api/v1/teacher/classrooms", "", false},
		{http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"算法"}`, true},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + uuid.New().String(), "", false},
		{http.MethodPatch, "/api/v1/teacher/classrooms/" + uuid.New().String(), `{"name":"算法"}`, true},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/students", "", false},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/students", `{"accounts":["s10001"]}`, true},
		{http.MethodDelete, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/students/" + uuid.New().String(), "", true},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/open", "", true},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + uuid.New().String() + "/close", "", true},
	}
}

// ---------------------------------------------------------------------------
// The three middleware layers
// ---------------------------------------------------------------------------

// TestTeacherClassroomRoutesRequireAllThreeLayers is the §37/§63 test for the whole
// group: no session is 401, a student session is 403, an ADMIN session is 403 (§4 —
// an administrator is not a super-teacher), and a write without the CSRF token is
// 403. Nothing reaches the service in any of those cases.
func TestTeacherClassroomRoutesRequireAllThreeLayers(t *testing.T) {
	for _, route := range classroomRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			// 1. No session at all → 401.
			anonymous := newClassroomHarness(t, nil, nil)
			rec := anonymous.request(route.method, route.path, route.body, nil, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeAuthRequired) {
				t.Errorf("anonymous: code = %s, want AUTH_REQUIRED", code)
			}
			if len(anonymous.classrooms.calls) != 0 {
				t.Errorf("anonymous: the service was called (%v)", anonymous.classrooms.calls)
			}

			// 2. A student session → 403 ROLE_FORBIDDEN.
			studentHarness := newClassroomHarness(t, nil, nil)
			_, studentCookies := studentHarness.studentSession(t, "s10001")
			rec = studentHarness.request(route.method, route.path, route.body, studentCookies, csrf(studentCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("student: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("student: code = %s, want ROLE_FORBIDDEN", code)
			}

			// 3. An ADMIN session → 403 ROLE_FORBIDDEN. §4: the admin surface manages
			// accounts; it cannot open, close or edit somebody's classroom, and the
			// refusal happens before any ownership logic runs.
			adminHarness := newClassroomHarness(t, nil, nil)
			_, adminCookies := adminHarness.adminSession(t, "admin-01")
			rec = adminHarness.request(route.method, route.path, route.body, adminCookies, csrf(adminCookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Errorf("admin: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeRoleForbidden) {
				t.Errorf("admin: code = %s, want ROLE_FORBIDDEN", code)
			}
			if len(adminHarness.classrooms.calls) != 0 {
				t.Errorf("admin: the service was called (%v)", adminHarness.classrooms.calls)
			}

			// 4. Reads do not need CSRF; writes without it are 403 CSRF_INVALID.
			writer := newClassroomHarness(t, nil, nil)
			_, cookies := writer.teacherSession(t, "teacher-01")
			if !route.write {
				rec = writer.request(route.method, route.path, route.body, cookies, nil, "203.0.113.9:44444")
				if rec.Code == http.StatusForbidden && decodeErrorCode(t, rec) == string(apperr.CodeCSRFInvalid) {
					t.Error("a read must not require a CSRF token")
				}
				return
			}
			rec = writer.request(route.method, route.path, route.body, cookies, nil, "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("without CSRF: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeCSRFInvalid) {
				t.Errorf("without CSRF: code = %s, want CSRF_INVALID", code)
			}
			if len(writer.classrooms.calls) != 0 {
				t.Errorf("without CSRF: the service was called (%v)", writer.classrooms.calls)
			}
		})
	}
}

// TestTeacherClassroomRoutesRejectNonOwners walks every endpoint with a service
// that answers "not yours". §58 gives this its own code, so the frontend can say
// "only the teacher who created this classroom can do that" rather than "not found".
func TestTeacherClassroomRoutesRejectNonOwners(t *testing.T) {
	notOwner := classroom.ErrNotOwner
	for _, route := range classroomRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			if route.method == http.MethodPost && route.path == "/api/v1/teacher/classrooms" {
				// Creating a classroom has no resource to own yet — the owner is the
				// caller by construction, which the create test above pins instead.
				t.Skip("ownership does not apply to creating a new classroom")
			}
			h := newClassroomHarness(t, nil, nil)
			_, cookies := h.teacherSession(t, "teacher-02")
			h.classrooms.getErr = notOwner
			h.classrooms.listErr = notOwner
			h.classrooms.updateErr = notOwner
			h.classrooms.studentsErr = notOwner
			h.classrooms.addErr = notOwner
			h.classrooms.removeErr = notOwner
			h.classrooms.openErr = notOwner
			h.classrooms.closeErr = notOwner

			rec := h.request(route.method, route.path, route.body, cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != string(apperr.CodeClassroomNotOwner) {
				t.Fatalf("code = %s, want CLASSROOM_NOT_OWNER", code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Create / list / get
// ---------------------------------------------------------------------------

func TestCreateClassroomReturns201WithLocation(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	teacher, cookies := h.teacherSession(t, "teacher-01")
	h.classrooms.createResult = sampleClassroom(t, classroom.StatusClosed, false)

	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms",
		`{"name":"  C++ 晚自习 ","description":"  每周三  "}`, cookies, csrf(cookies), "203.0.113.9:44444")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/teacher/classrooms/"+h.classrooms.createResult.ID.String() {
		t.Errorf("Location = %q", got)
	}
	// The owner comes from the session, never from the body.
	if h.classrooms.lastCreate.TeacherID != teacher.ID {
		t.Errorf("owner = %s, want the session's teacher %s", h.classrooms.lastCreate.TeacherID, teacher.ID)
	}
	if h.classrooms.lastCreate.Name != "  C++ 晚自习 " {
		t.Errorf("the handler must pass the raw name to the service (which trims it), got %q", h.classrooms.lastCreate.Name)
	}

	body := jsonBodyOf(t, rec)
	classroomObj, ok := body["classroom"].(map[string]any)
	if !ok {
		t.Fatalf("response has no classroom object: %s", rec.Body.String())
	}
	for _, field := range []string{"id", "name", "description", "status", "ownerTeacherId", "studentCount", "currentRun", "createdAt", "updatedAt"} {
		if _, present := classroomObj[field]; !present {
			t.Errorf("classroom DTO is missing %q: %s", field, rec.Body.String())
		}
	}
	if classroomObj["currentRun"] != nil {
		t.Errorf("a new classroom must report currentRun = null: %v", classroomObj["currentRun"])
	}
	assertNoMediaMaterial(t, rec)
}

func TestCreateClassroomSurfacesValidationMessages(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")
	_, err := classroom.NormalizeName("")
	h.classrooms.createErr = err

	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms", `{"name":""}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
		t.Errorf("code = %s, want INVALID_REQUEST", code)
	}
	if message := errorMessage(t, rec); message != "name is required and must not be blank" {
		t.Errorf("message = %q, want the service's own sentence", message)
	}
}

func TestCreateClassroomRejectsUnknownFields(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")

	for _, body := range []string{
		`{"name":"算法","ownerTeacherId":"` + uuid.New().String() + `"}`,
		`{"name":"算法","status":"OPEN"}`,
		`{"name":"算法","currentRunId":null}`,
	} {
		rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms", body, cookies, csrf(cookies), "203.0.113.9:44444")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
			t.Errorf("%s: code = %s, want INVALID_REQUEST", body, code)
		}
	}
	if len(h.classrooms.calls) != 0 {
		t.Errorf("a rejected body reached the service (%v)", h.classrooms.calls)
	}
}

func TestListClassroomsEnvelope(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	teacher, cookies := h.teacherSession(t, "teacher-01")

	// Empty list: `[]`, never null.
	rec := h.request(http.MethodGet, "/api/v1/teacher/classrooms", "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"classrooms":[]}` {
		t.Errorf("empty list body = %s, want {\"classrooms\":[]}", got)
	}
	if h.classrooms.lastListOwner != teacher.ID {
		t.Errorf("list owner = %s, want the session's teacher", h.classrooms.lastListOwner)
	}

	// One open classroom: the run is embedded, the room name is not.
	h.classrooms.listResult = []classroom.Classroom{*sampleClassroom(t, classroom.StatusOpen, true)}
	rec = h.request(http.MethodGet, "/api/v1/teacher/classrooms", "", cookies, nil, "203.0.113.9:44444")
	body := jsonBodyOf(t, rec)
	list, ok := body["classrooms"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("classrooms = %v, want one entry", body["classrooms"])
	}
	entry := list[0].(map[string]any)
	run, ok := entry["currentRun"].(map[string]any)
	if !ok {
		t.Fatalf("currentRun = %v, want an object", entry["currentRun"])
	}
	if _, present := run["id"]; !present {
		t.Error("currentRun has no id")
	}
	if _, present := run["openedAt"]; !present {
		t.Error("currentRun has no openedAt")
	}
	if entry["studentCount"] != float64(2) {
		t.Errorf("studentCount = %v, want 2", entry["studentCount"])
	}
	assertNoMediaMaterial(t, rec)
}

func TestGetClassroomErrors(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{"not found", "/api/v1/teacher/classrooms/" + uuid.New().String(), classroom.ErrNotFound, http.StatusNotFound, "CLASSROOM_NOT_FOUND"},
		{"not owner", "/api/v1/teacher/classrooms/" + uuid.New().String(), classroom.ErrNotOwner, http.StatusForbidden, "CLASSROOM_NOT_OWNER"},
		{"bad uuid", "/api/v1/teacher/classrooms/not-a-uuid", nil, http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newClassroomHarness(t, nil, nil)
			_, cookies := h.teacherSession(t, "teacher-01")
			h.classrooms.getErr = tc.serviceErr
			h.classrooms.getResult = sampleClassroom(t, classroom.StatusClosed, false)

			rec := h.request(http.MethodGet, tc.path, "", cookies, nil, "203.0.113.9:44444")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != tc.wantCode {
				t.Fatalf("code = %s, want %s", code, tc.wantCode)
			}
			if tc.wantCode == "INVALID_REQUEST" && len(h.classrooms.calls) != 0 {
				t.Errorf("a malformed id reached the service (%v)", h.classrooms.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func TestUpdateClassroomAcceptsEachFieldIndependently(t *testing.T) {
	name := "算法（进阶）"

	tests := []struct {
		name            string
		body            string
		wantName        *string
		wantDescription *string
		wantDescSet     bool
	}{
		{"name only", `{"name":"算法（进阶）"}`, &name, nil, false},
		{"description only", `{"description":"新说明"}`, nil, ptrOf("新说明"), true},
		{"both", `{"name":"算法（进阶）","description":"新说明"}`, &name, ptrOf("新说明"), true},
		{"explicit null clears", `{"description":null}`, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newClassroomHarness(t, nil, nil)
			_, cookies := h.teacherSession(t, "teacher-01")
			h.classrooms.updateResult = sampleClassroom(t, classroom.StatusClosed, false)

			rec := h.request(http.MethodPatch, "/api/v1/teacher/classrooms/"+uuid.New().String(),
				tc.body, cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			in := h.classrooms.lastUpdate
			if (in.Name == nil) != (tc.wantName == nil) || (in.Name != nil && *in.Name != *tc.wantName) {
				t.Errorf("name = %v, want %v", in.Name, tc.wantName)
			}
			if in.DescriptionSet != tc.wantDescSet {
				t.Errorf("descriptionSet = %v, want %v", in.DescriptionSet, tc.wantDescSet)
			}
			if (in.Description == nil) != (tc.wantDescription == nil) || (in.Description != nil && *in.Description != *tc.wantDescription) {
				t.Errorf("description = %v, want %v", in.Description, tc.wantDescription)
			}
		})
	}
}

func TestUpdateClassroomRejectsForbiddenAndUnknownFields(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")
	id := uuid.New().String()

	for _, body := range []string{
		`{"status":"OPEN"}`, // the state machine has its own endpoints
		`{"currentRunId":"` + uuid.New().String() + `"}`,
		`{"ownerTeacherId":"` + uuid.New().String() + `"}`,
		`{"name":"x","unknownField":1}`,
		`{}`, // no field at all
	} {
		rec := h.request(http.MethodPatch, "/api/v1/teacher/classrooms/"+id, body, cookies, csrf(cookies), "203.0.113.9:44444")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
			continue
		}
		if code := decodeErrorCode(t, rec); code != string(apperr.CodeInvalidRequest) {
			t.Errorf("%s: code = %s, want INVALID_REQUEST", body, code)
		}
	}
}

func TestUpdateClassroomRejectsNullName(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")

	rec := h.request(http.MethodPatch, "/api/v1/teacher/classrooms/"+uuid.New().String(),
		`{"name":null}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(h.classrooms.calls) != 0 {
		t.Errorf("a null name reached the service (%v)", h.classrooms.calls)
	}
}

// ---------------------------------------------------------------------------
// Roster
// ---------------------------------------------------------------------------

func TestListStudentsEnvelope(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")

	rec := h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/students", "", cookies, nil, "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"students":[]}` {
		t.Errorf("empty roster body = %s, want {\"students\":[]}", got)
	}

	h.classrooms.students = []classroom.Student{{
		ID: uuid.New(), Account: "s10001", DisplayName: "张三",
		Status: user.StatusDisabled, AddedAt: time.Date(2026, 3, 4, 19, 5, 0, 0, time.UTC),
	}}
	rec = h.request(http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/students", "", cookies, nil, "203.0.113.9:44444")
	list, ok := jsonBodyOf(t, rec)["students"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("students = %v, want one entry", jsonBodyOf(t, rec)["students"])
	}
	entry := list[0].(map[string]any)
	if entry["status"] != string(user.StatusDisabled) {
		t.Errorf("status = %v, want DISABLED (a disabled student stays on the roster)", entry["status"])
	}
	for _, field := range []string{"id", "account", "displayName", "status", "addedAt"} {
		if _, present := entry[field]; !present {
			t.Errorf("student DTO is missing %q", field)
		}
	}
}

func TestAddStudentsPartialSuccessShape(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	teacher, cookies := h.teacherSession(t, "teacher-01")
	classroomID := uuid.New()
	h.classrooms.addResult = &classroom.AddStudentsResult{
		Students: []classroom.Student{{
			ID: uuid.New(), Account: "s10001", DisplayName: "张三",
			Status: user.StatusActive, AddedAt: time.Date(2026, 3, 4, 19, 5, 0, 0, time.UTC),
		}},
		Rejected: []classroom.Rejection{
			{Account: "s99999", Code: apperr.CodeStudentNotFound},
			{Account: "T9001", Code: apperr.CodeNotAStudent},
		},
	}

	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students",
		`{"accounts":["s10001","s99999","T9001"]}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBodyOf(t, rec)
	students, ok := body["students"].([]any)
	if !ok || len(students) != 1 {
		t.Fatalf("students = %v, want the full roster after the call", body["students"])
	}
	rejected, ok := body["rejected"].([]any)
	if !ok || len(rejected) != 2 {
		t.Fatalf("rejected = %v, want two entries", body["rejected"])
	}
	first := rejected[0].(map[string]any)
	if first["account"] != "s99999" || first["code"] != string(apperr.CodeStudentNotFound) {
		t.Errorf("rejected[0] = %v, want {s99999, STUDENT_NOT_FOUND}", first)
	}
	if h.classrooms.lastAdd.TeacherID != teacher.ID || h.classrooms.lastAdd.ClassroomID != classroomID {
		t.Errorf("service saw %+v", h.classrooms.lastAdd)
	}
	if len(h.classrooms.lastAdd.Accounts) != 3 {
		t.Errorf("accounts = %v, want all three passed through", h.classrooms.lastAdd.Accounts)
	}
}

func TestAddStudentsEmptyResultIsAnArray(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")

	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/students",
		`{"accounts":[]}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"rejected":[],"students":[]}` {
		t.Errorf("body = %s, want arrays and not nulls", got)
	}
}

func TestAddStudentsSurfacesEmptyListRejection(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")
	h.classrooms.addErr = classroom.InvalidRequestf("accounts must contain at least one account")

	rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/students",
		`{"accounts":[]}`, cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if message := errorMessage(t, rec); message != "accounts must contain at least one account" {
		t.Errorf("message = %q", message)
	}
}

func TestRemoveStudent(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	teacher, cookies := h.teacherSession(t, "teacher-01")
	classroomID, studentID := uuid.New(), uuid.New()

	rec := h.request(http.MethodDelete, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students/"+studentID.String(),
		"", cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 must have no body, got %q", rec.Body.String())
	}
	if h.classrooms.lastRemove != [2]uuid.UUID{studentID, teacher.ID} {
		t.Errorf("service saw %v, want {student, teacher}", h.classrooms.lastRemove)
	}

	// Not on the roster → 404 STUDENT_NOT_ASSIGNED (§58).
	h.classrooms.removeErr = classroom.ErrStudentNotAssigned
	rec = h.request(http.MethodDelete, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students/"+studentID.String(),
		"", cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != string(apperr.CodeStudentNotAssigned) {
		t.Errorf("code = %s, want STUDENT_NOT_ASSIGNED", code)
	}

	// A malformed student id → 400 and no service call.
	before := len(h.classrooms.calls)
	rec = h.request(http.MethodDelete, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students/nope",
		"", cookies, csrf(cookies), "203.0.113.9:44444")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad student id: status = %d, want 400", rec.Code)
	}
	if len(h.classrooms.calls) != before {
		t.Error("a malformed student id reached the service")
	}
}

// ---------------------------------------------------------------------------
// Open / close
// ---------------------------------------------------------------------------

func TestOpenAndCloseResponses(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		h := newClassroomHarness(t, nil, nil)
		teacher, cookies := h.teacherSession(t, "teacher-01")
		opened := sampleClassroom(t, classroom.StatusOpen, true)
		run := *opened.CurrentRun
		h.classrooms.openResult = opened
		h.classrooms.openRun = &run

		rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+opened.ID.String()+"/open",
			"", cookies, csrf(cookies), "203.0.113.9:44444")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		body := jsonBodyOf(t, rec)
		classroomObj, ok := body["classroom"].(map[string]any)
		if !ok {
			t.Fatalf("no classroom object: %s", rec.Body.String())
		}
		if classroomObj["status"] != "OPEN" {
			t.Errorf("classroom.status = %v, want OPEN", classroomObj["status"])
		}
		runObj, ok := body["run"].(map[string]any)
		if !ok {
			t.Fatalf("no run object: %s", rec.Body.String())
		}
		for _, field := range []string{"id", "classroomId", "status", "openedAt", "closedAt"} {
			if _, present := runObj[field]; !present {
				t.Errorf("run DTO is missing %q", field)
			}
		}
		if runObj["status"] != "OPEN" || runObj["closedAt"] != nil {
			t.Errorf("run = %v, want OPEN with closedAt null", runObj)
		}
		if h.classrooms.lastOpen != [2]uuid.UUID{opened.ID, teacher.ID} {
			t.Errorf("service saw %v, want {classroom, teacher}", h.classrooms.lastOpen)
		}
		assertNoMediaMaterial(t, rec)
	})

	t.Run("close", func(t *testing.T) {
		h := newClassroomHarness(t, nil, nil)
		_, cookies := h.teacherSession(t, "teacher-01")
		closed := sampleClassroom(t, classroom.StatusClosed, false)
		closedAt := time.Date(2026, 3, 4, 21, 5, 0, 0, time.UTC)
		run := classroom.Run{
			ID: uuid.New(), ClassroomID: closed.ID, Status: classroom.StatusClosed,
			LiveKitRoomName: "lk_" + uuid.New().String(),
			OpenedAt:        closedAt.Add(-2 * time.Hour), ClosedAt: &closedAt,
		}
		h.classrooms.closeResult = closed
		h.classrooms.closeRun = &run

		rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+closed.ID.String()+"/close",
			"", cookies, csrf(cookies), "203.0.113.9:44444")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		body := jsonBodyOf(t, rec)
		classroomObj := body["classroom"].(map[string]any)
		if classroomObj["status"] != "CLOSED" || classroomObj["currentRun"] != nil {
			t.Errorf("classroom = %v, want CLOSED with currentRun null", classroomObj)
		}
		runObj := body["run"].(map[string]any)
		if runObj["status"] != "CLOSED" || runObj["closedAt"] == nil {
			t.Errorf("run = %v, want CLOSED with a closedAt (§49)", runObj)
		}
		assertNoMediaMaterial(t, rec)
	})
}

func TestOpenAndCloseConflicts(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		serviceErr error
		wantCode   string
	}{
		{"open an open classroom", "open", classroom.ErrAlreadyOpen, "CLASSROOM_ALREADY_OPEN"},
		{"close a closed classroom", "close", classroom.ErrAlreadyClosed, "CLASSROOM_ALREADY_CLOSED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newClassroomHarness(t, nil, nil)
			_, cookies := h.teacherSession(t, "teacher-01")
			h.classrooms.openErr = tc.serviceErr
			h.classrooms.closeErr = tc.serviceErr

			rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/"+uuid.New().String()+"/"+tc.path,
				"", cookies, csrf(cookies), "203.0.113.9:44444")
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != tc.wantCode {
				t.Fatalf("code = %s, want %s", code, tc.wantCode)
			}
		})
	}
}

func TestOpenAndCloseRequireAValidUUID(t *testing.T) {
	h := newClassroomHarness(t, nil, nil)
	_, cookies := h.teacherSession(t, "teacher-01")

	for _, path := range []string{"open", "close"} {
		rec := h.request(http.MethodPost, "/api/v1/teacher/classrooms/12/open",
			"", cookies, csrf(cookies), "203.0.113.9:44444")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
	if len(h.classrooms.calls) != 0 {
		t.Errorf("a malformed id reached the service (%v)", h.classrooms.calls)
	}
}

// ---------------------------------------------------------------------------
// Contract guards
// ---------------------------------------------------------------------------

// assertNoMediaMaterial fails when a response carries the LiveKit room name or the
// raw run id field. The room name is a media-plane detail (§8/§33) and Phase 6 will
// hand it out with the media token; leaking it in every classroom response would
// put it in browser history and screenshots for no benefit.
func assertNoMediaMaterial(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, forbidden := range []string{"livekitRoomName", "livekit_room_name", "lk_", "currentRunId"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response contains %q: %s", forbidden, body)
		}
	}
}

// jsonBodyOf decodes a response into a generic map.
func jsonBodyOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

func ptrOf[T any](value T) *T { return &value }
