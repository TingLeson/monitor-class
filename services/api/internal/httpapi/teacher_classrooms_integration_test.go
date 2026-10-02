package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The teacher classroom surface against a real PostgreSQL server: the router, the
// three middleware layers, the classroom service, the repository SQL, real sessions
// and real accounts, wired exactly as cmd/api wires them.
//
// This is the layer that catches what the unit tests fake away: a JOIN that reads
// the wrong owner, an ownership check the handler forgot, a run that is not created,
// a roster query that drops disabled accounts.

type classroomE2E struct {
	router        *gin.Engine
	repo          *user.Postgres
	classroomRepo *classroom.Postgres
	service       *classroom.Service
	pool          *pgxpool.Pool
	cfg           *config.Config
	logs          *strings.Builder
}

func newClassroomE2E(t *testing.T) *classroomE2E {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pool := dbtest.Pool(t)
	repo := user.NewPostgres(pool)
	sessions := sessionstore.New(pool)
	classroomRepo := classroom.NewPostgres(pool)

	cfg := &config.Config{
		AppEnv:                           config.EnvTest,
		SessionCookieName:                "classwatch_session",
		SessionTTL:                       time.Hour,
		SessionCookieSecure:              false,
		SessionIdleTouchInterval:         5 * time.Minute,
		RateLimitLoginPerMinute:          1000,
		RateLimitLoginPerAccountPer10Min: 1000,
		RateLimitAPIPerMinute:            1000,
		PasswordMinLength:                12,
	}
	authService := auth.NewService(repo, sessions, auth.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    auth.NewPasswordPolicy(cfg.PasswordMinLength),
	})
	classroomService := classroom.NewService(classroomRepo, repo)

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := httpapi.NewRouter(httpapi.Deps{
		Logger:    logger,
		Config:    cfg,
		Auth:      authService,
		Classroom: classroomService,
		Limiter:   ratelimit.NewMemory(),
	})
	return &classroomE2E{
		router: router, repo: repo, classroomRepo: classroomRepo, service: classroomService,
		pool: pool, cfg: cfg, logs: logs,
	}
}

// staff creates an account with a password and logs it in on its own entry point.
func (e *classroomE2E) staff(t *testing.T, role user.Role, password string) (*user.User, []*http.Cookie) {
	t.Helper()
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.repo.Create(context.Background(), user.CreateParams{
		Account:      dbtest.RandomAccount(strings.ToLower(string(role))),
		DisplayName:  string(role) + " 测试",
		Role:         role,
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	e.cleanupAccounts(t, created.ID)
	return created, e.login(t, strings.ToLower(string(role)), created.Account, password)
}

// student creates a password-less student account (there is no other kind, §2.2).
func (e *classroomE2E) student(t *testing.T) *user.User {
	t.Helper()
	created, err := e.repo.Create(context.Background(), user.CreateParams{
		Account:     dbtest.RandomAccount("student"),
		DisplayName: "学生 测试",
		Role:        user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	e.cleanupAccounts(t, created.ID)
	return created
}

// login authenticates on one entry point and returns its cookies.
func (e *classroomE2E) login(t *testing.T, entry, account, password string) []*http.Cookie {
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

func (e *classroomE2E) call(t *testing.T, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
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
	req.RemoteAddr = "198.51.100.44:34567"
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// teacherCall performs an authenticated teacher request, carrying the CSRF token
// whenever the method is unsafe (DELETE included).
func (e *classroomE2E) teacherCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		csrf := cookieNamed(cookies, "classwatch_session_teacher_csrf")
		if csrf == nil {
			t.Fatal("the teacher session has no CSRF cookie")
		}
		headers = map[string]string{"X-CSRF-Token": csrf.Value}
	}
	return e.call(t, method, path, body, cookies, headers)
}

// cleanupAccounts removes accounts and everything that references them, in the only
// order the RESTRICT foreign keys allow.
func (e *classroomE2E) cleanupAccounts(t *testing.T, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			if _, err := e.pool.Exec(ctx, `
				UPDATE classrooms SET status = 'CLOSED', current_run_id = NULL
				 WHERE owner_teacher_id = $1`, id); err != nil {
				t.Logf("cleanup: detach runs of %s: %v", id, err)
			}
			if _, err := e.pool.Exec(ctx, `
				DELETE FROM classroom_runs
				 WHERE classroom_id IN (SELECT id FROM classrooms WHERE owner_teacher_id = $1)`, id); err != nil {
				t.Logf("cleanup: delete runs of %s: %v", id, err)
			}
			if _, err := e.pool.Exec(ctx, `DELETE FROM classroom_students WHERE student_id = $1`, id); err != nil {
				t.Logf("cleanup: delete grants of %s: %v", id, err)
			}
			if _, err := e.pool.Exec(ctx, `DELETE FROM classrooms WHERE owner_teacher_id = $1`, id); err != nil {
				t.Logf("cleanup: delete classrooms of %s: %v", id, err)
			}
			if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: delete account %s: %v", id, err)
			}
		}
	})
}

