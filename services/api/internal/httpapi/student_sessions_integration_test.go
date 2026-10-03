package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
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

	// The §26 half: what the monitor asked to revoke, and what the fake reports back.
	enforceCalls []enforceCall
	enforceErr   error
	revoked      []media.PeerSubscriptionRevocation

	// The §31 half: the private-talk subscriptions the control plane asked for, modelled
	// as a real subscription book (identity → track sid → subscribed) so a test can assert
	// WHO ends up able to hear the teacher, not merely which RPCs were sent.
	talkCalls     []talkEnforceCall
	talkErr       error
	subscriptions map[string]map[string]bool
}

// enforceCall is one EnforceNoPeerSubscriptions invocation.
type enforceCall struct {
	room     string
	students []string
	allowed  []string
}

// talkEnforceCall is one EnforcePrivateTalk invocation.
type talkEnforceCall struct {
	room     string
	students []string
	teachers []string
	target   string
}

func newFakeMediaPlane() *fakeMediaPlane {
	return &fakeMediaPlane{
		participants:  map[string]media.ParticipantTracks{},
		subscriptions: map[string]map[string]bool{},
	}
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

// EnforceNoPeerSubscriptions is the §26 half of the fake. It applies the revocation to
// its own subscription bookkeeping instead of calling LiveKit, so an integration test can
// assert what the wall asked for without a real SFU.
func (f *fakeMediaPlane) EnforceNoPeerSubscriptions(
	_ context.Context,
	roomName string,
	students []string,
	observed map[string]media.ParticipantTracks,
	allowedTrackOwners []string,
) ([]media.PeerSubscriptionRevocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enforceCalls = append(f.enforceCalls, enforceCall{
		room: roomName, students: append([]string{}, students...), allowed: append([]string{}, allowedTrackOwners...),
	})
	if f.enforceErr != nil {
		return nil, f.enforceErr
	}
	return append([]media.PeerSubscriptionRevocation{}, f.revoked...), nil
}

// EnforcePrivateTalk is the §31 half of the fake: it applies the subscription change to a
// book of its own (identity → track sid → subscribed), which is what lets an integration
// test assert the END STATE — "only this student can hear the teacher" — rather than the
// RPC that produced it.
func (f *fakeMediaPlane) EnforcePrivateTalk(
	_ context.Context,
	roomName string,
	students []string,
	observed map[string]media.ParticipantTracks,
	teacherIdentities []string,
	targetIdentity string,
) (media.PrivateTalkEnforcement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.talkCalls = append(f.talkCalls, talkEnforceCall{
		room:     roomName,
		students: append([]string{}, students...),
		teachers: append([]string{}, teacherIdentities...),
		target:   targetIdentity,
	})
	if f.talkErr != nil {
		return media.PrivateTalkEnforcement{}, f.talkErr
	}

	var micSids []string
	for _, identity := range teacherIdentities {
		if _, present := observed[identity]; !present {
			continue
		}
		for _, track := range observed[identity].Tracks {
			if track.Source == media.PublishMicrophone && track.Sid != "" {
				micSids = append(micSids, track.Sid)
			}
		}
	}
	enforcement := media.PrivateTalkEnforcement{TrackSids: micSids}
	for _, student := range students {
		if _, present := observed[student]; !present {
			continue
		}
		subscribe := student == targetIdentity
		for _, sid := range micSids {
			if f.subscriptions[student] == nil {
				f.subscriptions[student] = map[string]bool{}
			}
			if f.subscriptions[student][sid] == subscribe {
				continue
			}
			f.subscriptions[student][sid] = subscribe
			change := media.PrivateTalkSubscription{StudentIdentity: student, TrackSid: sid}
			if subscribe {
				enforcement.Granted = append(enforcement.Granted, change)
			} else {
				enforcement.Revoked = append(enforcement.Revoked, change)
			}
		}
	}
	return enforcement, nil
}

// subscribedTo reports whether one student's client would be receiving one track.
func (f *fakeMediaPlane) subscribedTo(identity, trackSid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscriptions[identity][trackSid]
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

// assertNoSession asserts the null tile of §29: a student who is authorized but has no
// session in this run. The two session members must be null — not an empty string, not a
// zero uuid, and not a seventh status — and every media block must be present and false.
func assertNoSession(t *testing.T, tile map[string]any) {
	t.Helper()
	if tile["sessionId"] != nil || tile["sessionStatus"] != nil {
		t.Errorf("tile %v has a session, want null: this student never entered", tile)
	}
	if tile["connection"] != "UNKNOWN" || tile["joinedAt"] != nil || tile["lastEventAt"] != nil {
		t.Errorf("tile %v = %v/%v/%v, want UNKNOWN and null timestamps", tile, tile["connection"], tile["joinedAt"], tile["lastEventAt"])
	}
	for _, member := range []string{"screen", "camera", "microphone"} {
		track, ok := tile[member].(map[string]any)
		if !ok || track["active"] != false {
			t.Errorf("%s = %v, want {active:false}", member, tile[member])
		}
	}
}

// monitorOrder calls the monitor endpoint and returns the tiles' student ids IN ORDER.
func (e *mediaE2E) monitorOrder(t *testing.T, teacherCookies []*http.Cookie, classroomID uuid.UUID) []string {
	t.Helper()
	rec := e.teacherCall(t, http.MethodGet, "/api/v1/teacher/classrooms/"+classroomID.String()+"/monitor", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor: status = %d (%s)", rec.Code, rec.Body.String())
	}
	order := make([]string, 0, 8)
	for _, tile := range studentsOf(t, jsonBody(t, rec)) {
		order = append(order, tile["studentId"].(string))
	}
	return order
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

	// The token request carries the permissions of §28/§76 and the run's opaque room:
	// the screen (mandatory, §21), the camera (optional, §24) and — from Phase 10 — the
	// microphone (optional, §25). Phase 10 is the phase in which the student's three
	// sources are finally complete, and this list is asserted in ORDER so a fourth source
	// cannot appear without a deliberate change here.
	req := e.media.lastTokenRequest(t)
	if req.Identity != sessionID.String() {
		t.Errorf("token identity = %q, want %s", req.Identity, sessionID)
	}
	if !strings.HasPrefix(req.RoomName, "lk_") || strings.Contains(req.RoomName, "C++") {
		t.Errorf("token room = %q, want an opaque lk_<run_uuid> name (§8)", req.RoomName)
	}
	wantSources := []media.PublishSource{media.PublishScreenShare, media.PublishCamera, media.PublishMicrophone}
	if len(req.PublishSources) != len(wantSources) {
		t.Fatalf("publish sources = %v, want %v", req.PublishSources, wantSources)
	}
	for i := range wantSources {
		if req.PublishSources[i] != wantSources[i] {
			t.Fatalf("publish sources = %v, want %v", req.PublishSources, wantSources)
		}
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
	// And the lesson is untouched: the wall shows the authorized student — who has not
	// joined, so their tile is the null one — and nothing at all for the outsider, who
	// was refused before a session could exist.
	tiles := e.monitorOf(t, teacherCookies, classroomID)
	if len(tiles) != 1 {
		t.Fatalf("monitor shows %d tiles, want 1 (the authorized student)", len(tiles))
	}
	if _, ok := tiles[outsider.ID.String()]; ok {
		t.Error("a student whose join was refused appears on the wall")
	}
	assertNoSession(t, tiles[assigned.ID.String()])
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

// TestMonitorReportsTheCameraWithoutChangingTheStatus is the §51/§24 acceptance test: the
// camera flag of the wall comes from the media-plane observation, and an observed camera
// never moves the session out of CONNECTING — §21 makes the SCREEN the mandatory track, and
// a camera is not a substitute for supervision.
func TestMonitorReportsTheCameraWithoutChangingTheStatus(t *testing.T) {
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

	// --- 1. a camera on a student who is not sharing a screen --------------------------
	e.media.publish(identity, media.ParticipantTracks{Camera: true})
	tiles := e.monitorOf(t, teacherCookies, classroomID)
	tile := tiles[student.ID.String()]
	if camera, _ := tile["camera"].(map[string]any); camera["active"] != true {
		t.Fatalf("camera = %v, want active:true from the observation", tile["camera"])
	}
	if tile["sessionStatus"] != "CONNECTING" {
		t.Fatalf("sessionStatus = %v, want CONNECTING: a camera never produces ONLINE (§21/§24)", tile["sessionStatus"])
	}
	if screen, _ := tile["screen"].(map[string]any); screen["active"] != false {
		t.Errorf("screen = %v, want active:false", tile["screen"])
	}
	// The connection is still UNKNOWN: only a screen track makes the control plane claim
	// GOOD, because that is the only media it can attest to.
	if tile["connection"] != "UNKNOWN" {
		t.Errorf("connection = %v, want UNKNOWN", tile["connection"])
	}
	if stored := e.storedSession(t, sessionID); stored.Status != "CONNECTING" {
		t.Errorf("stored status = %v, want CONNECTING: the camera must not touch the row", stored.Status)
	}

	// --- 2. screen and camera together: ONLINE, with the camera reported ----------------
	e.media.publish(identity, media.ParticipantTracks{ScreenShare: true, Camera: true})
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	tile = tiles[student.ID.String()]
	if tile["sessionStatus"] != "ONLINE" {
		t.Fatalf("sessionStatus = %v, want ONLINE", tile["sessionStatus"])
	}
	if camera, _ := tile["camera"].(map[string]any); camera["active"] != true {
		t.Errorf("camera = %v, want active:true next to the screen", tile["camera"])
	}

	// --- 3. the camera goes away, the screen stays: ONLINE, camera false ----------------
	e.media.publish(identity, media.ParticipantTracks{ScreenShare: true})
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	tile = tiles[student.ID.String()]
	if camera, _ := tile["camera"].(map[string]any); camera["active"] != false {
		t.Errorf("camera = %v, want active:false after the camera was unpublished", tile["camera"])
	}
	if tile["sessionStatus"] != "ONLINE" {
		t.Errorf("sessionStatus = %v, want ONLINE: the screen is still shared", tile["sessionStatus"])
	}

	// --- 4. the participant is gone: the camera flag is not carried over ----------------
	e.media.disconnect(identity)
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	tile = tiles[student.ID.String()]
	if camera, _ := tile["camera"].(map[string]any); camera["active"] != false {
		t.Errorf("camera = %v, want active:false for an absent participant", tile["camera"])
	}
	if tile["sessionStatus"] != "DISCONNECTED" {
		t.Errorf("sessionStatus = %v, want DISCONNECTED", tile["sessionStatus"])
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

// ---------------------------------------------------------------------------
// The monitoring wall is the roster (§29/§73)
// ---------------------------------------------------------------------------

// TestMonitorListsTheWholeRoster is the Phase 7 contract end to end, against a real
// PostgreSQL: the wall lists every authorized student, not only the ones with a session
// row. A student who never joined is a tile whose session members are null (not a
// missing tile, and not an invented status), joining fills that tile in, and leaving
// turns it into LEFT — which is still a session, and still not null.
func TestMonitorListsTheWholeRoster(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	first, firstCookies := e.student(t)
	second, secondCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, first.ID, second.ID)

	classroomID := e.openClassroom(t, teacherCookies, first, second)

	// --- 1. the classroom is open and nobody has joined: two tiles, two null sessions ---
	tiles := e.monitorOf(t, teacherCookies, classroomID)
	if len(tiles) != 2 {
		t.Fatalf("monitor returned %d tiles, want the whole roster (2): %v", len(tiles), tiles)
	}
	for _, student := range []*user.User{first, second} {
		tile, ok := tiles[student.ID.String()]
		if !ok {
			t.Fatalf("student %s has no tile", student.Account)
		}
		assertNoSession(t, tile)
	}

	// The order is by ACCOUNT, not by who joined first: §29's grid must not reshuffle
	// between two polls while a teacher is looking at it.
	ordered := e.monitorOrder(t, teacherCookies, classroomID)
	accounts := []string{first.Account, second.Account}
	sort.Strings(accounts)
	for i, account := range accounts {
		idOf := first.ID
		if account == second.Account {
			idOf = second.ID
		}
		if ordered[i] != idOf.String() {
			t.Errorf("tile %d = %s, want the student with account %s", i, ordered[i], account)
		}
	}

	// --- 2. one student joins: that tile gets a session, the other stays null ---
	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", firstCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	joinedID, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}

	tiles = e.monitorOf(t, teacherCookies, classroomID)
	if len(tiles) != 2 {
		t.Fatalf("monitor returned %d tiles after one join, want 2", len(tiles))
	}
	joined := tiles[first.ID.String()]
	if joined["sessionId"] != joinedID.String() {
		t.Errorf("sessionId = %v, want %s", joined["sessionId"], joinedID)
	}
	// No participant was published to the fake media plane, so the honest state is
	// DISCONNECTED (Phase 6's transition), never ONLINE.
	if joined["sessionStatus"] != "DISCONNECTED" {
		t.Errorf("sessionStatus = %v, want DISCONNECTED (nobody was observed in the room)", joined["sessionStatus"])
	}
	assertNoSession(t, tiles[second.ID.String()])

	// --- 3. both joined: two sessions ---
	if rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", secondCookies); rec.Code != http.StatusOK {
		t.Fatalf("second join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	for _, student := range []*user.User{first, second} {
		if tiles[student.ID.String()]["sessionId"] == nil {
			t.Errorf("student %s still has a null session after joining", student.Account)
		}
	}

	// --- 4. one leaves: LEFT, not null ---
	rec = e.studentCall(t, http.MethodPost, "/api/v1/student/sessions/"+joinedID.String()+"/leave", "", firstCookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("leave: status = %d (%s)", rec.Code, rec.Body.String())
	}
	tiles = e.monitorOf(t, teacherCookies, classroomID)
	left := tiles[first.ID.String()]
	if left["sessionId"] == nil {
		t.Fatal("sessionId = null after leaving, want the LEFT session to stay visible")
	}
	if left["sessionStatus"] != "LEFT" {
		t.Errorf("sessionStatus = %v, want LEFT", left["sessionStatus"])
	}
	if stored := e.storedSession(t, joinedID); stored.Status != "LEFT" {
		t.Errorf("stored status = %v, want LEFT", stored.Status)
	}
}

// TestMonitorIsolatesStudentsInTheMediaPlane is the §26 half end to end: the monitor poll
// calls the media plane with the run's student identities, the teacher whitelisted, and
// logs what it revoked.
func TestMonitorIsolatesStudentsInTheMediaPlane(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	first, firstCookies := e.student(t)
	second, secondCookies := e.student(t)
	e.cleanupAccounts(t, teacher.ID, first.ID, second.ID)

	classroomID := e.openClassroom(t, teacherCookies, first, second)
	rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", firstCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	firstSession, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}
	if rec := e.studentCall(t, http.MethodPost, "/api/v1/student/classrooms/"+classroomID.String()+"/join", "", secondCookies); rec.Code != http.StatusOK {
		t.Fatalf("second join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	secondSession, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("second sessionId: %v", err)
	}

	// The teacher's media identity is their login session id (§44), which this control
	// plane mints but never stores in student_sessions — so it is exactly the identity the
	// whitelist has to recognise as "not a student".
	e.media.publish("teacher-login-session", media.ParticipantTracks{Microphone: true})
	e.media.publish(firstSession.String(), media.ParticipantTracks{ScreenShare: true})

	// The media plane reports one revocation, which the service must log with the fields
	// operations greps for.
	e.media.mu.Lock()
	e.media.revoked = []media.PeerSubscriptionRevocation{{
		ObserverIdentity:   secondSession.String(),
		TrackOwnerIdentity: firstSession.String(),
		TrackSid:           "TR_FIRST",
	}}
	e.media.mu.Unlock()

	e.monitorOf(t, teacherCookies, classroomID)

	e.media.mu.Lock()
	calls := append([]enforceCall{}, e.media.enforceCalls...)
	e.media.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("enforcement calls = %d, want one per observation", len(calls))
	}
	if len(calls[0].students) != 2 {
		t.Errorf("students = %v, want both session identities", calls[0].students)
	}
	found := false
	for _, allowed := range calls[0].allowed {
		if allowed == "teacher-login-session" {
			found = true
		}
	}
	if !found {
		t.Errorf("allowed = %v, want the teacher's identity whitelisted", calls[0].allowed)
	}
	for _, want := range []string{
		"action=peer_subscription_revoked",
		"observer_identity=" + secondSession.String(),
		"subscribed_track_owner=" + firstSession.String(),
		"track_sid=TR_FIRST",
	} {
		if !strings.Contains(e.logs.String(), want) {
			t.Errorf("the revocation log line is missing %q", want)
		}
	}
}

// TestMonitorKeepsDisabledStudentsOnTheWall: a student whose account was disabled is
// still on the roster — they were authorized for this course and may be re-enabled — so
// the wall keeps their tile. The DTO is frozen and carries no account-status member, so
// what this test pins is exactly the contract: the row stays, and it is the null tile
// unless they had already entered this lesson.
func TestMonitorKeepsDisabledStudentsOnTheWall(t *testing.T) {
	e := newMediaE2E(t)
	teacher, teacherCookies := e.staff(t)
	present, _ := e.student(t)
	disabled, _ := e.student(t)
	e.cleanupAccounts(t, teacher.ID, present.ID, disabled.ID)

	classroomID := e.openClassroom(t, teacherCookies, present, disabled)
	if err := e.users.SetStatus(context.Background(), disabled.ID, user.StatusDisabled); err != nil {
		t.Fatalf("disable student: %v", err)
	}
	// The disabled account cannot log in any more (§2.2), so it cannot produce a session.
	rec := e.call(t, http.MethodPost, "/api/v1/student/auth/login", `{"account":"`+disabled.Account+`"}`, nil, nil)
	if rec.Code == http.StatusOK {
		t.Error("a disabled account could still log in")
	}

	tiles := e.monitorOf(t, teacherCookies, classroomID)
	if len(tiles) != 2 {
		t.Fatalf("monitor returned %d tiles, want 2 (the whole roster)", len(tiles))
	}
	tile, ok := tiles[disabled.ID.String()]
	if !ok {
		t.Fatal("a disabled but authorized student disappeared from the wall")
	}
	if tile["displayName"] != disabled.DisplayName {
		t.Errorf("displayName = %v, want the roster's name", tile["displayName"])
	}
	assertNoSession(t, tile)
}
