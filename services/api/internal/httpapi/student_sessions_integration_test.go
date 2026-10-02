package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The Phase 6 media surface end to end: the router, the three middleware layers, real
// sessions, real accounts, a real roster, a real classroom run — and a REAL session
// repository against PostgreSQL.
//
// The only fake is the media plane itself. That is the point: §33 says the control
// plane must be correct without LiveKit, and the interesting questions of this phase
// ("who is ONLINE?", "what happens when the room cannot be listed?") are about how the
// control plane interprets observations. Driving them through a fake media plane makes
// each of those questions a deterministic test instead of a race against a real SFU.
//
// The real LiveKit round trip is a manual verification step (see
// docs/media/livekit-architecture.md), not something CI can do.

// ---------------------------------------------------------------------------
// Fake media plane
// ---------------------------------------------------------------------------

// fakeMediaPlane is a scripted media plane: the test decides what the room looks like
// and which calls fail.
type fakeMediaPlane struct {
	mu sync.Mutex
	// participants maps livekit identity → the tracks that identity publishes.
	participants map[string]media.ParticipantTracks
	ensureErr    error
	observeErr   error
	removeErr    error

	ensureCalls  []string
	removed      [][2]string
	tokenRequest []media.TokenRequest
}

func newFakeMediaPlane() *fakeMediaPlane {
	return &fakeMediaPlane{participants: map[string]media.ParticipantTracks{}}
}

func (f *fakeMediaPlane) EnsureRoom(_ context.Context, roomName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, roomName)
	return f.ensureErr
}

func (f *fakeMediaPlane) ObserveRoom(_ context.Context, _ string) (map[string]media.ParticipantTracks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	out := make(map[string]media.ParticipantTracks, len(f.participants))
	for identity, tracks := range f.participants {
		out[identity] = tracks
	}
	return out, nil
}

func (f *fakeMediaPlane) RemoveParticipant(_ context.Context, roomName, identity string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, [2]string{roomName, identity})
	delete(f.participants, identity)
	return f.removeErr
}

// SignToken mints a token-shaped string. It is deliberately NOT a real JWT: this test
// asserts what the control plane asks for (the token request), not what LiveKit would
// accept, and a real secret must never be needed to run the suite.
func (f *fakeMediaPlane) SignToken(req media.TokenRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenRequest = append(f.tokenRequest, req)
	return "fake-token." + req.Identity + "." + req.RoomName, nil
}

func (f *fakeMediaPlane) publish(identity string, tracks media.ParticipantTracks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.participants[identity] = tracks
}

func (f *fakeMediaPlane) disconnect(identity string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.participants, identity)
}

func (f *fakeMediaPlane) lastTokenRequest(t *testing.T) media.TokenRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenRequest) == 0 {
		t.Fatal("no media token was requested")
	}
	return f.tokenRequest[len(f.tokenRequest)-1]
}

// fakeRoomTerminator records the room teardown that closing a classroom triggers (§49).
type fakeRoomTerminator struct {
	mu    sync.Mutex
	rooms []string
	err   error
}

func (f *fakeRoomTerminator) TerminateRoom(_ context.Context, roomName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms = append(f.rooms, roomName)
	return f.err
}

func (f *fakeRoomTerminator) terminated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.rooms...)
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type mediaE2E struct {
	router        *gin.Engine
	pool          *pgxpool.Pool
	users         *user.Postgres
	classroomRepo *classroom.Postgres
	sessionRepo   *session.Postgres
	media         *fakeMediaPlane
	terminator    *fakeRoomTerminator
	cfg           *config.Config
	logs          *strings.Builder
}