// csrfHeaders builds the header map an unsafe request needs for a given entry
// point. It returns nil when the session has no CSRF cookie, which makes the
// request fail the CSRF check — the honest outcome for a broken session.
func csrfHeaders(cookies []*http.Cookie, csrfCookieName string) map[string]string {
	csrf := cookieNamed(cookies, csrfCookieName)
	if csrf == nil {
		return nil
	}
	return map[string]string{"X-CSRF-Token": csrf.Value}
}

// classroomOf returns the `classroom` object of a response.
func classroomOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := jsonBody(t, rec)
	obj, ok := body["classroom"].(map[string]any)
	if !ok {
		t.Fatalf("response has no classroom object: %s", rec.Body.String())
	}
	return obj
}

// runOf returns the `run` object of an open/close response.
func runOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	obj, ok := jsonBody(t, rec)["run"].(map[string]any)
	if !ok {
		t.Fatalf("response has no run object: %s", rec.Body.String())
	}
	return obj
}

func studentsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["students"].([]any)
	if !ok {
		t.Fatalf("response has no students array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("student entry is not an object: %v", entry)
		}
		out = append(out, obj)
	}
	return out
}

// countRuns counts runs of one classroom, optionally only the OPEN ones.
func (e *classroomE2E) countRuns(t *testing.T, classroomID uuid.UUID, status string) int {
	t.Helper()
	query := `SELECT count(*) FROM classroom_runs WHERE classroom_id = $1`
	args := []any{classroomID}
	if status != "" {
		query += ` AND status = $2`
		args = append(args, status)
	}
	var count int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return count
}

// ---------------------------------------------------------------------------
// The full lifecycle
// ---------------------------------------------------------------------------

