package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/encoding/protojson"

	authservice "github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/realtime"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Phase 8 end to end: real PostgreSQL, real sessions, real signed webhooks, real
// WebSocket connections against the real router.
//
// The two fakes are the media plane (there is no LiveKit server in CI, and every
// interesting question in this phase is about how the control plane INTERPRETS an
// observation) and nothing else. In particular the webhook signature is REAL: the test
// signs the body exactly the way LiveKit does, with a throwaway key pair.
//
// Skipped when TEST_DATABASE_URL is unset.

// A key pair that exists only in this file. It is not a credential and is not a
// placeholder for one: the verifier is constructed with it explicitly, and the real key
// pair never appears outside configuration (§59).
const (
	testWebhookKey    = "APItestwebhookkey"
	testWebhookSecret = "test-webhook-secret-never-used-anywhere-else"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type runtimeE2E struct {
	router      *gin.Engine
	pool        *pgxpool.Pool
	users       *user.Postgres
	sessionRepo *session.Postgres
	media       *fakeMediaPlane
	hub         *realtime.Hub
	processor   *session.Processor
	cfg         *config.Config
	logs        *strings.Builder
}

func newRuntimeE2E(t *testing.T) *runtimeE2E {
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

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The runtime layer, wired exactly as cmd/api wires it (§47/§74), including Phase 10's
	// private-talk cycle: the session service reports its lifecycle into the processor, and
	// the processor reports "the target is gone" back into the private-talk state machine.
	hub := realtime.NewHub(realtime.HubConfig{Logger: logger})
	runtimeService := realtime.NewService(hub, realtime.NewAudience(pool), logger)
	processor := session.NewProcessor(sessionRepo, runtimeService)

	authService := authservice.NewService(users, sessionstore.New(pool), authservice.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    authservice.NewPasswordPolicy(cfg.PasswordMinLength),
	})
	classroomService := classroom.NewService(classroomRepo, users).
		WithRoomTerminator(terminator).
		WithRuntimeHooks(processor)
	sessionService := session.NewService(sessionRepo, classroomService, mediaPlane, session.Config{
		LiveKitURL: cfg.LiveKitURL,
		TokenTTL:   cfg.LiveKitTokenTTL,
	}).WithLifecycleEvents(processor).
		WithPrivateTalk(runtimeService, sessionRepo)
	processor.WithPrivateTalkEnder(sessionService)

	verifier, err := media.NewWebhookVerifier(testWebhookKey, testWebhookSecret)
	if err != nil {
		t.Fatalf("NewWebhookVerifier(): %v", err)
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Logger:      logger,
		Config:      cfg,
		Auth:        authService,
		Classroom:   classroomService,
		Session:     sessionService,
		PrivateTalk: sessionService,
		Webhook:     &httpapi.WebhookDeps{Verifier: verifier, Processor: processor},
		Socket:      hub,
		Limiter:     ratelimit.NewMemory(),
	})
	t.Cleanup(func() { hub.Shutdown(context.Background()) })

	return &runtimeE2E{
		router: router, pool: pool, users: users, sessionRepo: sessionRepo,
		media: mediaPlane, hub: hub, processor: processor, cfg: cfg, logs: logs,
	}
}

// signWebhook produces the Authorization header LiveKit sends: a short-lived JWT whose
// sha256 claim is the base64 SHA-256 of the exact body.
func signWebhook(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	token, err := auth.NewAccessToken(testWebhookKey, testWebhookSecret).
		SetValidFor(5 * time.Minute).
		SetSha256(base64.StdEncoding.EncodeToString(sum[:])).
		ToJWT()
	if err != nil {
		t.Fatalf("sign webhook: %v", err)
	}
	return token
}

