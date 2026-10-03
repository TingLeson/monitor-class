package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The three §31 endpoints end to end: the real router, the real middleware chain, real
// accounts and a real classroom roster in PostgreSQL, a real session repository and real
// event rows — and the fake media plane (there is no LiveKit server in CI, and the
// interesting questions are about what the control plane ASKS the media plane to do).
//
// WHAT this covers that the unit tests cannot: that the ownership, roster and classroom-state
// rules are the ones the REAL classroom domain answers, and that the media-subscription
// decision lands on the fake's subscription book — the same end state a real SFU reaches
// after UpdateSubscriptions.
//
// Skipped when TEST_DATABASE_URL is unset.

// privateTalkSetup is a classroom with an open run, two joined students, and a teacher whose
// microphone is published.
type privateTalkSetup struct {
	*runtimeE2E
	teacher         *user.User
	teacherCookies  []*http.Cookie
	strangerCookies []*http.Cookie
	studentA        *user.User
	studentACookies []*http.Cookie
	studentB        *user.User
	studentBCookies []*http.Cookie
	classroomID     uuid.UUID
	studentASession uuid.UUID
	studentBSession uuid.UUID
	teacherIdentity string
	teacherMicTrack string
}

func newPrivateTalkSetup(t *testing.T) *privateTalkSetup {
	t.Helper()
	e := newRuntimeE2E(t)
	ctx := context.Background()

	teacher, teacherCookies := e.staff(t)
	_, strangerCookies := e.staff(t)
	studentA, studentACookies := e.student(t, "张三")
	studentB, studentBCookies := e.student(t, "李四")
	e.cleanupAccounts(t, teacher.ID, studentA.ID, studentB.ID)

	classroomID := e.createClassroom(t, teacherCookies, studentA, studentB)
	e.openClassroom(t, classroomID, teacherCookies)
	studentASession := e.join(t, classroomID, studentACookies)
	studentBSession := e.join(t, classroomID, studentBCookies)

	// The teacher's media identity is their LOGIN session id (§44); the fake records it when
	// the console asks for its token. The students' identities are their session ids, which
	// is what the join path mints.
	rec := e.teacherCall(t, http.MethodPost,
		"/api/v1/teacher/classrooms/"+classroomID.String()+"/media-token", "", teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("media token: status = %d (%s)", rec.Code, rec.Body.String())
	}
	teacherIdentity := e.media.lastTokenRequest(t).Identity
	teacherMicTrack := "TR_T_MIC"
	e.media.publish(teacherIdentity, media.ParticipantTracks{
		ParticipantSid: "PA_teacher",
		Microphone:     true,
		Tracks:         []media.ObservedTrack{{Sid: teacherMicTrack, Source: media.PublishMicrophone}},
	})
	// Both students are in the room, publishing a screen (which is what the roster read asks
	// about, and what makes them "online" for §31).
	for _, sessionID := range []uuid.UUID{studentASession, studentBSession} {
		var identity string
		if err := e.pool.QueryRow(ctx,
			`SELECT livekit_identity FROM student_sessions WHERE id = $1`, sessionID).Scan(&identity); err != nil {
			t.Fatalf("read session identity: %v", err)
		}
		e.media.publish(identity, media.ParticipantTracks{
			ParticipantSid: "PA_" + identity,
			ScreenShare:    true,
			Tracks:         []media.ObservedTrack{{Sid: "TR_" + identity + "_SCREEN", Source: media.PublishScreenShare}},
		})
	}

	return &privateTalkSetup{
		runtimeE2E: e, teacher: teacher, teacherCookies: teacherCookies, strangerCookies: strangerCookies,
		studentA: studentA, studentACookies: studentACookies,
		studentB: studentB, studentBCookies: studentBCookies,
		classroomID: classroomID, studentASession: studentASession, studentBSession: studentBSession,
		teacherIdentity: teacherIdentity, teacherMicTrack: teacherMicTrack,
	}
}

// path is the classroom's private-talk URL.
func (s *privateTalkSetup) path() string {
	return "/api/v1/teacher/classrooms/" + s.classroomID.String() + "/private-talk"
}

// start issues the POST for one student, as one teacher.
func (s *privateTalkSetup) start(t *testing.T, studentID uuid.UUID, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return s.teacherCall(t, http.MethodPost, s.path(), `{"studentId":"`+studentID.String()+`"}`, cookies)
}