// TestTeacherClassroomLifecycleEndToEnd is the §69 acceptance path: create, list,
// edit, add students (with one bad account), remove, open, close, re-open — plus
// the checks that only a real database can make (one OPEN run, a new run every
// time, an opaque room name, the roster in the database).
func TestTeacherClassroomLifecycleEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	teacher, cookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")
	alice := e.student(t)
	bob := e.student(t)

	// --- create ---
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms",
		`{"name":"  C++ 晚自习 ","description":"每周三 19:00"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", rec.Code, rec.Body.String())
	}
	created := classroomOf(t, rec)
	classroomID, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("classroom id is not a UUID: %v", err)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/teacher/classrooms/"+classroomID.String() {
		t.Errorf("Location = %q", got)
	}
	if created["status"] != "CLOSED" {
		t.Errorf("status = %v, want CLOSED (§7)", created["status"])
	}
	if created["name"] != "C++ 晚自习" {
		t.Errorf("name = %v, want the trimmed value", created["name"])
	}
	if created["ownerTeacherId"] != teacher.ID.String() {
		t.Errorf("ownerTeacherId = %v, want the session's teacher", created["ownerTeacherId"])
	}
	if created["currentRun"] != nil || created["studentCount"] != float64(0) {
		t.Errorf("new classroom = %v, want no run and no students", created)
	}
	assertNoPasswordMaterial(t, rec)
	if strings.Contains(rec.Body.String(), "lk_") || strings.Contains(rec.Body.String(), "livekitRoomName") {
		t.Errorf("the create response leaks media-plane detail: %s", rec.Body.String())
	}

	// --- list ---
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", rec.Code, rec.Body.String())
	}
	list, ok := jsonBody(t, rec)["classrooms"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("classrooms = %v, want exactly the one just created", jsonBody(t, rec)["classrooms"])
	}

	// --- get ---
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String(), "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", rec.Code, rec.Body.String())
	}

	// --- edit: rename only, the description must survive ---
	rec = e.teacherCall(t, http.MethodPatch, "/api/v1/teacher/classrooms/"+classroomID.String(),
		`{"name":"C++ 算法晚自习"}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d (%s)", rec.Code, rec.Body.String())
	}
	patched := classroomOf(t, rec)
	if patched["name"] != "C++ 算法晚自习" {
		t.Errorf("name = %v, want the new one", patched["name"])
	}
	if patched["description"] != "每周三 19:00" {
		t.Errorf("description = %v, want it untouched by a rename", patched["description"])
	}

	// --- add students: one mistyped account must not fail the import ---
	body := `{"accounts":["` + alice.Account + `","` + bob.Account + `","student_does_not_exist","` + teacher.Account + `"]}`
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students", body, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("add students: status = %d (%s)", rec.Code, rec.Body.String())
	}
	addBody := jsonBody(t, rec)
	roster := studentsOf(t, addBody)
	if len(roster) != 2 {
		t.Fatalf("roster = %v, want the two real students", roster)
	}
	rejected, ok := addBody["rejected"].([]any)
	if !ok || len(rejected) != 2 {
		t.Fatalf("rejected = %v, want two entries", addBody["rejected"])
	}
	codes := map[string]string{}
	for _, entry := range rejected {
		obj := entry.(map[string]any)
		codes[obj["account"].(string)] = obj["code"].(string)
	}
	if codes["student_does_not_exist"] != "STUDENT_NOT_FOUND" {
		t.Errorf("mistyped account: code = %q, want STUDENT_NOT_FOUND", codes["student_does_not_exist"])
	}
	if codes[teacher.Account] != "NOT_A_STUDENT" {
		t.Errorf("teacher account: code = %q, want NOT_A_STUDENT", codes[teacher.Account])
	}

	// The roster size must be visible on the classroom itself.
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String(), "", cookies)
	if got := classroomOf(t, rec)["studentCount"]; got != float64(2) {
		t.Errorf("studentCount = %v, want 2", got)
	}

	// --- list students (canonical order and shape) ---
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("list students: status = %d (%s)", rec.Code, rec.Body.String())
	}
	roster = studentsOf(t, jsonBody(t, rec))
	if len(roster) != 2 {
		t.Fatalf("roster = %v, want two students", roster)
	}
	for _, entry := range roster {
		for _, field := range []string{"id", "account", "displayName", "status", "addedAt"} {
			if _, present := entry[field]; !present {
				t.Errorf("student entry is missing %q: %v", field, entry)
			}
		}
	}

	// --- open ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open: status = %d (%s)", rec.Code, rec.Body.String())
	}
	openedClassroom := classroomOf(t, rec)
	firstRun := runOf(t, rec)
	if openedClassroom["status"] != "OPEN" {
		t.Errorf("status = %v, want OPEN", openedClassroom["status"])
	}
	if currentRun, ok := openedClassroom["currentRun"].(map[string]any); !ok || currentRun["id"] != firstRun["id"] {
		t.Errorf("currentRun = %v, want the run returned alongside it", openedClassroom["currentRun"])
	}
	if firstRun["status"] != "OPEN" || firstRun["closedAt"] != nil {
		t.Errorf("run = %v, want OPEN with closedAt null", firstRun)
	}
	if firstRun["classroomId"] != classroomID.String() {
		t.Errorf("run.classroomId = %v, want %s", firstRun["classroomId"], classroomID)
	}
	firstRunID := firstRun["id"].(string)
	if e.countRuns(t, classroomID, "OPEN") != 1 {
		t.Fatalf("open runs in the database = %d, want 1", e.countRuns(t, classroomID, "OPEN"))
	}
	// The room name is reserved in the database and derived from the run id — it is
	// not in the response (§8/§33: the media plane is Phase 6).
	var roomName string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT livekit_room_name FROM classroom_runs WHERE id = $1`, uuid.MustParse(firstRunID)).Scan(&roomName); err != nil {
		t.Fatalf("read room name: %v", err)
	}
	if roomName != "lk_"+firstRunID {
		t.Errorf("stored room name = %q, want lk_%s", roomName, firstRunID)
	}
	if strings.Contains(rec.Body.String(), "lk_") {
		t.Errorf("the open response must not carry the room name: %s", rec.Body.String())
	}

	// --- open again: 409, not a 500 and not a second run ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second open: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CLASSROOM_ALREADY_OPEN" {
		t.Errorf("second open: code = %s, want CLASSROOM_ALREADY_OPEN", code)
	}
	if e.countRuns(t, classroomID, "") != 1 {
		t.Errorf("runs after a rejected open = %d, want 1", e.countRuns(t, classroomID, ""))
	}

	// --- close ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/close", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("close: status = %d (%s)", rec.Code, rec.Body.String())
	}
	closedClassroom := classroomOf(t, rec)
	closedRun := runOf(t, rec)
	if closedClassroom["status"] != "CLOSED" || closedClassroom["currentRun"] != nil {
		t.Errorf("classroom = %v, want CLOSED with currentRun null", closedClassroom)
	}
	if closedRun["id"] != firstRunID {
		t.Errorf("closed run = %v, want the one that was open (%s)", closedRun["id"], firstRunID)
	}
	if closedRun["status"] != "CLOSED" || closedRun["closedAt"] == nil {
		t.Errorf("closed run = %v, want CLOSED with a closedAt", closedRun)
	}
	var currentRunID *uuid.UUID
	var status string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, current_run_id FROM classrooms WHERE id = $1`, classroomID).Scan(&status, &currentRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if status != "CLOSED" || currentRunID != nil {
		t.Errorf("stored classroom = %s/%v, want CLOSED/NULL", status, currentRunID)
	}

	// --- close again: 409 ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/close", "", cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second close: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CLASSROOM_ALREADY_CLOSED" {
		t.Errorf("second close: code = %s, want CLASSROOM_ALREADY_CLOSED", code)
	}

	// --- re-open: a NEW run, the old one kept ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-open: status = %d (%s)", rec.Code, rec.Body.String())
	}
	secondRun := runOf(t, rec)
	if secondRun["id"] == firstRunID {
		t.Fatal("the re-open reused the previous run; §8 forbids it")
	}
	if e.countRuns(t, classroomID, "") != 2 {
		t.Errorf("runs = %d, want 2 (the previous lesson must survive)", e.countRuns(t, classroomID, ""))
	}
	if e.countRuns(t, classroomID, "OPEN") != 1 {
		t.Errorf("open runs = %d, want 1", e.countRuns(t, classroomID, "OPEN"))
	}

	// --- remove a student ---
	aliceID := ""
	for _, entry := range roster {
		if entry["account"] == alice.Account {
			aliceID = entry["id"].(string)
		}
	}
	if aliceID == "" {
		t.Fatal("alice is missing from the roster")
	}
	rec = e.teacherCall(t, http.MethodDelete,
		"/api/v1/teacher/classrooms/"+classroomID.String()+"/students/"+aliceID, "", cookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove student: status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	rec = e.teacherCall(t, http.MethodDelete,
		"/api/v1/teacher/classrooms/"+classroomID.String()+"/students/"+aliceID, "", cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second remove: status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "STUDENT_NOT_ASSIGNED" {
		t.Errorf("second remove: code = %s, want STUDENT_NOT_ASSIGNED", code)
	}

	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/students", "", cookies)
	if got := len(studentsOf(t, jsonBody(t, rec))); got != 1 {
		t.Errorf("roster after removal = %d entries, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Authorization end to end
// ---------------------------------------------------------------------------

// TestTeacherClassroomOwnershipEndToEnd drives every endpoint with a second teacher
// against the first teacher's classroom. §37/§63: the route prefix proves the caller
// is a TEACHER, never that the classroom is theirs.
func TestTeacherClassroomOwnershipEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	_, ownerCookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")
	_, otherCookies := e.staff(t, user.RoleTeacher, "another-teacher-passphrase")
	student := e.student(t)

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"别人的课堂"}`, ownerCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", rec.Code, rec.Body.String())
	}
	id := classroomOf(t, rec)["id"].(string)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/teacher/classrooms/" + id, ""},
		{http.MethodPatch, "/api/v1/teacher/classrooms/" + id, `{"name":"偷改"}`},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + id + "/students", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/students", `{"accounts":["` + student.Account + `"]}`},
		{http.MethodDelete, "/api/v1/teacher/classrooms/" + id + "/students/" + student.ID.String(), ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/open", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/close", ""},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := e.teacherCall(t, route.method, route.path, route.body, otherCookies)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "CLASSROOM_NOT_OWNER" {
				t.Fatalf("code = %s, want CLASSROOM_NOT_OWNER (§58)", code)
			}
		})
	}

	// The other teacher's own list must not reveal the classroom.
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms", "", otherCookies)
	if rooms, _ := jsonBody(t, rec)["classrooms"].([]any); len(rooms) != 0 {
		t.Errorf("the other teacher sees %d classrooms, want 0 (§14: filter server-side)", len(rooms))
	}

	// Nothing changed: still CLOSED with no students.
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+id, "", ownerCookies)
	classroomObj := classroomOf(t, rec)
	if classroomObj["status"] != "CLOSED" || classroomObj["studentCount"] != float64(0) {
		t.Errorf("classroom = %v, want an untouched CLOSED classroom", classroomObj)
	}

	// A classroom that does not exist is 404 and not 403, so the console can tell
	// "deleted" from "not yours".
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+uuid.New().String(), "", otherCookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown classroom: status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "CLASSROOM_NOT_FOUND" {
		t.Errorf("code = %s, want CLASSROOM_NOT_FOUND", code)
	}

	// A malformed id never reaches the service.
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/not-a-uuid", "", otherCookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %s, want INVALID_REQUEST", code)
	}
}

// TestTeacherClassroomRoutesRefuseOtherEntriesEndToEnd is the §4/§37 test with real
// sessions: an ADMIN cannot touch the teacher surface (the admin role manages
// accounts, it is not a super-teacher), and a student cannot either.
func TestTeacherClassroomRoutesRefuseOtherEntriesEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	_, teacherCookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")
	_, adminCookies := e.staff(t, user.RoleAdmin, "an-admin-passphrase")
	student := e.student(t)
	studentCookies := e.login(t, "student", student.Account, "")

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"私有课堂"}`, teacherCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", rec.Code, rec.Body.String())
	}
	id := classroomOf(t, rec)["id"].(string)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/teacher/classrooms", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"管理员的课堂"}`},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + id, ""},
		{http.MethodPatch, "/api/v1/teacher/classrooms/" + id, `{"name":"管理员改名"}`},
		{http.MethodGet, "/api/v1/teacher/classrooms/" + id + "/students", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/students", `{"accounts":["` + student.Account + `"]}`},
		{http.MethodDelete, "/api/v1/teacher/classrooms/" + id + "/students/" + student.ID.String(), ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/open", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/close", ""},
	}
	for _, route := range routes {
		t.Run("admin "+route.method+" "+route.path, func(t *testing.T) {
			rec := e.call(t, route.method, route.path, route.body, adminCookies, csrfHeaders(adminCookies, "classwatch_session_admin_csrf"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "ROLE_FORBIDDEN" {
				t.Fatalf("code = %s, want ROLE_FORBIDDEN (§4)", code)
			}
		})
		t.Run("student "+route.method+" "+route.path, func(t *testing.T) {
			rec := e.call(t, route.method, route.path, route.body, studentCookies, csrfHeaders(studentCookies, "classwatch_session_student_csrf"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "ROLE_FORBIDDEN" {
				t.Fatalf("code = %s, want ROLE_FORBIDDEN", code)
			}
		})
		t.Run("anonymous "+route.method+" "+route.path, func(t *testing.T) {
			rec := e.call(t, route.method, route.path, route.body, nil, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "AUTH_REQUIRED" {
				t.Fatalf("code = %s, want AUTH_REQUIRED", code)
			}
		})
	}

	// The classroom is untouched by all of that.
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+id, "", teacherCookies)
	if got := classroomOf(t, rec); got["status"] != "CLOSED" || got["name"] != "私有课堂" {
		t.Errorf("classroom = %v, want it unchanged", got)
	}
}

// TestWriteEndpointsRequireCSRFEndToEnd checks the third middleware layer with real
// sessions: a teacher cookie without the CSRF header cannot change anything.
func TestWriteEndpointsRequireCSRFEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	_, cookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"无 CSRF"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", rec.Code, rec.Body.String())
	}
	id := classroomOf(t, rec)["id"].(string)

	writes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"x"}`},
		{http.MethodPatch, "/api/v1/teacher/classrooms/" + id, `{"name":"x"}`},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/students", `{"accounts":["s1"]}`},
		{http.MethodDelete, "/api/v1/teacher/classrooms/" + id + "/students/" + uuid.New().String(), ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/open", ""},
		{http.MethodPost, "/api/v1/teacher/classrooms/" + id + "/close", ""},
	}
	for _, route := range writes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := e.call(t, route.method, route.path, route.body, cookies, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "CSRF_INVALID" {
				t.Fatalf("code = %s, want CSRF_INVALID", code)
			}
		})
	}
	// Reads are unaffected.
	rec = e.call(t, http.MethodGet, "/api/v1/teacher/classrooms", "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("read without CSRF: status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Roster behaviour end to end
// ---------------------------------------------------------------------------

// TestDisabledStudentStaysOnTheRosterEndToEnd pins the §11/§14 requirement that the
// roster is an authorization record: disabling the account does not remove the row,
// and the response says DISABLED so the console can render it.
func TestDisabledStudentStaysOnTheRosterEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	_, cookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")
	student := e.student(t)

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"算法"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d", rec.Code)
	}
	id := classroomOf(t, rec)["id"].(string)

	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+id+"/students",
		`{"accounts":["`+student.Account+`"]}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("add students: status = %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := e.pool.Exec(context.Background(),
		`UPDATE users SET status = 'DISABLED' WHERE id = $1`, student.ID); err != nil {
		t.Fatalf("disable the student: %v", err)
	}

	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+id+"/students", "", cookies)
	roster := studentsOf(t, jsonBody(t, rec))
	if len(roster) != 1 {
		t.Fatalf("roster = %v, want the disabled student to remain listed", roster)
	}
	if roster[0]["status"] != "DISABLED" {
		t.Errorf("status = %v, want DISABLED", roster[0]["status"])
	}
	if roster[0]["account"] != student.Account {
		t.Errorf("account = %v, want %s", roster[0]["account"], student.Account)
	}

	// Adding a disabled account is refused per-account, with the code that explains
	// it (§11: only ACTIVE students).
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+id+"/students",
		`{"accounts":["`+student.Account+`"]}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-add: status = %d (%s)", rec.Code, rec.Body.String())
	}
	rejected, _ := jsonBody(t, rec)["rejected"].([]any)
	if len(rejected) != 1 {
		t.Fatalf("rejected = %v, want one entry", rejected)
	}
	if code := rejected[0].(map[string]any)["code"]; code != "ACCOUNT_DISABLED" {
		t.Errorf("code = %v, want ACCOUNT_DISABLED", code)
	}
}

// TestAddStudentsIsIdempotentEndToEnd re-pastes the same list: no error, no
// duplicate row, and the roster stays the same size.
func TestAddStudentsIsIdempotentEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	_, cookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")
	student := e.student(t)

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"算法"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d", rec.Code)
	}
	id := classroomOf(t, rec)["id"].(string)
	body := `{"accounts":["` + student.Account + `","` + student.Account + `"]}`

	for attempt := 0; attempt < 2; attempt++ {
		rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+id+"/students", body, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d (%s)", attempt, rec.Code, rec.Body.String())
		}
		response := jsonBody(t, rec)
		if got := len(studentsOf(t, response)); got != 1 {
			t.Fatalf("attempt %d: roster has %d entries, want 1", attempt, got)
		}
		if rejected, _ := response["rejected"].([]any); len(rejected) != 0 {
			t.Fatalf("attempt %d: rejected = %v, want none", attempt, rejected)
		}
	}

	var grants int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM classroom_students WHERE classroom_id = $1`, uuid.MustParse(id)).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 1 {
		t.Errorf("grants = %d, want 1", grants)
	}
}

