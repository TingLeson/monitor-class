package session

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// The §31 private-talk state machine against in-memory fakes.
//
// WHAT these tests are about: the three-layer rule of Phase 10 — a process-local state
// machine, a media-plane subscription decision, and an audit row — and the four ways it can
// be wrong: two students hearing the teacher, nobody hearing the teacher, a restart leaving
// a ghost subscription, and a classmate learning that a talk exists.
//
// The media fake models the SUBSCRIPTION SET (see fakeMedia.EnforcePrivateTalk) rather than
// only recording calls, because the product question is "who can hear the teacher?", and a
// test that counts RPCs can pass while the answer is wrong.
//
// The end-to-end version (real PostgreSQL, real event rows, close triggering a revocation)
// is in privatetalk_integration_test.go.

// talkHarness is the shared harness with a teacher who has a microphone and two students
// who are both in the room.
type talkHarness struct {
	*harness
	// teacherSession is the teacher's LOGIN session id, which is their participant identity
	// in the media room (§44).
	teacherSession uuid.UUID
	studentSession *StudentSession
	otherSession   *StudentSession
}

// teacherMicTrack is the sid of the teacher's microphone publication in every test.
const teacherMicTrack = "TR_T_MIC"

func newTalkHarness(t *testing.T) *talkHarness {
	t.Helper()
	h := newHarness(t)
	th := &talkHarness{harness: h, teacherSession: uuid.New()}

	h.addStudent(h.otherID, "S10087", "学生 B")
	// Both students are ONLINE with a screen, so they are "online" in the sense §31 asks
	// about and only the microphone differs between the cases below.
	th.studentSession = h.repo.seed(h.runID, h.studentID, StatusOnline, "学生 A")
	th.otherSession = h.repo.seed(h.runID, h.otherID, StatusOnline, "学生 B")

	th.publishTeacherMic(true)
	th.publishStudent(th.studentSession, false)
	th.publishStudent(th.otherSession, false)
	return th
}

// publishTeacherMic puts (or removes) the teacher's microphone publication in the room.
func (th *talkHarness) publishTeacherMic(on bool) {
	identity := th.teacherSession.String()
	if !on {
		th.media.observed[identity] = media.ParticipantTracks{ParticipantSid: "PA_teacher"}
		return
	}
	th.media.observed[identity] = media.ParticipantTracks{
		ParticipantSid: "PA_teacher",
		Microphone:     true,
		Tracks:         []media.ObservedTrack{{Sid: teacherMicTrack, Source: media.PublishMicrophone}},
	}
}

// publishStudent puts one student in the room, optionally with their microphone open.
func (th *talkHarness) publishStudent(session *StudentSession, mic bool) {
	tracks := []media.ObservedTrack{{Sid: "TR_" + session.LiveKitIdentity + "_SCREEN", Source: media.PublishScreenShare}}
	if mic {
		tracks = append(tracks, media.ObservedTrack{Sid: "TR_" + session.LiveKitIdentity + "_MIC", Source: media.PublishMicrophone})
	}
	th.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{
		ParticipantSid: "PA_" + session.LiveKitIdentity,
		ScreenShare:    true,
		Microphone:     mic,
		Tracks:         tracks,
	}
}

// start issues the POST for one student through the service, as the teacher.
func (th *talkHarness) start(t *testing.T, studentID uuid.UUID) (*PrivateTalkView, error) {
	t.Helper()
	return th.service.StartPrivateTalk(context.Background(), StartPrivateTalkInput{
		ClassroomID:        th.classroomID,
		TeacherID:          th.teacherID,
		TeacherSessionID:   th.teacherSession,
		TeacherDisplayName: "王老师",
		StudentID:          studentID,
	})
}

// mustStart fails the test when the POST was refused.
func (th *talkHarness) mustStart(t *testing.T, studentID uuid.UUID) *PrivateTalkView {
	t.Helper()
	view, err := th.start(t, studentID)
	if err != nil {
		t.Fatalf("StartPrivateTalk(%s): %v", studentID, err)
	}
	return view
}

