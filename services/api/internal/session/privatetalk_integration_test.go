package session_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/media"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Phase 10 against a real PostgreSQL server (§13/§25/§31/§76).
//
// WHAT only the database can prove here:
//
//   - the MIC_* and TEACHER_TALK_* rows really satisfy the CHECK constraint of 0008 and
//     really land with the payload shape the code claims (jsonb, identifiers only);
//   - AppendEvent is an UNCONDITIONAL append — the private-talk rows must be repeatable in a
//     way RecordEventOnce would silently swallow (talk to 张三, stop, talk again);
//   - the close path (Processor.CloseRunSessions) ends a talk and revokes the subscription in
//     the same run, which is the §31 trigger that has no request behind it.
//
// Skipped when TEST_DATABASE_URL is unset, so `go test ./...` stays green without a server.

// ---------------------------------------------------------------------------
// A media plane that remembers who is subscribed (§31)
// ---------------------------------------------------------------------------

// talkMediaPlane is the media half of these tests: it serves a room observation the test
// scripts and applies subscription changes to a book, so the assertion can be about the
// product rule ("who can hear the teacher?") rather than about RPC counts.
type talkMediaPlane struct {
	mu            sync.Mutex
	observed      map[string]media.ParticipantTracks
	subscriptions map[string]map[string]bool
	talkCalls     int
	observeErr    error
	talkErr       error
}

func newTalkMediaPlane() *talkMediaPlane {
	return &talkMediaPlane{
		observed:      map[string]media.ParticipantTracks{},
		subscriptions: map[string]map[string]bool{},
	}
}

func (m *talkMediaPlane) EnsureRoom(context.Context, string) error { return nil }

func (m *talkMediaPlane) ObserveRoom(context.Context, string) (map[string]media.ParticipantTracks, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.observeErr != nil {
		return nil, m.observeErr
	}
	out := make(map[string]media.ParticipantTracks, len(m.observed))
	for identity, tracks := range m.observed {
		out[identity] = tracks
	}
	return out, nil
}

func (m *talkMediaPlane) RemoveParticipant(context.Context, string, string) error { return nil }

func (m *talkMediaPlane) SignToken(media.TokenRequest) (string, error) { return "fake-token", nil }

func (m *talkMediaPlane) EnforceNoPeerSubscriptions(
	context.Context, string, []string, map[string]media.ParticipantTracks, []string,
) ([]media.PeerSubscriptionRevocation, error) {
	// §26 is not the subject of these tests; the media-layer reconciler has its own tests.
	return nil, nil
}