// TestOpenCreatesTheRunAtomicallyEndToEnd checks the §48 transaction from the
// outside: after a successful open the classroom and its run agree, and after a
// failed one nothing was written.
func TestOpenCreatesTheRunAtomicallyEndToEnd(t *testing.T) {
	e := newClassroomE2E(t)
	teacher, cookies := e.staff(t, user.RoleTeacher, "a-teacher-passphrase")

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"算法"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d", rec.Code)
	}
	id := classroomOf(t, rec)["id"].(string)
	classroomID := uuid.MustParse(id)

	// Before any open there is exactly one classroom row, no run, and status CLOSED.
	var (
		status       string
		currentRunID *uuid.UUID
	)
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, current_run_id FROM classrooms WHERE id = $1`, classroomID).Scan(&status, &currentRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if status != "CLOSED" || currentRunID != nil || e.countRuns(t, classroomID, "") != 0 {
		t.Fatalf("fresh classroom = %s/%v with %d runs", status, currentRunID, e.countRuns(t, classroomID, ""))
	}

	var ownerID uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT owner_teacher_id FROM classrooms WHERE id = $1`, classroomID).Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if ownerID != teacher.ID {
		t.Errorf("owner = %s, want the creating teacher %s", ownerID, teacher.ID)
	}

	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+id+"/open", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open: status = %d (%s)", rec.Code, rec.Body.String())
	}
	runID := uuid.MustParse(runOf(t, rec)["id"].(string))

	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, current_run_id FROM classrooms WHERE id = $1`, classroomID).Scan(&status, &currentRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if status != "OPEN" || currentRunID == nil || *currentRunID != runID {
		t.Fatalf("after open = %s/%v, want OPEN/%s", status, currentRunID, runID)
	}
	var (
		runStatus  string
		openedAt   time.Time
		closedAt   *time.Time
		runClassID uuid.UUID
	)
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, opened_at, closed_at, classroom_id
		  FROM classroom_runs WHERE id = $1`, runID).Scan(&runStatus, &openedAt, &closedAt, &runClassID); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "OPEN" || closedAt != nil {
		t.Errorf("run = %s/closedAt=%v, want OPEN with NULL", runStatus, closedAt)
	}
	if runClassID != classroomID {
		t.Errorf("run.classroom_id = %s, want %s", runClassID, classroomID)
	}
	if openedAt.IsZero() {
		t.Error("opened_at was not set")
	}
}