// stop issues the DELETE, as the teacher.
func (th *talkHarness) stop(t *testing.T) error {
	t.Helper()
	return th.service.StopPrivateTalk(context.Background(), StopPrivateTalkInput{
		ClassroomID:      th.classroomID,
		TeacherID:        th.teacherID,
		TeacherSessionID: th.teacherSession,
	})
}

// current reads the state the read endpoint would answer with.
func (th *talkHarness) current(t *testing.T) *PrivateTalkView {
	t.Helper()
	view, err := th.service.PrivateTalk(context.Background(), th.classroomID, th.teacherID)
	if err != nil {
		t.Fatalf("PrivateTalk(): %v", err)
	}
	return view
}

// canHear reports whether one student's media connection is subscribed to the teacher's
// microphone.
func (th *talkHarness) canHear(session *StudentSession) bool {
	return th.media.canHearTeacher(session.LiveKitIdentity, teacherMicTrack)
}

// ---------------------------------------------------------------------------
// IDLE → TALKING
// ---------------------------------------------------------------------------

// TestStartPrivateTalkSubscribesOnlyTheTarget is the Phase 10 acceptance path: one POST
// grants exactly one student, revokes everybody else, writes TEACHER_TALK_STARTED on that
// student's session, and tells the student and the teacher.
func TestStartPrivateTalkSubscribesOnlyTheTarget(t *testing.T) {
	h := newTalkHarness(t)

	view := h.mustStart(t, h.studentID)

	if view.Target == nil {
		t.Fatal("no target in the response")
	}
	if view.Target.StudentID != h.studentID || view.Target.SessionID != h.studentSession.ID {
		t.Fatalf("target = %+v, want student %s / session %s", view.Target, h.studentID, h.studentSession.ID)
	}
	if view.Target.DisplayName != "学生 A" {
		t.Fatalf("display name = %q, want the roster's", view.Target.DisplayName)
	}

	// §31: exactly one student can hear the teacher.
	if !h.canHear(h.studentSession) {
		t.Fatal("the target is not subscribed to the teacher's microphone")
	}
	if h.canHear(h.otherSession) {
		t.Fatal("another student is subscribed to the teacher's microphone: §31 forbids it")
	}

	// §13: the audit row belongs to the TARGET's session.
	if got := h.audit.eventsOf(h.studentSession.ID); len(got) != 1 || got[0] != EventTeacherTalkStarted {
		t.Fatalf("target events = %v, want [TEACHER_TALK_STARTED]", got)
	}
	if got := h.audit.eventsOf(h.otherSession.ID); len(got) != 0 {
		t.Fatalf("the other student's session grew talk rows: %v", got)
	}
	if payload := h.audit.rows[0].payload; payload["studentId"] != h.studentID.String() ||
		payload["runId"] != h.runID.String() || payload["teacherId"] != h.teacherID.String() {
		t.Fatalf("payload = %+v, want identifiers only", payload)
	}

	// §47: the student and the teacher are told, and the REQUEST follows because the
	// student's microphone is off (§25).
	if len(h.talk.started) != 1 || h.talk.started[0].ref.SessionID != h.studentSession.ID {
		t.Fatalf("PRIVATE_TALK_STARTED = %+v, want the target's session", h.talk.started)
	}
	if h.talk.started[0].teacherDisplayName != "王老师" {
		t.Fatalf("teacher display name = %q, want the authenticated one", h.talk.started[0].teacherDisplayName)
	}
	if len(h.talk.requested) != 1 || h.talk.requested[0].ref.SessionID != h.studentSession.ID {
		t.Fatalf("PRIVATE_TALK_REQUEST = %+v, want one for the target (§25: the mic is off)", h.talk.requested)
	}
	if len(h.talk.ended) != 0 {
		t.Fatalf("PRIVATE_TALK_ENDED = %+v, want none", h.talk.ended)
	}

	// The read endpoint agrees with what just happened.
	if current := h.current(t); current.Target == nil || current.Target.StudentID != h.studentID {
		t.Fatalf("GET = %+v, want the target", current)
	}
}