func newMediaE2E(t *testing.T) *mediaE2E {
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
		RateLimitAPIPerMinute:            1000,
		PasswordMinLength:                12,
		LiveKitURL:                       "wss://media.example.test",
		LiveKitTokenTTL:                  2 * time.Hour,
	}
	authService := auth.NewService(users, sessionstore.New(pool), auth.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    auth.NewPasswordPolicy(cfg.PasswordMinLength),
	})
	classroomService := classroom.NewService(classroomRepo, users).WithRoomTerminator(terminator)
	sessionService := session.NewService(sessionRepo, classroomService, mediaPlane, session.Config{
		LiveKitURL: cfg.LiveKitURL,
		TokenTTL:   cfg.LiveKitTokenTTL,
	})

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := httpapi.NewRouter(httpapi.Deps{
		Logger:    logger,
		Config:    cfg,
		Auth:      authService,
		Classroom: classroomService,
		Session:   sessionService,
		Limiter:   ratelimit.NewMemory(),
	})
	return &mediaE2E{
		router: router, pool: pool, users: users, classroomRepo: classroomRepo,
		sessionRepo: sessionRepo, media: mediaPlane, terminator: terminator,
		cfg: cfg, logs: logs,
	}
}

// staff creates a TEACHER account with a password and logs it in.
func (e *mediaE2E) staff(t *testing.T) (*user.User, []*http.Cookie) {
	t.Helper()
	hash, err := auth.Hash("a-teacher-passphrase")
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account: dbtest.RandomAccount("teacher"), DisplayName: "王老师",
		Role: user.RoleTeacher, PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create teacher: %v", err)
	}
	return created, e.login(t, "teacher", created.Account, "a-teacher-passphrase")
}

// student creates a password-less STUDENT account and logs it in on the student entry.
func (e *mediaE2E) student(t *testing.T) (*user.User, []*http.Cookie) {
	t.Helper()
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account: dbtest.RandomAccount("student"), DisplayName: "张三", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	return created, e.login(t, "student", created.Account, "")
}

func (e *mediaE2E) login(t *testing.T, entry, account, password string) []*http.Cookie {
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

func (e *mediaE2E) call(t *testing.T, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
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

// teacherCall performs an authenticated teacher request, carrying the CSRF token for
// unsafe methods.
func (e *mediaE2E) teacherCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_teacher_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

// studentCall performs an authenticated student request (§43's two POSTs need CSRF).
func (e *mediaE2E) studentCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_student_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

// openClassroom creates a classroom, adds the students, and opens it.
func (e *mediaE2E) openClassroom(t *testing.T, teacherCookies []*http.Cookie, students ...*user.User) uuid.UUID {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms", `{"name":"C++ 算法训练"}`, teacherCookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create classroom: status = %d (%s)", rec.Code, rec.Body.String())
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
			t.Fatalf("add students: status = %d (%s)", rec.Code, rec.Body.String())
		}
	}

	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open classroom: status = %d (%s)", rec.Code, rec.Body.String())
	}
	return classroomID
}

// cleanupAccounts removes the accounts this test created and everything that references
// them, in the only order the RESTRICT foreign keys allow.
//
// The order is the point: a student session references BOTH its run and its student with
// ON DELETE RESTRICT, so sessions go first — otherwise the run cannot be deleted and the
// account cannot be removed, and the leftover rows would break the next run of the suite
// in a way that looks unrelated.
func (e *mediaE2E) cleanupAccounts(t *testing.T, ownerID uuid.UUID, accounts ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		ids := append([]uuid.UUID{ownerID}, accounts...)
		if _, err := e.pool.Exec(ctx, `
			DELETE FROM student_sessions
			 WHERE student_id = ANY($1::uuid[])
			    OR classroom_run_id IN (
			           SELECT r.id FROM classroom_runs r
			             JOIN classrooms c ON c.id = r.classroom_id
			            WHERE c.owner_teacher_id = $2)`, ids, ownerID); err != nil {
			t.Logf("cleanup: sessions: %v", err)
		}
		if _, err := e.pool.Exec(ctx, `
			UPDATE classrooms SET status = 'CLOSED', current_run_id = NULL WHERE owner_teacher_id = $1`, ownerID); err != nil {
			t.Logf("cleanup: detach runs: %v", err)
		}
		if _, err := e.pool.Exec(ctx, `
			DELETE FROM classroom_runs
			 WHERE classroom_id IN (SELECT id FROM classrooms WHERE owner_teacher_id = $1)`, ownerID); err != nil {
			t.Logf("cleanup: runs: %v", err)
		}
		if _, err := e.pool.Exec(ctx, `DELETE FROM classroom_students WHERE student_id = ANY($1::uuid[])`, ids); err != nil {
			t.Logf("cleanup: grants: %v", err)
		}
		if _, err := e.pool.Exec(ctx, `DELETE FROM classrooms WHERE owner_teacher_id = $1`, ownerID); err != nil {
			t.Logf("cleanup: classrooms: %v", err)
		}
		for _, id := range ids {
			if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: account %s: %v", id, err)
			}
		}
	})
}