func (m *talkMediaPlane) EnforcePrivateTalk(
	_ context.Context,
	_ string,
	students []string,
	observed map[string]media.ParticipantTracks,
	teacherIdentities []string,
	targetIdentity string,
) (media.PrivateTalkEnforcement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.talkCalls++
	if m.talkErr != nil {
		return media.PrivateTalkEnforcement{}, m.talkErr
	}

	var micSids []string
	for _, identity := range teacherIdentities {
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
			if m.subscriptions[student] == nil {
				m.subscriptions[student] = map[string]bool{}
			}
			if m.subscriptions[student][sid] == subscribe {
				continue
			}
			m.subscriptions[student][sid] = subscribe
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

// publish puts one participant and its tracks in the room.
func (m *talkMediaPlane) publish(identity string, tracks ...media.ObservedTrack) {
	m.mu.Lock()
	defer m.mu.Unlock()
	observed := media.ParticipantTracks{ParticipantSid: "PA_" + identity, Tracks: tracks}
	for _, track := range tracks {
		switch track.Source {
		case media.PublishScreenShare:
			observed.ScreenShare = true
		case media.PublishCamera:
			observed.Camera = true
		case media.PublishMicrophone:
			observed.Microphone = true
		}
	}
	m.observed[identity] = observed
}

// canHear reports whether one participant's connection is subscribed to one track.
func (m *talkMediaPlane) canHear(identity, trackSid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscriptions[identity][trackSid]
}

// talkRecordingEvents is the message path of these tests: it records what was addressed to
// whom, without a hub.
type talkRecordingEvents struct {
	started   []uuid.UUID
	requested []uuid.UUID
	ended     []uuid.UUID
}

func (e *talkRecordingEvents) PrivateTalkStarted(_ context.Context, ref session.SessionRef, _ string) error {
	e.started = append(e.started, ref.SessionID)
	return nil
}

func (e *talkRecordingEvents) PrivateTalkRequested(_ context.Context, ref session.SessionRef, _ string) error {
	e.requested = append(e.requested, ref.SessionID)
	return nil
}

func (e *talkRecordingEvents) PrivateTalkEnded(_ context.Context, ref session.SessionRef) error {
	e.ended = append(e.ended, ref.SessionID)
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// talkFixture is the shared fixture plus a wired private-talk service.
type talkFixture struct {
	*fixture
	service *session.Service
	media   *talkMediaPlane
	events  *talkRecordingEvents
	// teacherSession is the teacher's participant identity (§44). It is the login session
	// id in production; here it is any UUID, because the media plane is a book.
	teacherSession  uuid.UUID
	teacherMic      string
	studentASession *session.StudentSession
	studentBSession *session.StudentSession
}

func newTalkFixture(t *testing.T) *talkFixture {
	t.Helper()
	f := newFixture(t)
	users := user.NewPostgres(f.pool)
	classrooms := classroom.NewService(classroom.NewPostgres(f.pool), users)
	mediaPlane := newTalkMediaPlane()
	events := &talkRecordingEvents{}

	service := session.NewService(f.repo, classrooms, mediaPlane, session.Config{
		LiveKitURL: "wss://media.example.test", TokenTTL: 0,
	}).WithPrivateTalk(events, f.repo)

	tf := &talkFixture{
		fixture: f, service: service, media: mediaPlane, events: events,
		teacherSession:  uuid.New(),
		teacherMic:      "TR_T_MIC",
		studentASession: f.create(t, f.studentA.ID),
		studentBSession: f.create(t, f.studentB.ID),
	}
	// The room: the teacher with a microphone, both students present.
	mediaPlane.publish(tf.teacherSession.String(), media.ObservedTrack{Sid: tf.teacherMic, Source: media.PublishMicrophone})
	mediaPlane.publish(tf.studentASession.LiveKitIdentity, media.ObservedTrack{Sid: "TR_A_SCREEN", Source: media.PublishScreenShare})
	mediaPlane.publish(tf.studentBSession.LiveKitIdentity, media.ObservedTrack{Sid: "TR_B_SCREEN", Source: media.PublishScreenShare})
	return tf
}

// start begins a talk with one student, as the owner teacher.
func (tf *talkFixture) start(t *testing.T, studentID uuid.UUID) {
	t.Helper()
	if _, err := tf.service.StartPrivateTalk(context.Background(), session.StartPrivateTalkInput{
		ClassroomID:        tf.classroomID,
		TeacherID:          tf.teacher.ID,
		TeacherSessionID:   tf.teacherSession,
		TeacherDisplayName: tf.teacher.DisplayName,
		StudentID:          studentID,
	}); err != nil {
		t.Fatalf("StartPrivateTalk(%s): %v", studentID, err)
	}
}

// stop ends the talk, as the owner teacher.
func (tf *talkFixture) stop(t *testing.T) {
	t.Helper()
	if err := tf.service.StopPrivateTalk(context.Background(), session.StopPrivateTalkInput{
		ClassroomID:      tf.classroomID,
		TeacherID:        tf.teacher.ID,
		TeacherSessionID: tf.teacherSession,
	}); err != nil {
		t.Fatalf("StopPrivateTalk(): %v", err)
	}
}

// talkEventRows reads one session's talk rows, oldest first, with their payloads.
func talkEventRows(t *testing.T, f *fixture, sessionID uuid.UUID) []struct {
	Type    string
	Payload map[string]any
} {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT type, payload FROM session_events
		 WHERE session_id = $1 AND type IN ('TEACHER_TALK_STARTED', 'TEACHER_TALK_ENDED')
		 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read talk rows: %v", err)
	}
	defer rows.Close()

	var out []struct {
		Type    string
		Payload map[string]any
	}
	for rows.Next() {
		var (
			kind    string
			payload map[string]any
		)
		if err := rows.Scan(&kind, &payload); err != nil {
			t.Fatalf("scan talk row: %v", err)
		}
		out = append(out, struct {
			Type    string
			Payload map[string]any
		}{Type: kind, Payload: payload})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate talk rows: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Event rows and their payloads
// ---------------------------------------------------------------------------

// TestPrivateTalkStartAndStopWriteRealEventRows is the §13/§31 audit contract: a start writes
// TEACHER_TALK_STARTED on the TARGET's session, a stop writes TEACHER_TALK_ENDED, the two are
// separate rows (not a guarded "once per session"), and the payload is identifiers only.
func TestPrivateTalkStartAndStopWriteRealEventRows(t *testing.T) {
	tf := newTalkFixture(t)

	tf.start(t, tf.studentA.ID)
	tf.stop(t)
	// A second talk with the same student: the private-talk rows must NOT be deduplicated —
	// "once per (session, type)" would erase this one, and it is a real event.
	tf.start(t, tf.studentA.ID)

	rows := talkEventRows(t, tf.fixture, tf.studentASession.ID)
	want := []string{"TEACHER_TALK_STARTED", "TEACHER_TALK_ENDED", "TEACHER_TALK_STARTED"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %v", rows, want)
	}
	for i := range want {
		if rows[i].Type != want[i] {
			t.Fatalf("rows = %+v, want %v", rows, want)
		}
	}
	if reason := rows[1].Payload["reason"]; reason != session.TalkEndReasonStopped {
		t.Fatalf("end reason = %v, want STOPPED", reason)
	}
	// The payload shape every talk row shares: opaque identifiers, queryable as jsonb.
	for _, row := range rows {
		if row.Payload["runId"] != tf.runID.String() {
			t.Fatalf("payload = %+v, want runId %s", row.Payload, tf.runID)
		}
		if row.Payload["studentId"] != tf.studentA.ID.String() {
			t.Fatalf("payload = %+v, want studentId %s", row.Payload, tf.studentA.ID)
		}
		for _, forbidden := range []string{"displayName", "account", "audio", "transcript"} {
			if _, leaked := row.Payload[forbidden]; leaked {
				t.Fatalf("payload carries %q: %+v", forbidden, row.Payload)
			}
		}
	}

	// The other student's session grew nothing: a talk is a fact about ONE student (§26).
	if rows := talkEventRows(t, tf.fixture, tf.studentBSession.ID); len(rows) != 0 {
		t.Fatalf("the classmate's session has talk rows: %+v", rows)
	}
}

// TestPrivateTalkSwitchWritesEndThenStart is §31's switching path as history: the old
// target's ENDED row (with its reason) comes before the new target's STARTED row.
func TestPrivateTalkSwitchWritesEndThenStart(t *testing.T) {
	tf := newTalkFixture(t)

	tf.start(t, tf.studentA.ID)
	tf.start(t, tf.studentB.ID)

	aRows := talkEventRows(t, tf.fixture, tf.studentASession.ID)
	if len(aRows) != 2 || aRows[0].Type != "TEACHER_TALK_STARTED" || aRows[1].Type != "TEACHER_TALK_ENDED" {
		t.Fatalf("old target rows = %+v, want STARTED then ENDED", aRows)
	}
	if reason := aRows[1].Payload["reason"]; reason != session.TalkEndReasonSwitched {
		t.Fatalf("end reason = %v, want SWITCHED", reason)
	}
	bRows := talkEventRows(t, tf.fixture, tf.studentBSession.ID)
	if len(bRows) != 1 || bRows[0].Type != "TEACHER_TALK_STARTED" {
		t.Fatalf("new target rows = %+v, want [TEACHER_TALK_STARTED]", bRows)
	}
	if !tf.media.canHear(tf.studentBSession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the new target cannot hear the teacher")
	}
	if tf.media.canHear(tf.studentASession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the old target can still hear the teacher: §31 forbids two simultaneous targets")
	}
}

// TestPrivateTalkEventRowsSatisfyTheVocabularyCheck is a small but real database assertion:
// the two types are in the CHECK constraint of 0008, so the write above could not have
// succeeded against a schema that had not been migrated.
func TestPrivateTalkEventRowsSatisfyTheVocabularyCheck(t *testing.T) {
	tf := newTalkFixture(t)
	tf.start(t, tf.studentA.ID)

	var count int
	if err := tf.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM session_events
		 WHERE session_id = $1 AND type IN ('MIC_STARTED','MIC_STOPPED','TEACHER_TALK_STARTED','TEACHER_TALK_ENDED')`,
		tf.studentASession.ID).Scan(&count); err != nil {
		t.Fatalf("count talk rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("phase 10 event rows = %d, want 1", count)
	}
}

// ---------------------------------------------------------------------------
// The microphone event path (§25/§76) against the real event log
// ---------------------------------------------------------------------------

// TestMicrophoneEventsLandWithTheFrozenPayload is the MIC half of the camera test in
// events_integration_test.go: MIC_STARTED / MIC_STOPPED rows, at-least-once delivery writing
// one row per real change, and a payload made of identifiers — never audio.
func TestMicrophoneEventsLandWithTheFrozenPayload(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	room, identity := f.roomName, stored.LiveKitIdentity

	published := micWebhook(webhook.EventTrackPublished, room, identity, "TR_mic")
	unpublished := micWebhook(webhook.EventTrackUnpublished, room, identity, "TR_mic")
	// A duplicate, then the stop, then a retry of the start: exactly two real changes.
	for _, event := range []*livekit.WebhookEvent{published, published, unpublished, published} {
		if err := processor.ProcessWebhook(ctx, event); err != nil {
			t.Fatalf("%s: %v", event.GetEvent(), err)
		}
	}

	want := []string{"MIC_STARTED", "MIC_STOPPED"}
	got := eventTypesOf(t, f, stored.ID)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// The session row is untouched by the microphone (§21/§25): only a screen decides.
	if row := f.session(t, stored.ID); row.Status != "CONNECTING" {
		t.Fatalf("status = %s, want CONNECTING", row.Status)
	}

	var payload map[string]any
	if err := f.pool.QueryRow(ctx, `
		SELECT payload FROM session_events WHERE session_id = $1 AND type = 'MIC_STARTED'`,
		stored.ID).Scan(&payload); err != nil {
		t.Fatalf("read MIC_STARTED payload: %v", err)
	}
	for key, want := range map[string]any{
		"trackSid":    "TR_mic",
		"trackSource": "MICROPHONE",
		"runId":       f.runID.String(),
		"studentId":   f.studentA.ID.String(),
	} {
		if payload[key] != want {
			t.Fatalf("payload[%s] = %v, want %v (payload = %+v)", key, payload[key], want, payload)
		}
	}
	// §13/§53: no audio, ever.
	for _, forbidden := range []string{"audio", "transcript", "durationSeconds", "displayName"} {
		if _, leaked := payload[forbidden]; leaked {
			t.Fatalf("payload carries %q: %+v", forbidden, payload)
		}
	}
}

// micWebhook is the microphone's version of cameraWebhook.
func micWebhook(kind, room, identity, sid string) *livekit.WebhookEvent {
	event := webhookFor(kind, room, identity)
	event.Track = &livekit.TrackInfo{Sid: sid, Source: livekit.TrackSource_MICROPHONE, Type: livekit.TrackType_AUDIO}
	return event
}

// ---------------------------------------------------------------------------
// Revocation triggers with no request behind them (§31)
// ---------------------------------------------------------------------------

// TestClosingTheRunEndsTheTalkAndRevokesTheSubscription is the trigger that matters most in
// production: the teacher closes the lesson, and the student who was hearing them must stop.
// It goes through the Processor, which is what the classroom close actually calls.
func TestClosingTheRunEndsTheTalkAndRevokesTheSubscription(t *testing.T) {
	tf := newTalkFixture(t)
	processor := session.NewProcessor(tf.repo, nil).WithPrivateTalkEnder(tf.service)
	ctx := context.Background()

	tf.start(t, tf.studentA.ID)
	if !tf.media.canHear(tf.studentASession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the talk did not start")
	}
	if tf.media.canHear(tf.studentBSession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("a classmate was subscribed to the teacher's microphone")
	}

	closed, err := processor.CloseRunSessions(ctx, tf.runID)
	if err != nil {
		t.Fatalf("CloseRunSessions(): %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed sessions = %d, want 2", closed)
	}

	if tf.media.canHear(tf.studentASession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the target is still subscribed to the teacher's microphone after the lesson ended")
	}
	if len(tf.events.ended) != 1 || tf.events.ended[0] != tf.studentASession.ID {
		t.Fatalf("PRIVATE_TALK_ENDED = %v, want the target's session", tf.events.ended)
	}

	rows := talkEventRows(t, tf.fixture, tf.studentASession.ID)
	if len(rows) != 2 || rows[1].Type != "TEACHER_TALK_ENDED" {
		t.Fatalf("rows = %+v, want STARTED then ENDED", rows)
	}
	if reason := rows[1].Payload["reason"]; reason != session.TalkEndReasonRoomClosed {
		t.Fatalf("end reason = %v, want ROOM_CLOSED", reason)
	}

	// The state is gone too: the read endpoint answers IDLE (the classroom is closed, which
	// is answered as "nobody is being talked to", not as an error).
	view, err := tf.service.PrivateTalk(ctx, tf.classroomID, tf.teacher.ID)
	if err != nil {
		t.Fatalf("PrivateTalk(): %v", err)
	}
	if view.Target != nil {
		t.Fatalf("target = %+v, want IDLE", view.Target)
	}
}

// TestParticipantLeftEndsTheTalk: the webhook path. The target's connection dropped, so the
// talk is over — the teacher's console has to be told even if nobody pressed anything.
func TestParticipantLeftEndsTheTalk(t *testing.T) {
	tf := newTalkFixture(t)
	processor := session.NewProcessor(tf.repo, nil).WithPrivateTalkEnder(tf.service)
	ctx := context.Background()

	tf.start(t, tf.studentA.ID)

	if err := processor.ProcessWebhook(ctx, webhookFor(webhook.EventParticipantLeft, tf.roomName, tf.studentASession.LiveKitIdentity)); err != nil {
		t.Fatalf("participant_left: %v", err)
	}

	if tf.media.canHear(tf.studentASession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the departed target is still subscribed to the teacher's microphone")
	}
	if len(tf.events.ended) != 1 {
		t.Fatalf("PRIVATE_TALK_ENDED = %v, want one", tf.events.ended)
	}
	rows := talkEventRows(t, tf.fixture, tf.studentASession.ID)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want STARTED then ENDED", rows)
	}
	if reason := rows[1].Payload["reason"]; reason != session.TalkEndReasonDisconnected {
		t.Fatalf("end reason = %v, want DISCONNECTED", reason)
	}
	// The session state machine did its own job too: the student is DISCONNECTED (§45).
	if row := tf.session(t, tf.studentASession.ID); row.Status != "DISCONNECTED" {
		t.Fatalf("status = %s, want DISCONNECTED", row.Status)
	}
}

// TestStudentLeaveEndsTheTalkOnce is the §43 path: the student presses 离开课堂. The talk ends
// once, and a retried leave does not append a second ENDED row.
func TestStudentLeaveEndsTheTalkOnce(t *testing.T) {
	tf := newTalkFixture(t)
	// The leave endpoint reports into the processor, and the processor reports a departed
	// target back into the private-talk state machine — the cycle main wires explicitly.
	processor := session.NewProcessor(tf.repo, nil).WithPrivateTalkEnder(tf.service)
	tf.service.WithLifecycleEvents(processor)

	ctx := context.Background()
	tf.start(t, tf.studentA.ID)

	if _, err := tf.service.Leave(ctx, tf.studentASession.ID, tf.studentA.ID); err != nil {
		t.Fatalf("Leave(): %v", err)
	}
	if tf.media.canHear(tf.studentASession.LiveKitIdentity, tf.teacherMic) {
		t.Fatal("the student who left is still subscribed to the teacher's microphone")
	}
	if len(tf.events.ended) != 1 {
		t.Fatalf("PRIVATE_TALK_ENDED = %v, want one", tf.events.ended)
	}

	// A retried leave (the same call again) must not add history: the session is already
	// LEFT, and the talk is already over.
	if _, err := tf.service.Leave(ctx, tf.studentASession.ID, tf.studentA.ID); err != nil {
		t.Fatalf("second Leave(): %v", err)
	}
	if rows := talkEventRows(t, tf.fixture, tf.studentASession.ID); len(rows) != 2 {
		t.Fatalf("rows = %+v, want exactly two after a retried leave", rows)
	}
	if len(tf.events.ended) != 1 {
		t.Fatalf("PRIVATE_TALK_ENDED = %v, want exactly one", tf.events.ended)
	}
}