// TestStartPrivateTalkSkipsTheRequestWhenTheMicrophoneIsAlreadyOn: §25's dialog asks the
// student to open something they have already opened.
func TestStartPrivateTalkSkipsTheRequestWhenTheMicrophoneIsAlreadyOn(t *testing.T) {
	h := newTalkHarness(t)
	h.publishStudent(h.studentSession, true)

	h.mustStart(t, h.studentID)

	if len(h.talk.started) != 1 {
		t.Fatalf("PRIVATE_TALK_STARTED = %+v, want one", h.talk.started)
	}
	if len(h.talk.requested) != 0 {
		t.Fatalf("PRIVATE_TALK_REQUEST = %+v, want none: the student is already publishing", h.talk.requested)
	}
}

// TestStartPrivateTalkTwiceIsIdempotent: the same student twice is one talk. A double click
// must not produce a second audit row, a second message, or a second "please open your
// microphone" dialog.
func TestStartPrivateTalkTwiceIsIdempotent(t *testing.T) {
	h := newTalkHarness(t)

	h.mustStart(t, h.studentID)
	h.mustStart(t, h.studentID)

	want := []EventType{EventTeacherTalkStarted}
	got := h.audit.eventsOf(h.studentSession.ID)
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("target events = %v, want %v", got, want)
	}
	if len(h.talk.started) != 1 || len(h.talk.requested) != 1 || len(h.talk.ended) != 0 {
		t.Fatalf("messages = started %d, requested %d, ended %d; want 1/1/0",
			len(h.talk.started), len(h.talk.requested), len(h.talk.ended))
	}
	if !h.canHear(h.studentSession) || h.canHear(h.otherSession) {
		t.Fatal("the media state drifted across an idempotent repeat")
	}
}

// TestStartPrivateTalkSwitchesTarget is §31's "张三 → 结束 → 李四": the old student is
// revoked IN THE SAME PASS as the new one is granted, the audit log shows ENDED then
// STARTED, and there is no moment in which both can hear the teacher.
func TestStartPrivateTalkSwitchesTarget(t *testing.T) {
	h := newTalkHarness(t)

	h.mustStart(t, h.studentID)
	if !h.canHear(h.studentSession) {
		t.Fatal("the first target was never subscribed")
	}
	h.talk.started, h.talk.requested, h.talk.ended = nil, nil, nil

	h.mustStart(t, h.otherID)

	if h.canHear(h.studentSession) {
		t.Fatal("the OLD target is still subscribed after the switch (§31: 不得出现两个学生同时听到)")
	}
	if !h.canHear(h.otherSession) {
		t.Fatal("the NEW target is not subscribed after the switch")
	}
	wantOld := []EventType{EventTeacherTalkStarted, EventTeacherTalkEnded}
	if got := h.audit.eventsOf(h.studentSession.ID); len(got) != len(wantOld) || got[1] != EventTeacherTalkEnded {
		t.Fatalf("old target events = %v, want %v", got, wantOld)
	}
	if payload := h.audit.rows[len(h.audit.rows)-2].payload; payload["reason"] != TalkEndReasonSwitched {
		t.Fatalf("switch end payload = %+v, want reason SWITCHED", payload)
	}
	if got := h.audit.eventsOf(h.otherSession.ID); len(got) != 1 || got[0] != EventTeacherTalkStarted {
		t.Fatalf("new target events = %v, want [TEACHER_TALK_STARTED]", got)
	}

	if len(h.talk.ended) != 1 || h.talk.ended[0].SessionID != h.studentSession.ID {
		t.Fatalf("PRIVATE_TALK_ENDED = %+v, want the old target", h.talk.ended)
	}
	if len(h.talk.started) != 1 || h.talk.started[0].ref.SessionID != h.otherSession.ID {
		t.Fatalf("PRIVATE_TALK_STARTED = %+v, want the new target", h.talk.started)
	}
	if current := h.current(t); current.Target == nil || current.Target.StudentID != h.otherID {
		t.Fatalf("GET = %+v, want the new target", current)
	}
}