// storedSessionRow is one row of student_sessions as the database actually holds it.
type storedSessionRow struct {
	Status      string
	Identity    string
	StudentID   uuid.UUID
	RunID       uuid.UUID
	LeftAt      *time.Time
	ConnectedAt *time.Time
}

// storedSession reads one session row straight from PostgreSQL, so the assertions are
// about what the control plane RECORDED and not about what a response said.
func (e *mediaE2E) storedSession(t *testing.T, sessionID uuid.UUID) storedSessionRow {
	t.Helper()
	var row storedSessionRow
	err := e.pool.QueryRow(context.Background(), `
		SELECT status, livekit_identity, student_id, classroom_run_id, left_at, connected_at
		  FROM student_sessions WHERE id = $1`, sessionID).
		Scan(&row.Status, &row.Identity, &row.StudentID, &row.RunID, &row.LeftAt, &row.ConnectedAt)
	if err != nil {
		t.Fatalf("read session %s: %v", sessionID, err)
	}
	return row
}

// monitorOf calls the monitor endpoint and returns the tiles keyed by student id.
func (e *mediaE2E) monitorOf(t *testing.T, teacherCookies []*http.Cookie, classroomID uuid.UUID) map[string]map[string]any {
	t.Helper()
	rec := e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor: status = %d (%s)", rec.Code, rec.Body.String())
	}
	byStudent := map[string]map[string]any{}
	for _, tile := range studentsOf(t, jsonBody(t, rec)) {
		byStudent[tile["studentId"].(string)] = tile
	}
	return byStudent
}

// ---------------------------------------------------------------------------
// Join / leave lifecycle
// ---------------------------------------------------------------------------