// postWebhook delivers one webhook to the real endpoint. A nil signature sends the
// request UNSIGNED, which is what a forger would do.
func (e *runtimeE2E) postWebhook(t *testing.T, event *livekit.WebhookEvent, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := protojson.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/livekit/webhook", bytes.NewReader(body))
	// The content type LiveKit uses; it exists so a signature is checked before parsing.
	req.Header.Set("Content-Type", "application/webhook+json")
	if signed {
		req.Header.Set("Authorization", signWebhook(t, body))
	} else {
		req.Header.Set("Authorization", "not-a-real-token")
	}
	req.RemoteAddr = "203.0.113.9:44444"
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Accounts, classrooms and sessions (the same paths the Phase 6/7 suites use)
// ---------------------------------------------------------------------------

func (e *runtimeE2E) staff(t *testing.T) (*user.User, []*http.Cookie) {
	t.Helper()
	hash, err := authservice.Hash("a-teacher-passphrase")
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

func (e *runtimeE2E) student(t *testing.T, displayName string) (*user.User, []*http.Cookie) {
	t.Helper()
	created, err := e.users.Create(context.Background(), user.CreateParams{
		Account: dbtest.RandomAccount("student"), DisplayName: displayName, Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	return created, e.login(t, "student", created.Account, "")
}

func (e *runtimeE2E) login(t *testing.T, entry, account, password string) []*http.Cookie {
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

func (e *runtimeE2E) call(t *testing.T, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
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

func (e *runtimeE2E) teacherCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_teacher_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

func (e *runtimeE2E) studentCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		headers = csrfHeaders(cookies, "classwatch_session_student_csrf")
	}
	return e.call(t, method, path, body, cookies, headers)
}

// createClassroom creates a CLOSED classroom with a roster. It does not open it, which is
// what lets a test connect the sockets first and still observe ROOM_OPENED.
func (e *runtimeE2E) createClassroom(t *testing.T, teacherCookies []*http.Cookie, students ...*user.User) uuid.UUID {
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
	return classroomID
}

func (e *runtimeE2E) openClassroom(t *testing.T, classroomID uuid.UUID, teacherCookies []*http.Cookie) {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/open", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("open classroom: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func (e *runtimeE2E) closeClassroom(t *testing.T, classroomID uuid.UUID, teacherCookies []*http.Cookie) {
	t.Helper()
	rec := e.teacherCall(t, http.MethodPost, "/api/v1/teacher/classrooms/"+classroomID.String()+"/close", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("close classroom: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

// join makes the student enter the lesson and returns the session id.
func (e *runtimeE2E) join(t *testing.T, classroomID uuid.UUID, studentCookies []*http.Cookie) uuid.UUID {
	t.Helper()
	rec := e.studentCall(t, http.MethodPost,
		"/api/v1/student/classrooms/"+classroomID.String()+"/join", "", studentCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status = %d (%s)", rec.Code, rec.Body.String())
	}
	sessionID, err := uuid.Parse(jsonBody(t, rec)["sessionId"].(string))
	if err != nil {
		t.Fatalf("sessionId: %v", err)
	}
	return sessionID
}

// sessionState is one row of student_sessions as the database holds it.
type sessionState struct {
	Status          string
	Identity        string
	RunID           uuid.UUID
	ConnectedAt     *time.Time
	ScreenStartedAt *time.Time
	ScreenLostAt    *time.Time
	LeftAt          *time.Time
}

func (e *runtimeE2E) sessionState(t *testing.T, sessionID uuid.UUID) sessionState {
	t.Helper()
	var row sessionState
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, livekit_identity, classroom_run_id,
		       connected_at, screen_started_at, screen_lost_at, left_at
		  FROM student_sessions WHERE id = $1`, sessionID).
		Scan(&row.Status, &row.Identity, &row.RunID, &row.ConnectedAt, &row.ScreenStartedAt,
			&row.ScreenLostAt, &row.LeftAt); err != nil {
		t.Fatalf("read session %s: %v", sessionID, err)
	}
	return row
}

func (e *runtimeE2E) eventTypes(t *testing.T, sessionID uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT type FROM session_events WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read session_events: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		types = append(types, kind)
	}
	return types
}

func (e *runtimeE2E) roomNameOf(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var room string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT livekit_room_name FROM classroom_runs WHERE id = $1`, runID).Scan(&room); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return room
}

// cleanup removes the accounts and everything that references them, in the order the
// RESTRICT foreign keys allow. The event rows go with the sessions (ON DELETE CASCADE).
func (e *runtimeE2E) cleanupAccounts(t *testing.T, ownerID uuid.UUID, accounts ...uuid.UUID) {
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

// ---------------------------------------------------------------------------
// Webhook → state machine → event log
// ---------------------------------------------------------------------------

func TestSignedWebhookDrivesTheSessionStateMachineEndToEnd(t *testing.T) {
	e := newRuntimeE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t, "张三")
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.createClassroom(t, teacherCookies, student)
	e.openClassroom(t, classroomID, teacherCookies)
	sessionID := e.join(t, classroomID, studentCookies)

	before := e.sessionState(t, sessionID)
	if before.Status != "CONNECTING" {
		t.Fatalf("status after join = %s, want CONNECTING", before.Status)
	}
	if got := e.eventTypes(t, sessionID); len(got) != 1 || got[0] != "SESSION_CREATED" {
		t.Fatalf("events after join = %v, want [SESSION_CREATED]", got)
	}
	room := e.roomNameOf(t, before.RunID)

	// --- an unsigned (forged) webhook changes nothing --------------------------------
	forged := livekit.WebhookEvent{
		Event:       webhook.EventTrackPublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: before.Identity},
		Track:       &livekit.TrackInfo{Sid: "TR_forged", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	rec := e.postWebhook(t, &forged, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged webhook: status = %d, want 401", rec.Code)
	}
	if got := e.sessionState(t, sessionID).Status; got != "CONNECTING" {
		t.Fatalf("status = %s after a forged webhook: a forged event moved the state machine", got)
	}

	// --- participant_joined: recorded, but NOT online (§45) --------------------------
	joined := livekit.WebhookEvent{
		Event:       webhook.EventParticipantJoined,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: before.Identity, Sid: "PA_1"},
	}
	if rec := e.postWebhook(t, &joined, true); rec.Code != http.StatusOK {
		t.Fatalf("participant_joined: status = %d (%s)", rec.Code, rec.Body.String())
	}
	after := e.sessionState(t, sessionID)
	if after.Status != "CONNECTING" {
		t.Fatalf("status = %s, want CONNECTING: only a screen track makes a session ONLINE", after.Status)
	}
	if after.ConnectedAt == nil {
		t.Fatal("connected_at was not recorded")
	}

	// --- the screen appears: ONLINE ---------------------------------------------------
	published := livekit.WebhookEvent{
		Event:       webhook.EventTrackPublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: before.Identity, Sid: "PA_1"},
		Track:       &livekit.TrackInfo{Sid: "TR_screen", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if rec := e.postWebhook(t, &published, true); rec.Code != http.StatusOK {
			t.Fatalf("track_published #%d: status = %d (%s)", attempt, rec.Code, rec.Body.String())
		}
	}
	after = e.sessionState(t, sessionID)
	if after.Status != "ONLINE" || after.ScreenStartedAt == nil {
		t.Fatalf("status = %s screen_started_at = %v, want ONLINE with a timestamp", after.Status, after.ScreenStartedAt)
	}
	// The duplicate delivery must not have produced a second transition or a second row.
	want := []string{"SESSION_CREATED", "PARTICIPANT_CONNECTED", "SCREEN_PUBLISHED"}
	if got := e.eventTypes(t, sessionID); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v: at-least-once delivery must not duplicate history", got, want)
	}

	// --- the screen goes away ---------------------------------------------------------
	unpublished := livekit.WebhookEvent{
		Event:       webhook.EventTrackUnpublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: before.Identity},
		Track:       &livekit.TrackInfo{Sid: "TR_screen", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	if rec := e.postWebhook(t, &unpublished, true); rec.Code != http.StatusOK {
		t.Fatalf("track_unpublished: status = %d", rec.Code)
	}
	after = e.sessionState(t, sessionID)
	if after.Status != "SCREEN_LOST" || after.ScreenLostAt == nil {
		t.Fatalf("status = %s screen_lost_at = %v, want SCREEN_LOST", after.Status, after.ScreenLostAt)
	}

	// --- a webhook about an identity that is not a student session --------------------
	// The teacher's media identity is their login session id and is deliberately not in
	// student_sessions: the event is acknowledged (200) and writes nothing.
	stranger := livekit.WebhookEvent{
		Event:       webhook.EventTrackPublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: uuid.NewString()},
		Track:       &livekit.TrackInfo{Sid: "TR_teacher", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	if rec := e.postWebhook(t, &stranger, true); rec.Code != http.StatusOK {
		t.Fatalf("unknown identity: status = %d, want 200 (a 4xx would make LiveKit retry forever)", rec.Code)
	}
	if got := e.sessionState(t, sessionID).Status; got != "SCREEN_LOST" {
		t.Fatalf("status = %s: an unrelated identity moved this session", got)
	}

	// --- the teacher closes the classroom ---------------------------------------------
	e.closeClassroom(t, classroomID, teacherCookies)
	after = e.sessionState(t, sessionID)
	if after.Status != "ROOM_CLOSED" {
		t.Fatalf("status after close = %s, want ROOM_CLOSED (§49)", after.Status)
	}
	if got := e.eventTypes(t, sessionID); got[len(got)-1] != "ROOM_CLOSED" {
		t.Fatalf("events = %v, want ROOM_CLOSED to be the last one", got)
	}

	// --- the room_finished webhook that follows must change nothing -------------------
	finished := livekit.WebhookEvent{
		Event: webhook.EventRoomFinished,
		Id:    uuid.NewString(),
		Room:  &livekit.Room{Name: room},
	}
	if rec := e.postWebhook(t, &finished, true); rec.Code != http.StatusOK {
		t.Fatalf("room_finished: status = %d", rec.Code)
	}
	after = e.sessionState(t, sessionID)
	if after.Status != "ROOM_CLOSED" {
		t.Fatalf("status = %s, want ROOM_CLOSED: terminal states are never re-entered", after.Status)
	}
	if got := e.eventTypes(t, sessionID); countOf(got, "ROOM_CLOSED") != 1 {
		t.Fatalf("events = %v, want exactly one ROOM_CLOSED (the webhook retried a lesson that was already over)", got)
	}
}

// TestSignedCameraWebhookIsIdempotentAndNeverTouchesTheSession is the Phase 9 acceptance
// path over the real endpoint: the signature is verified, the camera lands in the event log
// exactly once per real change, and `student_sessions` does not move — not the status, not
// the timestamps.
func TestSignedCameraWebhookIsIdempotentAndNeverTouchesTheSession(t *testing.T) {
	e := newRuntimeE2E(t)
	teacher, teacherCookies := e.staff(t)
	student, studentCookies := e.student(t, "张三")
	e.cleanupAccounts(t, teacher.ID, student.ID)

	classroomID := e.createClassroom(t, teacherCookies, student)
	e.openClassroom(t, classroomID, teacherCookies)
	sessionID := e.join(t, classroomID, studentCookies)

	before := e.sessionState(t, sessionID)
	if before.Status != "CONNECTING" {
		t.Fatalf("status after join = %s, want CONNECTING", before.Status)
	}
	room := e.roomNameOf(t, before.RunID)

	camera := func(kind, sid string) *livekit.WebhookEvent {
		return &livekit.WebhookEvent{
			Event:       kind,
			Id:          uuid.NewString(),
			Room:        &livekit.Room{Name: room},
			Participant: &livekit.ParticipantInfo{Identity: before.Identity, Sid: "PA_1"},
			Track:       &livekit.TrackInfo{Sid: sid, Source: livekit.TrackSource_CAMERA, Type: livekit.TrackType_VIDEO},
		}
	}

	// --- the camera comes on, delivered twice -----------------------------------------
	for attempt := 1; attempt <= 2; attempt++ {
		if rec := e.postWebhook(t, camera(webhook.EventTrackPublished, "TR_cam"), true); rec.Code != http.StatusOK {
			t.Fatalf("camera publish #%d: status = %d (%s)", attempt, rec.Code, rec.Body.String())
		}
	}
	// --- and goes off, delivered twice ------------------------------------------------
	for attempt := 1; attempt <= 2; attempt++ {
		if rec := e.postWebhook(t, camera(webhook.EventTrackUnpublished, "TR_cam"), true); rec.Code != http.StatusOK {
			t.Fatalf("camera unpublish #%d: status = %d (%s)", attempt, rec.Code, rec.Body.String())
		}
	}

	after := e.sessionState(t, sessionID)
	if after.Status != "CONNECTING" {
		t.Fatalf("status = %s, want CONNECTING: a camera must not produce ONLINE (§21/§24)", after.Status)
	}
	if after.ConnectedAt != nil || after.ScreenStartedAt != nil || after.ScreenLostAt != nil {
		t.Fatalf("session = %+v, want every timestamp untouched by the camera", after)
	}
	want := []string{"SESSION_CREATED", "CAMERA_STARTED", "CAMERA_STOPPED"}
	if got := e.eventTypes(t, sessionID); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v: at-least-once delivery must not duplicate camera history", got, want)
	}

	// --- the payload of the stored row ------------------------------------------------
	var sid, source string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT payload->>'trackSid', payload->>'trackSource' FROM session_events
		 WHERE session_id = $1 AND type = 'CAMERA_STARTED'`, sessionID).Scan(&sid, &source); err != nil {
		t.Fatalf("read camera payload: %v", err)
	}
	if sid != "TR_cam" || source != "CAMERA" {
		t.Fatalf("payload = %q/%q, want the track sid and the CAMERA source", sid, source)
	}

	// --- a camera event that arrives after the lesson is over -------------------------
	// The classroom is closed, so every session of the run is ROOM_CLOSED (terminal). A
	// late camera publication is acknowledged (200) and writes nothing (§74).
	e.closeClassroom(t, classroomID, teacherCookies)
	if rec := e.postWebhook(t, camera(webhook.EventTrackPublished, "TR_cam_late"), true); rec.Code != http.StatusOK {
		t.Fatalf("late camera publish: status = %d, want 200 (a 4xx would make LiveKit retry forever)", rec.Code)
	}
	if got := e.sessionState(t, sessionID).Status; got != "ROOM_CLOSED" {
		t.Fatalf("status = %s, want ROOM_CLOSED", got)
	}
	want = append(want, "ROOM_CLOSED")
	if got := e.eventTypes(t, sessionID); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v: a terminal session records no camera", got, want)
	}
}

// ---------------------------------------------------------------------------
// WebSocket end to end
// ---------------------------------------------------------------------------
// wsClient is one WebSocket connection with a reader behind it.
type wsClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan []byte
	closed chan error
}

func (e *runtimeE2E) dial(t *testing.T, server *httptest.Server, path string, cookies []*http.Cookie) *wsClient {
	t.Helper()
	header := http.Header{}
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	header.Set("Cookie", strings.Join(parts, "; "))

	url := "ws" + strings.TrimPrefix(server.URL, "http") + path
	conn, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", path, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := &wsClient{t: t, conn: conn, frames: make(chan []byte, 32), closed: make(chan error, 1)}
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				client.closed <- err
				close(client.frames)
				return
			}
			client.frames <- data
		}
	}()
	return client
}

// waitFor reads until a message of the given type arrives, ignoring the others (the
// order of independent events is not part of the contract).
func (c *wsClient) waitFor(kind realtime.MessageType) realtime.Message {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case raw, open := <-c.frames:
			if !open {
				c.t.Fatalf("connection closed while waiting for %s", kind)
			}
			var message realtime.Message
			if err := json.Unmarshal(raw, &message); err != nil {
				c.t.Fatalf("message is not an envelope: %v (%s)", err, raw)
			}
			if message.Type == kind {
				return message
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

// expectSilence asserts that nothing at all arrives within the window. It is the §26
// assertion: a student's socket must not learn about a classmate.
func (c *wsClient) expectSilence(what string) {
	c.t.Helper()
	select {
	case raw, open := <-c.frames:
		if !open {
			return
		}
		c.t.Fatalf("%s: received %s, want silence", what, raw)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestWebSocketScopingEndToEnd(t *testing.T) {
	e := newRuntimeE2E(t)
	teacher, teacherCookies := e.staff(t)
	alice, aliceCookies := e.student(t, "张三")
	bob, bobCookies := e.student(t, "李四")
	e.cleanupAccounts(t, teacher.ID, alice.ID, bob.ID)

	// The classroom exists with its roster but is still CLOSED, so the sockets can be
	// connected BEFORE the open and the ROOM_OPENED broadcast has somewhere to go.
	classroomID := e.createClassroom(t, teacherCookies, alice, bob)

	server := httptest.NewServer(e.router)
	defer server.Close()

	aliceSocket := e.dial(t, server, "/ws/student", aliceCookies)
	bobSocket := e.dial(t, server, "/ws/student", bobCookies)
	teacherSocket := e.dial(t, server, "/ws/teacher", teacherCookies)

	// Wait for the server side to have registered all three: the client's Dial returns as
	// soon as the handshake is answered, which is a moment before registration.
	waitForCondition(t, "sockets registered", func() bool {
		return e.hub.Connections(realtime.RoleStudent, alice.ID) == 1 &&
			e.hub.Connections(realtime.RoleStudent, bob.ID) == 1 &&
			e.hub.Connections(realtime.RoleTeacher, teacher.ID) == 1
	})

	// --- PING/PONG over the real route -------------------------------------------------
	if err := aliceSocket.conn.WriteJSON(map[string]string{"type": "PING"}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if pong := aliceSocket.waitFor(realtime.TypePong); pong.Data == nil {
		t.Fatal("PONG carried a null data object")
	}

	// --- opening the lesson reaches BOTH authorized students and not the teacher -------
	e.openClassroom(t, classroomID, teacherCookies)

	openedRunID := ""
	for name, socket := range map[string]*wsClient{"alice": aliceSocket, "bob": bobSocket} {
		opened := socket.waitFor(realtime.TypeRoomOpened)
		if opened.Data["classroomId"] != classroomID.String() {
			t.Fatalf("%s: classroomId = %v", name, opened.Data["classroomId"])
		}
		if opened.Data["classroomName"] != "C++ 算法训练" {
			t.Fatalf("%s: classroomName = %v", name, opened.Data["classroomName"])
		}
		runID, _ := opened.Data["runId"].(string)
		if runID == "" {
			t.Fatalf("%s: ROOM_OPENED without a runId", name)
		}
		if openedRunID == "" {
			openedRunID = runID
		} else if runID != openedRunID {
			t.Fatalf("the two students were told about different runs: %s / %s", openedRunID, runID)
		}
	}
	teacherSocket.expectSilence("ROOM_OPENED is for the students whose dashboard changes")

	// --- a student joins and starts sharing: the teacher is told, the classmate is not --
	sessionID := e.join(t, classroomID, aliceCookies)
	state := e.sessionState(t, sessionID)
	room := e.roomNameOf(t, state.RunID)

	published := &livekit.WebhookEvent{
		Event:       webhook.EventTrackPublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: state.Identity},
		Track:       &livekit.TrackInfo{Sid: "TR_screen", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	if rec := e.postWebhook(t, published, true); rec.Code != http.StatusOK {
		t.Fatalf("track_published: status = %d", rec.Code)
	}

	online := teacherSocket.waitFor(realtime.TypeStudentOnline)
	if online.Data["studentId"] != alice.ID.String() ||
		online.Data["displayName"] != "张三" ||
		online.Data["sessionId"] != sessionID.String() {
		t.Fatalf("STUDENT_ONLINE data = %+v", online.Data)
	}
	bobSocket.expectSilence("a classmate going online")
	aliceSocket.expectSilence("another student's arrival is not her business")

	// --- the screen is lost: the teacher AND that one student --------------------------
	unpublished := &livekit.WebhookEvent{
		Event:       webhook.EventTrackUnpublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: state.Identity},
		Track:       &livekit.TrackInfo{Sid: "TR_screen", Source: livekit.TrackSource_SCREEN_SHARE},
	}
	if rec := e.postWebhook(t, unpublished, true); rec.Code != http.StatusOK {
		t.Fatalf("track_unpublished: status = %d", rec.Code)
	}

	teacherCopy := teacherSocket.waitFor(realtime.TypeScreenLost)
	if teacherCopy.Data["studentId"] != alice.ID.String() ||
		teacherCopy.Data["sessionId"] != sessionID.String() {
		t.Fatalf("teacher SCREEN_LOST data = %+v", teacherCopy.Data)
	}

	// The student's own copy says nothing about anybody: not her name, not the teacher,
	// and above all not a classmate (§26).
	studentCopy := aliceSocket.waitFor(realtime.TypeScreenLost)
	if studentCopy.Data["sessionId"] != sessionID.String() {
		t.Fatalf("student SCREEN_LOST data = %+v", studentCopy.Data)
	}
	if _, leaked := studentCopy.Data["studentId"]; leaked {
		t.Fatalf("the student's own message carries a studentId: %+v", studentCopy.Data)
	}
	if _, leaked := studentCopy.Data["displayName"]; leaked {
		t.Fatalf("the student's own message carries a display name: %+v", studentCopy.Data)
	}

	// THE assertion of this phase's §26 rule: the other student's socket received
	// nothing at all about Alice's screen.
	bobSocket.expectSilence("another student's screen state")

	// --- the camera (§24/§75): the owner is told, and NOBODY else ----------------------
	cameraPublished := &livekit.WebhookEvent{
		Event:       webhook.EventTrackPublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: state.Identity, Sid: "PA_1"},
		Track:       &livekit.TrackInfo{Sid: "TR_cam", Source: livekit.TrackSource_CAMERA, Type: livekit.TrackType_VIDEO},
	}
	if rec := e.postWebhook(t, cameraPublished, true); rec.Code != http.StatusOK {
		t.Fatalf("camera track_published: status = %d (%s)", rec.Code, rec.Body.String())
	}

	cameraOn := teacherSocket.waitFor(realtime.TypeCameraChanged)
	if cameraOn.Data["studentId"] != alice.ID.String() ||
		cameraOn.Data["sessionId"] != sessionID.String() ||
		cameraOn.Data["active"] != true {
		t.Fatalf("CAMERA_CHANGED data = %+v", cameraOn.Data)
	}
	// A camera is not the supervision: the session is still SCREEN_LOST (§21/§24).
	if got := e.sessionState(t, sessionID).Status; got != "SCREEN_LOST" {
		t.Fatalf("status = %s after a camera was published, want SCREEN_LOST: a camera never restores ONLINE", got)
	}
	// §26: neither the classmate nor the student herself is told about a camera. For the
	// student this is also a design choice, not an oversight — see Service.CameraChanged.
	bobSocket.expectSilence("a classmate's camera")
	aliceSocket.expectSilence("her own camera is her own UI, not a server message")

	if rec := e.postWebhook(t, &livekit.WebhookEvent{
		Event:       webhook.EventTrackUnpublished,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: state.Identity},
		Track:       &livekit.TrackInfo{Sid: "TR_cam", Source: livekit.TrackSource_CAMERA},
	}, true); rec.Code != http.StatusOK {
		t.Fatalf("camera track_unpublished: status = %d", rec.Code)
	}
	cameraOff := teacherSocket.waitFor(realtime.TypeCameraChanged)
	if cameraOff.Data["active"] != false || cameraOff.Data["sessionId"] != sessionID.String() {
		t.Fatalf("CAMERA_CHANGED after the camera stopped = %+v", cameraOff.Data)
	}
	bobSocket.expectSilence("a classmate's camera being switched off")

	// The event log holds exactly one CAMERA_STARTED and one CAMERA_STOPPED, and the
	// session row still says SCREEN_LOST with no connected_at change.
	cameraEvents := 0
	for _, kind := range e.eventTypes(t, sessionID) {
		if kind == "CAMERA_STARTED" || kind == "CAMERA_STOPPED" {
			cameraEvents++
		}
	}
	if cameraEvents != 2 {
		t.Fatalf("camera events = %d, want 2: %v", cameraEvents, e.eventTypes(t, sessionID))
	}
	if got := e.sessionState(t, sessionID); got.ScreenStartedAt == nil || got.Status != "SCREEN_LOST" {
		t.Fatalf("session = %+v, want an unchanged SCREEN_LOST row", got)
	}

	// --- closing the lesson reaches the students and the owner -------------------------
	e.closeClassroom(t, classroomID, teacherCookies)
	for name, socket := range map[string]*wsClient{
		"alice": aliceSocket, "bob": bobSocket, "teacher": teacherSocket,
	} {
		closed := socket.waitFor(realtime.TypeRoomClosed)
		if closed.Data["classroomId"] != classroomID.String() {
			t.Fatalf("%s: ROOM_CLOSED data = %+v", name, closed.Data)
		}
		if closed.Data["runId"] != openedRunID {
			t.Fatalf("%s: runId = %v, want %s", name, closed.Data["runId"], openedRunID)
		}
	}
}

func TestWebSocketHandshakeWithoutASessionClosesWith4401(t *testing.T) {
	e := newRuntimeE2E(t)
	server := httptest.NewServer(e.router)
	defer server.Close()

	// A browser cannot read the HTTP status of a failed upgrade, so the server ACCEPTS the
	// handshake and closes with 4401. The socket is never registered and never receives a
	// message; the only thing it carries is the reason.
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d), want an accepted upgrade followed by a 4401 close", err, status)
	}
	defer conn.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	if !websocket.IsCloseError(readErr, realtime.CloseSessionInvalid) {
		t.Fatalf("close error = %v, want close code %d", readErr, realtime.CloseSessionInvalid)
	}
}

func TestSocketPathAnswersAPlainGETWithTheErrorEnvelope(t *testing.T) {
	// The close-code behaviour is for handshakes. An ops probe (or a health check) that
	// simply GETs the path gets the same envelope as every other route, which is what
	// keeps the endpoint debuggable from curl.
	e := newRuntimeE2E(t)
	server := httptest.NewServer(e.router)
	defer server.Close()

	resp, err := http.Get(server.URL + "/ws/student")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("body is not the error envelope: %v", err)
	}
	if envelope.Error.Code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", envelope.Error.Code)
	}
}

func TestWebSocketHandshakeRefusesTheWrongEntryPoint(t *testing.T) {
	e := newRuntimeE2E(t)
	teacher, teacherCookies := e.staff(t)
	e.cleanupAccounts(t, teacher.ID)

	server := httptest.NewServer(e.router)
	defer server.Close()

	// A teacher's cookie presented to the student socket: authenticated, wrong entry.
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	header := http.Header{}
	parts := make([]string, 0, len(teacherCookies))
	for _, c := range teacherCookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	header.Set("Cookie", strings.Join(parts, "; "))

	conn, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	defer conn.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, readErr := conn.ReadMessage(); !websocket.IsCloseError(readErr, realtime.CloseRoleForbidden) {
		t.Fatalf("close error = %v, want close code %d", readErr, realtime.CloseRoleForbidden)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func countOf(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