// ---------------------------------------------------------------------------
// TALKING → IDLE
// ---------------------------------------------------------------------------

// TestStopPrivateTalkEndsAndIsIdempotent: DELETE ends the talk, revokes the target and
// tells both audiences — and a second DELETE is a silent success, not a second event.
func TestStopPrivateTalkEndsAndIsIdempotent(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)

	if err := h.stop(t); err != nil {
		t.Fatalf("StopPrivateTalk(): %v", err)
	}

	if h.canHear(h.studentSession) {
		t.Fatal("the target is still subscribed after the stop")
	}
	want := []EventType{EventTeacherTalkStarted, EventTeacherTalkEnded}
	got := h.audit.eventsOf(h.studentSession.ID)
	if len(got) != len(want) || got[1] != EventTeacherTalkEnded {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if payload := h.audit.rows[len(h.audit.rows)-1].payload; payload["reason"] != TalkEndReasonStopped {
		t.Fatalf("stop payload = %+v, want reason STOPPED", payload)
	}
	if len(h.talk.ended) != 1 || h.talk.ended[0].SessionID != h.studentSession.ID {
		t.Fatalf("PRIVATE_TALK_ENDED = %+v, want the target", h.talk.ended)
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE", current)
	}

	// Idempotent: nothing left to end.
	before := len(h.audit.rows)
	if err := h.stop(t); err != nil {
		t.Fatalf("second StopPrivateTalk(): %v", err)
	}
	if len(h.audit.rows) != before || len(h.talk.ended) != 1 {
		t.Fatalf("a second stop wrote history: rows %d→%d, ended %d", before, len(h.audit.rows), len(h.talk.ended))
	}
}

// TestStopPrivateTalkWithoutATalkIsASilentSuccess: the teardown path must not fail when
// there is nothing to tear down.
func TestStopPrivateTalkWithoutATalkIsASilentSuccess(t *testing.T) {
	h := newTalkHarness(t)

	if err := h.stop(t); err != nil {
		t.Fatalf("StopPrivateTalk(): %v", err)
	}
	if len(h.audit.rows) != 0 || len(h.talk.ended) != 0 {
		t.Fatalf("rows = %+v, messages = %+v, want none", h.audit.rows, h.talk.ended)
	}
}

// TestStopPrivateTalkOnAClosedClassroomIsStillASuccess: the teacher may close the lesson and
// then have the console send its teardown call. The client must not be shown an error for an
// end state that already holds.
func TestStopPrivateTalkOnAClosedClassroomIsStillASuccess(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)

	// The lesson ends: the classroom row is CLOSED and has no current run.
	h.directory.classrooms[h.classroomID].Status = classroom.StatusClosed
	h.directory.classrooms[h.classroomID].CurrentRun = nil
	h.directory.classrooms[h.classroomID].CurrentRunID = nil

	if err := h.stop(t); err != nil {
		t.Fatalf("StopPrivateTalk() on a closed classroom: %v", err)
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE", current)
	}
}

// ---------------------------------------------------------------------------
// Refusals (§31)
// ---------------------------------------------------------------------------

// TestStartPrivateTalkRequiresTheTeachersMicrophone is the refusal the whole phase turns
// on: without a teacher microphone track there is nothing a student could be subscribed to,
// and a 200 would tell the teacher they are talking while nobody can hear them.
func TestStartPrivateTalkRequiresTheTeachersMicrophone(t *testing.T) {
	h := newTalkHarness(t)
	h.publishTeacherMic(false)

	_, err := h.start(t, h.studentID)
	if !errors.Is(err, ErrTeacherMicRequired) {
		t.Fatalf("error = %v, want ErrTeacherMicRequired", err)
	}
	// Not a silent success: no state, no rows, no messages, no media call.
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE", current)
	}
	if len(h.audit.rows) != 0 || len(h.talk.started) != 0 || len(h.media.talkCalls) != 0 {
		t.Fatalf("a refused start wrote something: rows=%+v started=%+v media=%+v",
			h.audit.rows, h.talk.started, h.media.talkCalls)
	}
}