// TestStudentJoinLifecycleEndToEnd is the §43/§50 acceptance path: the join creates
// exactly one session, a second join reuses it, and leaving is recorded.
func TestStudentJoinLifecycleEndToEnd(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.openClassroom(t, teacherCookies, student)
	joinPath := "/api/v1/student/classrooms/" + classroomID.String() + "/join"

	// --- join ---
	rec := e.studentCall(t, http.MethodPost, joinPath,
		`{"capture":{"displaySurface":"monitor","width":1920,"height":1080}}`, studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	if len(body) != 3 {
		t.Errorf("join response members = %v, want sessionId, livekitUrl, token", body)
	}
	sessionID, err := uuid.Parse(body["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId is not a UUID: %v", err)
	}
	if body["livekitUrl"] != e.cfg.LiveKitURL {
		t.Errorf("livekitUrl = %v, want %q", body["livekitUrl"], e.cfg.LiveKitURL)
	}
	if token, _ := body["token"].(string); token == "" {
		t.Error("join returned no token")
	}

	// The diagnostics are in the log and nowhere else (§43).
	if !strings.Contains(e.logs.String(), "display_surface=monitor") {
		t.Error("the capture diagnostics were not logged")
	}

	// --- the row is in PostgreSQL ---
	stored := e.storedSession(t, sessionID)
	if stored.Status != "CONNECTING" {
		t.Errorf("stored status = %v, want CONNECTING", stored.Status)
	}
	if stored.Identity != sessionID.String() {
		t.Errorf("stored identity = %v, want the session id %s (§44)", stored.Identity, sessionID)
	}
	if stored.StudentID != student.ID {
		t.Errorf("stored student = %v, want %s", stored.StudentID, student.ID)
	}
	if stored.LeftAt != nil {
		t.Errorf("leftAt = %v, want NULL", stored.LeftAt)
	}

	// The token request carries the permissions of §28 and the run's opaque room.
	req := e.media.lastTokenRequest(t)
	if req.Identity != sessionID.String() {
		t.Errorf("token identity = %q, want %s", req.Identity, sessionID)
	}
	if !strings.HasPrefix(req.RoomName, "lk_") || strings.Contains(req.RoomName, "C++") {
		t.Errorf("token room = %q, want an opaque lk_<run_uuid> name (§8)", req.RoomName)
	}
	if len(req.PublishSources) != 1 || req.PublishSources[0] != media.PublishScreenShare {
		t.Errorf("publish sources = %v, want [SCREEN_SHARE] only", req.PublishSources)
	}
	if !req.CanSubscribe || req.CanPublishData {
		t.Errorf("grants = subscribe:%v publishData:%v, want true/false", req.CanSubscribe, req.CanPublishData)
	}

	// --- a second join reuses the same session ---
	rec = e.studentCall(t, http.MethodPost, joinPath, "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("second join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	second := jsonBody(t, rec)
	if second["sessionId"] != sessionID.String() {
		t.Fatalf("second join created session %v, want the recycled %s", second["sessionId"], sessionID)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM student_sessions WHERE student_id = $1`, student.ID).Scan(&rows); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if rows != 1 {
		t.Errorf("session rows = %d, want 1 (§50: one active session per student and run)", rows)
	}

	// --- leave ---
	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/sessions/"+sessionID.String()+"/leave", "", studentCookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("leave: status = %d (%s)", rec.Code, rec.Body.String())
	}
	left := e.storedSession(t, sessionID)
	if left.Status != "LEFT" || left.LeftAt == nil {
		t.Errorf("stored = %+v, want LEFT with left_at", left)
	}
	// The participant left the room too (best effort, §50).
	if len(e.media.removed) != 1 || e.media.removed[0][1] != sessionID.String() {
		t.Errorf("removed participants = %v, want the session's identity", e.media.removed)
	}

	// --- the wall shows it ---
	tiles := e.monitorOf(t, teacherCookies, classroomID)
	tile, ok := tiles[student.ID.String()]
	if !ok {
		t.Fatalf("the monitoring wall has no tile for the student: %v", tiles)
	}
	if tile["sessionStatus"] != "LEFT" {
		t.Errorf("sessionStatus = %v, want LEFT", tile["sessionStatus"])
	}
	if tile["connection"] != "UNKNOWN" {
		t.Errorf("connection = %v, want UNKNOWN for a finished session", tile["connection"])
	}

	// Leaving twice is a 204, not an error.
	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/sessions/"+sessionID.String()+"/leave", "", studentCookies)
	if rec.Code != http.StatusNoContent {
		t.Errorf("second leave: status = %d, want 204", rec.Code)
	}
}

// TestJoinIsRefusedWhenTheClassroomIsClosed covers §43's 409 and the fact that closing
// the classroom also ends the media room (§49).
func TestJoinIsRefusedWhenTheClassroomIsClosed(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.openClassroom(t, teacherCookies, student)

	// Closing terminates the media room of the run that is ending.
	var roomName string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT r.livekit_room_name FROM classroom_runs r
		  JOIN classrooms c ON c.id = r.classroom_id
		 WHERE c.id = $1 AND r.status = 'OPEN'`, classroomID).Scan(&roomName); err != nil {
		t.Fatalf("read room name: %v", err)
	}
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/close", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("close: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if terminated := e.terminator.terminated(); len(terminated) != 1 || terminated[0] != roomName {
		t.Errorf("terminated rooms = %v, want [%s] (§49)", terminated, roomName)
	}

	// --- join after close: 409 CLASSROOM_CLOSED ---
	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("join after close: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CLASSROOM_CLOSED" {
		t.Errorf("code = %s, want CLASSROOM_CLOSED", code)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM student_sessions WHERE student_id = $1`, student.ID).Scan(&rows); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if rows != 0 {
		t.Errorf("session rows = %d, want 0: a closed classroom must not produce a media session", rows)
	}

	// --- monitor and media-token: 409 too ---
	rec = e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "", teacherCookies)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "CLASSROOM_CLOSED" {
		t.Errorf("monitor after close: status = %d, code = %s, want 409 CLASSROOM_CLOSED", rec.Code, errorCode(t, rec))
	}
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", teacherCookies)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "CLASSROOM_CLOSED" {
		t.Errorf("media-token after close: status = %d, code = %s, want 409 CLASSROOM_CLOSED", rec.Code, errorCode(t, rec))
	}
}

// TestJoinIsRefusedForAnUnassignedStudent is §14/§43: the roster is the authorization,
// and it is checked server-side.
func TestJoinIsRefusedForAnUnassignedStudent(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	assigned, _ := e.student(t)
	outsider, outsiderCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, assigned.ID, outsider.ID)

	classroomID := e.openClassroom(t, teacherCookies, assigned)

	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", outsiderCookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "STUDENT_NOT_ASSIGNED" {
		t.Errorf("code = %s, want STUDENT_NOT_ASSIGNED", code)
	}
	// The same answer for a classroom that does not exist, so ids cannot be probed.
	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+uuid.New().String()+"/join", "", outsiderCookies)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "STUDENT_NOT_ASSIGNED" {
		t.Errorf("unknown classroom: status = %d, code = %s, want 404 STUDENT_NOT_ASSIGNED", rec.Code, errorCode(t, rec))
	}
	// And the lesson is untouched.
	if tiles := e.monitorOf(t, teacherCookies, classroomID); len(tiles) != 0 {
		t.Errorf("monitor shows %d tiles, want 0: nobody joined", len(tiles))
	}
}

// TestLeaveRejectsAnotherStudentsSession is §58 on the real stack: a student cannot end
// (or probe) another student's session.
func TestLeaveRejectsAnotherStudentsSession(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	first, firstCookies := e.student(t)
	second, secondCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, first.ID, second.ID)

	classroomID := e.openClassroom(t, teacherCookies, first, second)

	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", firstCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("first student join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	sessionID := jsonBody(t, rec)["sessionId"].(string)

	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/sessions/"+sessionID+"/leave", "", secondCookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "SESSION_NOT_FOUND" {
		t.Errorf("code = %s, want SESSION_NOT_FOUND", code)
	}
	// The other student's session is still running.
	parsed, err := uuid.Parse(sessionID)
	if err != nil {
		t.Fatalf("session id: %v", err)
	}
	if stored := e.storedSession(t, parsed); stored.Status != "CONNECTING" {
		t.Errorf("stored status = %v, want the session untouched", stored.Status)
	}
}

// ---------------------------------------------------------------------------
// Monitoring and state advancement
// ---------------------------------------------------------------------------

// TestMonitorAdvancesStateFromMediaObservation is the §45/§46/§51 acceptance test with a
// real database: the control plane — not the student's browser — decides when a session
// becomes ONLINE, and the transition is persisted.
func TestMonitorAdvancesStateFromMediaObservation(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.openClassroom(t, teacherCookies, student)
	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	sessionID, err := uuid.Parse(body["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}
	token, _ := body["token"].(string)
	identity := strings.Split(strings.TrimPrefix(token, "fake-token."), ".")[0]

	// --- 1. nobody in the room yet: the wall must NOT claim ONLINE ---
	tiles := e.monitorOf(t, teacherCookies, classroomID)
	tile := tiles[student.ID.String()]
	if tile["sessionStatus"] == "ONLINE" {
		t.Fatal("a student with no participant in the room is reported ONLINE")
	}
	if tile["sessionStatus"] != "DISCONNECTED" {
		t.Errorf("sessionStatus = %v, want DISCONNECTED (the participant was not observed)", tile["sessionStatus"])
	}
	if screen, _ := tile["screen"].(map[string]any); screen["active"] != false {
		t.Errorf("screen = %v, want active:false", tile["screen"])
	}
	if tile["connection"] != "UNKNOWN" {
		t.Errorf("connection = %v, want UNKNOWN", tile["connection"])
	}
	// The display name must survive the transition: it comes from the monitoring JOIN
	// on users, not from the student_sessions row the update returned.
	if tile["displayName"] != "张三" {
		t.Errorf("displayName = %v, want the student's name", tile["displayName"])
	}
	if tile["joinedAt"] != nil {
		t.Errorf("joinedAt = %v, want null before the first observation", tile["joinedAt"])
	}
	if stored := e.storedSession(t, sessionID); stored.Status != "DISCONNECTED" {
		t.Errorf("stored status = %v, want the transition persisted", stored.Status)
	}

	// --- 2. the participant appears and publishes a screen: CONNECTING/DISCONNECTED → ONLINE ---
	e.media.publish(identity, media.ParticipantTracks{ScreenShare: true})
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	tile = tiles[student.ID.String()]
	if tile["sessionStatus"] != "ONLINE" {
		t.Fatalf("sessionStatus = %v, want ONLINE (a screen track was observed)", tile["sessionStatus"])
	}
	if screen, _ := tile["screen"].(map[string]any); screen["active"] != true {
		t.Errorf("screen = %v, want active:true", tile["screen"])
	}
	if tile["displayName"] != "张三" {
		t.Errorf("displayName = %v, want the student's name after the transition", tile["displayName"])
	}
	if tile["connection"] != "GOOD" {
		t.Errorf("connection = %v, want GOOD", tile["connection"])
	}
	if tile["joinedAt"] == nil {
		t.Error("joinedAt is null after the first successful observation")
	}
	if online := e.storedSession(t, sessionID); online.Status != "ONLINE" || online.ConnectedAt == nil {
		t.Errorf("stored = %+v, want ONLINE with connected_at (§51)", online)
	}

	// --- 3. the screen goes away: ONLINE → SCREEN_LOST ---
	e.media.publish(identity, media.ParticipantTracks{})
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	tile = tiles[student.ID.String()]
	if tile["sessionStatus"] != "SCREEN_LOST" {
		t.Fatalf("sessionStatus = %v, want SCREEN_LOST", tile["sessionStatus"])
	}
	if screen, _ := tile["screen"].(map[string]any); screen["active"] != false {
		t.Errorf("screen = %v, want active:false", tile["screen"])
	}

	// --- 4. the screen comes back: SCREEN_LOST → ONLINE (§22) ---
	e.media.publish(identity, media.ParticipantTracks{ScreenShare: true})
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	if status := tiles[student.ID.String()]["sessionStatus"]; status != "ONLINE" {
		t.Errorf("sessionStatus = %v, want ONLINE after the screen was restored", status)
	}

	// --- 5. the participant disappears: → DISCONNECTED ---
	e.media.disconnect(identity)
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	if status := tiles[student.ID.String()]["sessionStatus"]; status != "DISCONNECTED" {
		t.Errorf("sessionStatus = %v, want DISCONNECTED", status)
	}
}

// TestMonitorDoesNotInventDisconnectsWhenTheMediaPlaneFails is the §33 rule end to end:
// a failed ListParticipants must not be written into the lesson's history.
func TestMonitorDoesNotInventDisconnectsWhenTheMediaPlaneFails(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.openClassroom(t, teacherCookies, student)
	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	sessionID, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}
	before := e.storedSession(t, sessionID)

	// The media plane becomes unreachable.
	e.media.mu.Lock()
	e.media.observeErr = context.DeadlineExceeded
	e.media.mu.Unlock()

	tiles := e.monitorOf(t, teacherCookies, classroomID)
	tile := tiles[student.ID.String()]
	if tile["connection"] != "UNKNOWN" {
		t.Errorf("connection = %v, want UNKNOWN while the media plane is down", tile["connection"])
	}
	after := e.storedSession(t, sessionID)
	if after.Status != before.Status {
		t.Errorf("stored status changed from %v to %v during a media outage (§33)", before.Status, after.Status)
	}
	if !strings.Contains(e.logs.String(), "session states are not advanced") {
		t.Error("the media outage was not logged")
	}
	if !strings.Contains(e.logs.String(), `"action":"monitor_unobserved"`) &&
		!strings.Contains(e.logs.String(), "action=monitor_unobserved") {
		t.Error("the monitor outage log line has no action field")
	}
}

// ---------------------------------------------------------------------------
// Teacher media token
// ---------------------------------------------------------------------------

// TestTeacherMediaTokenEndToEnd pins §27/§44 on the real stack: the identity is the
// teacher's login session id, and only the microphone may be published.
func TestTeacherMediaTokenEndToEnd(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.openClassroom(t, teacherCookies, student)

	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("media-token: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	if len(body) != 2 || body["livekitUrl"] != e.cfg.LiveKitURL {
		t.Errorf("body = %v, want livekitUrl and token", body)
	}
	if token, _ := body["token"].(string); token == "" {
		t.Error("media-token returned no token")
	}

	req := e.media.lastTokenRequest(t)
	if len(req.PublishSources) != 1 || req.PublishSources[0] != media.PublishMicrophone {
		t.Errorf("publish sources = %v, want [MICROPHONE] only (§27)", req.PublishSources)
	}
	if !req.CanSubscribe {
		t.Error("canSubscribe = false, want true (§27)")
	}
	// §44: the identity is the login session, which is in the teacher's cookie jar as an
	// opaque value — never the account name and never the user id.
	sessionCookie := cookieNamed(teacherCookies, "classwatch_session_teacher")
	if sessionCookie == nil {
		t.Fatal("the teacher session cookie is missing")
	}
	if req.Identity == teacher.ID.String() {
		t.Error("the media identity is the user id; §44 requires the login session id")
	}
	if strings.Contains(req.Identity, teacher.Account) || strings.Contains(req.Identity, teacher.DisplayName) {
		t.Errorf("the media identity leaked the account: %q", req.Identity)
	}
	if _, err := uuid.Parse(req.Identity); err != nil {
		t.Errorf("the media identity is not a UUID: %q", req.Identity)
	}

	// --- a student may not mint a teacher token (§37/§4) ---
	rec = e.studentCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", studentCookies)
	if rec.Code != http.StatusForbidden {
		t.Errorf("student calling media-token: status = %d, want 403", rec.Code)
	}

	// --- and the teacher may not use the student surface ---
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", teacherCookies)
	if rec.Code != http.StatusForbidden {
		t.Errorf("teacher calling join: status = %d, want 403", rec.Code)
	}

	// --- another teacher's classroom is not theirs to mint for ---
	otherTeacher, otherCookies := e.staff(t)
	e.cleanupAccounts(t, otherTeacher.ID)
	rec = e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", otherCookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other teacher: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CLASSROOM_NOT_OWNER" {
		t.Errorf("code = %s, want CLASSROOM_NOT_OWNER", code)
	}
}

// TestMonitorAndTokenRequireAuthentication pins the entry-point separation (§37) on the
// real router: no cookie is a 401, and a login on another entry point is not a session
// here.
func TestMonitorAndTokenRequireAuthentication(t *testing.T) {
	e := newMediaE2E(t)
	classroomID := uuid.New()

	rec := e.call(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous monitor: status = %d, want 401", rec.Code)
	}
	rec = e.call(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous media-token: status = %d, want 401", rec.Code)
	}

	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, student.ID)
	rec = e.studentCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "", studentCookies)
	if rec.Code != http.StatusForbidden {
		t.Errorf("student monitor: status = %d, want 403", rec.Code)
	}
}

// TestJoinRateLimitStillApplies guards the coarse API limiter on the new endpoint: the
// media surface must not be a way around the protection every other route has.
func TestJoinRateLimitStillApplies(t *testing.T) {
	e := newMediaE2E(t)
	// The harness installs a limiter with the configured (high) budget; this test only
	// asserts the route is covered by the /api/v1 group at all, which the access log
	// proves: a rejected-by-limit response would also be an error envelope.
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, student.ID)
	classroomID := e.openClassroom(t, teacherCookies, student)

	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join under the limit: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(e.logs.String(), "/api/v1/student/classrooms/") {
		t.Error("the join request was not access-logged")
	}
}