// identityOf reads one session's media identity.
func (s *privateTalkSetup) identityOf(t *testing.T, sessionID uuid.UUID) string {
	t.Helper()
	var identity string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT livekit_identity FROM student_sessions WHERE id = $1`, sessionID).Scan(&identity); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return identity
}

// runIDOf reads the classroom's current run.
func (s *privateTalkSetup) runIDOf(t *testing.T) uuid.UUID {
	t.Helper()
	var runID uuid.UUID
	if err := s.pool.QueryRow(context.Background(),
		`SELECT current_run_id FROM classrooms WHERE id = $1`, s.classroomID).Scan(&runID); err != nil {
		t.Fatalf("read current run: %v", err)
	}
	return runID
}

// talkEventTypes lists the TEACHER_TALK_* rows of one session, oldest first.
func (s *privateTalkSetup) talkEventTypes(t *testing.T, sessionID uuid.UUID) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
		SELECT type FROM session_events
		 WHERE session_id = $1 AND type IN ('TEACHER_TALK_STARTED', 'TEACHER_TALK_ENDED')
		 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read talk events: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan talk event: %v", err)
		}
		types = append(types, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate talk events: %v", err)
	}
	return types
}

// targetOf decodes the frozen `{"target": … | null}` body.
func targetOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := jsonBody(t, rec)
	raw, ok := body["target"]
	if !ok {
		t.Fatalf("body has no target member: %s", rec.Body.String())
	}
	if raw == nil {
		return nil
	}
	target, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("target is %T, want an object or null", raw)
	}
	return target
}

// ---------------------------------------------------------------------------
// The acceptance path
// ---------------------------------------------------------------------------

// TestPrivateTalkEndToEnd is §31 through the real HTTP surface: start, read, switch, stop —
// with the media subscription state and the audit rows asserted after each step.
func TestPrivateTalkEndToEnd(t *testing.T) {
	s := newPrivateTalkSetup(t)
	identityA := s.identityOf(t, s.studentASession)
	identityB := s.identityOf(t, s.studentBSession)

	// --- start with 张三 -------------------------------------------------------------
	rec := s.start(t, s.studentA.ID, s.teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: status = %d (%s)", rec.Code, rec.Body.String())
	}
	target := targetOf(t, rec)
	if target["studentId"] != s.studentA.ID.String() || target["sessionId"] != s.studentASession.String() ||
		target["displayName"] != "张三" {
		t.Fatalf("target = %v", target)
	}
	if !s.media.subscribedTo(identityA, s.teacherMicTrack) {
		t.Fatal("the selected student is not subscribed to the teacher's microphone")
	}
	if s.media.subscribedTo(identityB, s.teacherMicTrack) {
		t.Fatal("another student is subscribed to the teacher's microphone: §31 forbids it")
	}
	if got := s.talkEventTypes(t, s.studentASession); len(got) != 1 || got[0] != "TEACHER_TALK_STARTED" {
		t.Fatalf("talk events = %v, want [TEACHER_TALK_STARTED]", got)
	}

	// --- read ----------------------------------------------------------------------
	rec = s.teacherCall(t, http.MethodGet, s.path(), "", s.teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := targetOf(t, rec); got["studentId"] != s.studentA.ID.String() {
		t.Fatalf("read target = %v", got)
	}

	// --- switch to 李四 -------------------------------------------------------------
	rec = s.start(t, s.studentB.ID, s.teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !s.media.subscribedTo(identityB, s.teacherMicTrack) {
		t.Fatal("the new target is not subscribed to the teacher's microphone")
	}
	if s.media.subscribedTo(identityA, s.teacherMicTrack) {
		t.Fatal("the old target is still subscribed after the switch: §31 不得出现两个学生同时听到")
	}
	if got := s.talkEventTypes(t, s.studentASession); len(got) != 2 || got[1] != "TEACHER_TALK_ENDED" {
		t.Fatalf("old target events = %v, want STARTED then ENDED", got)
	}

	// --- stop ----------------------------------------------------------------------
	rec = s.teacherCall(t, http.MethodDelete, s.path(), "", s.teacherCookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if s.media.subscribedTo(identityA, s.teacherMicTrack) || s.media.subscribedTo(identityB, s.teacherMicTrack) {
		t.Fatal("a student is still subscribed after the stop")
	}
	rec = s.teacherCall(t, http.MethodGet, s.path(), "", s.teacherCookies)
	if got := targetOf(t, rec); got != nil {
		t.Fatalf("read after stop = %v, want null", got)
	}
}

// ---------------------------------------------------------------------------
// Refusals, on the real authorization path
// ---------------------------------------------------------------------------

// TestPrivateTalkRejectsAStudentWhoIsNotOnTheRoster: STUDENT_NOT_ASSIGNED is decided by the
// real roster read, not by a fake.
func TestPrivateTalkRejectsAStudentWhoIsNotOnTheRoster(t *testing.T) {
	s := newPrivateTalkSetup(t)

	stranger, _ := s.student(t, "陌生人")
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger.ID); err != nil {
			t.Logf("cleanup: stranger account: %v", err)
		}
	})

	rec := s.start(t, stranger.ID, s.teacherCookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "STUDENT_NOT_ASSIGNED" {
		t.Fatalf("code = %s, want STUDENT_NOT_ASSIGNED", code)
	}
}

// TestPrivateTalkRejectsAnotherTeacher: ownership is a server-side rule on the real row, so a
// different logged-in teacher is refused even though the route group only checks the role.
func TestPrivateTalkRejectsAnotherTeacher(t *testing.T) {
	s := newPrivateTalkSetup(t)

	rec := s.start(t, s.studentA.ID, s.strangerCookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("start: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "CLASSROOM_NOT_OWNER" {
		t.Fatalf("start: code = %s, want CLASSROOM_NOT_OWNER", code)
	}

	rec = s.teacherCall(t, http.MethodGet, s.path(), "", s.strangerCookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "CLASSROOM_NOT_OWNER" {
		t.Fatalf("read: code = %s, want CLASSROOM_NOT_OWNER", code)
	}
}

// TestPrivateTalkRejectsAClosedClassroom: a talk cannot start outside an open lesson. The
// read and the stop of a closed classroom still answer "nobody is being talked to" — the
// close already ended any talk, and the console's teardown call must not error.
func TestPrivateTalkRejectsAClosedClassroom(t *testing.T) {
	s := newPrivateTalkSetup(t)
	s.closeClassroom(t, s.classroomID, s.teacherCookies)

	rec := s.start(t, s.studentA.ID, s.teacherCookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("start: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "CLASSROOM_CLOSED" {
		t.Fatalf("start: code = %s, want CLASSROOM_CLOSED", code)
	}

	rec = s.teacherCall(t, http.MethodGet, s.path(), "", s.teacherCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := targetOf(t, rec); got != nil {
		t.Fatalf("read = %v, want null", got)
	}
	rec = s.teacherCall(t, http.MethodDelete, s.path(), "", s.teacherCookies)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop: status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// TestPrivateTalkRequiresTheTeacherMicrophoneEndToEnd is §31's most important refusal: a
// teacher who has not published a microphone must be told, not silently "connected".
func TestPrivateTalkRequiresTheTeacherMicrophoneEndToEnd(t *testing.T) {
	s := newPrivateTalkSetup(t)
	// The teacher's microphone is gone (they never opened it, or they stopped it).
	s.media.publish(s.teacherIdentity, media.ParticipantTracks{ParticipantSid: "PA_teacher"})

	rec := s.start(t, s.studentA.ID, s.teacherCookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "TEACHER_MIC_REQUIRED" {
		t.Fatalf("code = %s, want TEACHER_MIC_REQUIRED", code)
	}
	// Nothing was granted, and the read still says IDLE.
	if s.media.subscribedTo(s.identityOf(t, s.studentASession), s.teacherMicTrack) {
		t.Fatal("a refused start left a subscription behind")
	}
	rec = s.teacherCall(t, http.MethodGet, s.path(), "", s.teacherCookies)
	if got := targetOf(t, rec); got != nil {
		t.Fatalf("read = %v, want null", got)
	}
}

// ---------------------------------------------------------------------------
// A trigger with no request behind it
// ---------------------------------------------------------------------------

// TestPrivateTalkEndsWhenTheTargetLeavesEndToEnd is the webhook path through the whole HTTP
// surface: a SIGNED participant_left ends the talk, revokes the subscription and writes the
// audit row — with no teacher request involved.
func TestPrivateTalkEndsWhenTheTargetLeavesEndToEnd(t *testing.T) {
	s := newPrivateTalkSetup(t)
	identityA := s.identityOf(t, s.studentASession)

	if rec := s.start(t, s.studentA.ID, s.teacherCookies); rec.Code != http.StatusOK {
		t.Fatalf("start: status = %d (%s)", rec.Code, rec.Body.String())
	}

	left := livekit.WebhookEvent{
		Event:       webhook.EventParticipantLeft,
		Id:          uuid.NewString(),
		Room:        &livekit.Room{Name: s.roomNameOf(t, s.runIDOf(t))},
		Participant: &livekit.ParticipantInfo{Identity: identityA, Sid: "PA_target"},
	}
	if rec := s.postWebhook(t, &left, true); rec.Code != http.StatusOK {
		t.Fatalf("webhook: status = %d (%s)", rec.Code, rec.Body.String())
	}

	if s.media.subscribedTo(identityA, s.teacherMicTrack) {
		t.Fatal("the departed target is still subscribed to the teacher's microphone")
	}
	if events := s.talkEventTypes(t, s.studentASession); len(events) != 2 || events[1] != "TEACHER_TALK_ENDED" {
		t.Fatalf("talk events = %v, want STARTED then ENDED", events)
	}
	if got := targetOf(t, s.teacherCall(t, http.MethodGet, s.path(), "", s.teacherCookies)); got != nil {
		t.Fatalf("read = %v, want null", got)
	}
}