// TestStartPrivateTalkRejectsAnOfflineTarget covers the three shapes of "not online": a
// student who never joined, one whose session is over, and one whose session is active but
// who is not in the media room.
func TestStartPrivateTalkRejectsAnOfflineTarget(t *testing.T) {
	t.Run("never joined", func(t *testing.T) {
		h := newTalkHarness(t)
		h.repo.sessions = map[uuid.UUID]*StudentSession{}
		if _, err := h.start(t, h.studentID); !errors.Is(err, ErrPrivateTalkUnavailable) {
			t.Fatalf("error = %v, want ErrPrivateTalkUnavailable", err)
		}
	})

	t.Run("session is over", func(t *testing.T) {
		h := newTalkHarness(t)
		h.studentSession.Status = StatusLeft
		if _, err := h.start(t, h.studentID); !errors.Is(err, ErrPrivateTalkUnavailable) {
			t.Fatalf("error = %v, want ErrPrivateTalkUnavailable", err)
		}
	})

	t.Run("active session but not in the room", func(t *testing.T) {
		h := newTalkHarness(t)
		delete(h.media.observed, h.studentSession.LiveKitIdentity)
		if _, err := h.start(t, h.studentID); !errors.Is(err, ErrPrivateTalkUnavailable) {
			t.Fatalf("error = %v, want ErrPrivateTalkUnavailable", err)
		}
	})
}

// TestStartPrivateTalkRejectsAStudentWhoIsNotOnTheRoster: naming a student this classroom
// does not authorize is 404 STUDENT_NOT_ASSIGNED, the same answer the join path gives.
func TestStartPrivateTalkRejectsAStudentWhoIsNotOnTheRoster(t *testing.T) {
	h := newTalkHarness(t)

	stranger := uuid.New()
	_, err := h.start(t, stranger)
	if !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Fatalf("error = %v, want classroom.ErrStudentNotAssigned", err)
	}
}

// TestStartPrivateTalkRejectsAnotherTeacherAndAClosedClassroom pins the two authorization
// answers §58 separates: "not yours" (403) and "not running" (409).
func TestStartPrivateTalkRejectsAnotherTeacherAndAClosedClassroom(t *testing.T) {
	t.Run("another teacher", func(t *testing.T) {
		h := newTalkHarness(t)
		_, err := h.service.StartPrivateTalk(context.Background(), StartPrivateTalkInput{
			ClassroomID:      h.classroomID,
			TeacherID:        uuid.New(),
			TeacherSessionID: uuid.New(),
			StudentID:        h.studentID,
		})
		if !errors.Is(err, classroom.ErrNotOwner) {
			t.Fatalf("error = %v, want classroom.ErrNotOwner", err)
		}
	})

	t.Run("closed classroom", func(t *testing.T) {
		h := newTalkHarness(t)
		h.directory.classrooms[h.classroomID].Status = classroom.StatusClosed
		h.directory.classrooms[h.classroomID].CurrentRun = nil
		h.directory.classrooms[h.classroomID].CurrentRunID = nil
		if _, err := h.start(t, h.studentID); !errors.Is(err, ErrClassroomClosed) {
			t.Fatalf("error = %v, want ErrClassroomClosed", err)
		}
	})
}

// TestStartPrivateTalkMakesNoPromiseItCannotKeep: when the media plane refuses the
// subscription, the POST fails and the state stays IDLE. The alternative — a 200 with a
// target the room does not agree with — is the failure this whole phase exists to prevent.
func TestStartPrivateTalkMakesNoPromiseItCannotKeep(t *testing.T) {
	h := newTalkHarness(t)
	h.media.talkErr = errors.New("livekit: unavailable")

	_, err := h.start(t, h.studentID)
	if !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("error = %v, want ErrMediaUnavailable", err)
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE: a refused start must not become state", current)
	}
	if len(h.audit.rows) != 0 || len(h.talk.started) != 0 {
		t.Fatalf("a refused start wrote history: rows=%+v started=%+v", h.audit.rows, h.talk.started)
	}
}

// TestStartPrivateTalkFailsWhenTheRoomCannotBeObserved: "the media plane did not answer"
// must not be answered as TEACHER_MIC_REQUIRED — that would tell the teacher to check a
// button that may be perfectly fine.
func TestStartPrivateTalkFailsWhenTheRoomCannotBeObserved(t *testing.T) {
	h := newTalkHarness(t)
	h.media.observeErr = errors.New("livekit: unreachable")

	_, err := h.start(t, h.studentID)
	if !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("error = %v, want ErrMediaUnavailable", err)
	}
	if errors.Is(err, ErrTeacherMicRequired) {
		t.Fatal("an observation failure was reported as a missing microphone")
	}
}

// ---------------------------------------------------------------------------
// Revocation triggers (§31)
// ---------------------------------------------------------------------------

// TestEndPrivateTalkForSessionRevokesAndEnds is the path a webhook or the leave endpoint
// takes: the target is gone, so the subscription goes and both audiences are told.
func TestEndPrivateTalkForSessionRevokesAndEnds(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)
	h.talk.started, h.talk.requested = nil, nil

	if err := h.service.EndPrivateTalkForSession(context.Background(), h.studentSession.ID, TalkEndReasonLeft); err != nil {
		t.Fatalf("EndPrivateTalkForSession(): %v", err)
	}

	if h.canHear(h.studentSession) {
		t.Fatal("the departed target is still subscribed to the teacher's microphone")
	}
	if got := h.audit.eventsOf(h.studentSession.ID); len(got) != 2 || got[1] != EventTeacherTalkEnded {
		t.Fatalf("events = %v, want [TEACHER_TALK_STARTED TEACHER_TALK_ENDED]", got)
	}
	if payload := h.audit.rows[len(h.audit.rows)-1].payload; payload["reason"] != TalkEndReasonLeft {
		t.Fatalf("end payload = %+v, want reason LEFT", payload)
	}
	if len(h.talk.ended) != 1 {
		t.Fatalf("PRIVATE_TALK_ENDED = %+v, want one", h.talk.ended)
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE", current)
	}

	// Idempotent: a second delivery of the same departure finds nothing to end.
	if err := h.service.EndPrivateTalkForSession(context.Background(), h.studentSession.ID, TalkEndReasonLeft); err != nil {
		t.Fatalf("second EndPrivateTalkForSession(): %v", err)
	}
	if len(h.talk.ended) != 1 || len(h.audit.rows) != 2 {
		t.Fatalf("a repeated departure wrote history: ended=%d rows=%d", len(h.talk.ended), len(h.audit.rows))
	}
}

// TestEndPrivateTalkForRunEndsTheLesson is the close path: one run, one classroom, one talk.
func TestEndPrivateTalkForRunEndsTheLesson(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)

	if err := h.service.EndPrivateTalkForRun(context.Background(), h.runID, TalkEndReasonRoomClosed); err != nil {
		t.Fatalf("EndPrivateTalkForRun(): %v", err)
	}

	if h.canHear(h.studentSession) {
		t.Fatal("the target is still subscribed after the lesson ended")
	}
	if len(h.talk.ended) != 1 {
		t.Fatalf("PRIVATE_TALK_ENDED = %+v, want one", h.talk.ended)
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE", current)
	}

	// The same call for a run with no talk is a silent success.
	if err := h.service.EndPrivateTalkForRun(context.Background(), uuid.New(), TalkEndReasonRoomClosed); err != nil {
		t.Fatalf("unknown run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Convergence (§31: no ghost subscriptions)
// ---------------------------------------------------------------------------

// TestMonitorConvergesOnTheCurrentTarget: the monitor is the reconciliation loop that makes
// a process-local state machine safe. With a live target it keeps exactly that student
// subscribed and everybody else revoked — the "everybody else" half is what repairs a
// switch that failed halfway, or a client that re-subscribed on its own.
func TestMonitorConvergesOnTheCurrentTarget(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)

	// A misbehaving (or merely reloaded) client subscribed the wrong student.
	h.media.subscriptions[h.otherSession.LiveKitIdentity] = map[string]bool{teacherMicTrack: true}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}

	if !h.canHear(h.studentSession) {
		t.Fatal("the target lost their subscription to a monitor poll")
	}
	if h.canHear(h.otherSession) {
		t.Fatal("the monitor left a classmate subscribed to the teacher's microphone: §31")
	}
}

// TestMonitorWithNoTargetUnsubscribesEveryone is the restart property: the registry is
// empty (or was never populated in this process), the media plane still delivers the
// teacher's audio to whoever was selected before, and the next poll must stop it.
func TestMonitorWithNoTargetUnsubscribesEveryone(t *testing.T) {
	h := newTalkHarness(t)

	// The state a previous process left behind: both students subscribed.
	for _, session := range []*StudentSession{h.studentSession, h.otherSession} {
		h.media.subscriptions[session.LiveKitIdentity] = map[string]bool{teacherMicTrack: true}
	}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}

	if h.canHear(h.studentSession) || h.canHear(h.otherSession) {
		t.Fatalf("a ghost subscription survived the monitor: %+v", h.media.subscriptions)
	}
	// The convergence call names no target and the teacher, which is how it finds the
	// microphone to revoke.
	if len(h.media.talkCalls) == 0 {
		t.Fatal("the monitor never asked the media plane to converge")
	}
	last := h.media.talkCalls[len(h.media.talkCalls)-1]
	if last.target != "" {
		t.Fatalf("convergence target = %q, want empty (IDLE)", last.target)
	}
	if len(last.teachers) != 1 || last.teachers[0] != h.teacherSession.String() {
		t.Fatalf("teacher identities = %v, want the observed non-student participant", last.teachers)
	}
}

// TestMonitorDropsATargetWhoseSessionIsOver: if nothing told the state machine that the talk
// ended (a missed webhook, a restart mid-lesson), the monitor notices, stops the audio and
// stops reporting the talk.
func TestMonitorDropsATargetWhoseSessionIsOver(t *testing.T) {
	h := newTalkHarness(t)
	h.mustStart(t, h.studentID)

	h.studentSession.Status = StatusLeft

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}

	if h.canHear(h.studentSession) {
		t.Fatal("a target whose session is over is still subscribed to the teacher's microphone")
	}
	if current := h.current(t); current.Target != nil {
		t.Fatalf("GET = %+v, want IDLE after the target's session ended", current)
	}
}

// TestPrivateTalkForAnotherTeacherIsRefusedOnTheReadPath: the read and the stop must not
// become an oracle for "does this classroom exist and is it mine?".
func TestPrivateTalkForAnotherTeacherIsRefusedOnTheReadPath(t *testing.T) {
	h := newTalkHarness(t)

	if _, err := h.service.PrivateTalk(context.Background(), h.classroomID, uuid.New()); !errors.Is(err, classroom.ErrNotOwner) {
		t.Fatalf("error = %v, want classroom.ErrNotOwner", err)
	}
	if err := h.service.StopPrivateTalk(context.Background(), StopPrivateTalkInput{
		ClassroomID: h.classroomID, TeacherID: uuid.New(), TeacherSessionID: uuid.New(),
	}); !errors.Is(err, classroom.ErrNotOwner) {
		t.Fatalf("stop error = %v, want classroom.ErrNotOwner", err)
	}
}

// TestPrivateTalkSurvivesABrokenBroadcast: the audience layer failing must not fail the
// teacher's action, and must not lose the state — the media plane already agrees with it.
func TestPrivateTalkSurvivesABrokenBroadcast(t *testing.T) {
	h := newTalkHarness(t)
	h.talk.err = errors.New("hub is not connected")

	// No error reaches the caller: the message is a courtesy, the media plane is the truth.
	if _, err := h.start(t, h.studentID); err != nil {
		t.Fatalf("StartPrivateTalk(): %v", err)
	}
	if !h.canHear(h.studentSession) {
		t.Fatal("the subscription was not applied")
	}
	if current := h.current(t); current.Target == nil {
		t.Fatal("the state was lost because a message could not be sent")
	}
}
