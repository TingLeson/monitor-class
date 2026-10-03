package media

import (
	"context"
	"errors"
	"testing"
)

// The §31 rule at the media boundary: who the Control Plane subscribes to the teacher's
// microphone, how often it says so, and what happens when LiveKit refuses.
//
// The fake is the same RoomService stand-in the §26 tests use, with one addition: it keeps
// the SUBSCRIPTION STATE it was told about, so a test can ask the product question — "can
// this student hear the teacher?" — instead of only counting calls.

// talkObservation builds a room with a teacher publishing a microphone and the given
// students present.
func talkObservation(studentTracks map[string][]ObservedTrack) map[string]ParticipantTracks {
	observed := observation(map[string][]ObservedTrack{
		testTeacher: {{Sid: "TR_T_MIC", Source: PublishMicrophone}},
	})
	for identity, tracks := range studentTracks {
		entry := observed[identity]
		entry.ParticipantSid = "PA_" + identity
		entry.Tracks = tracks
		observed[identity] = entry
	}
	return observed
}

// subscribedTo applies the recorded UpdateSubscriptions calls to a book of subscription
// state, which is what turns "these RPCs were sent" into "this is who receives the audio".
func subscribedTo(fake *fakeRoomService) map[string]map[string]bool {
	state := map[string]map[string]bool{}
	for _, req := range fake.subscriptions {
		if state[req.GetIdentity()] == nil {
			state[req.GetIdentity()] = map[string]bool{}
		}
		for _, sid := range req.GetTrackSids() {
			state[req.GetIdentity()][sid] = req.GetSubscribe()
		}
	}
	return state
}

// TestEnforcePrivateTalkSubscribesOnlyTheTarget is the core §31 assertion: the selected
// student is subscribed to the teacher's microphone, and every other student of the room is
// explicitly unsubscribed from the same track.
func TestEnforcePrivateTalkSubscribesOnlyTheTarget(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})

	enforcement, err := client.EnforcePrivateTalk(context.Background(), testRoom,
		[]string{testStudentA, testStudentB}, observed, []string{testTeacher}, testStudentA)
	if err != nil {
		t.Fatalf("EnforcePrivateTalk(): %v", err)
	}

	state := subscribedTo(fake)
	if !state[testStudentA]["TR_T_MIC"] {
		t.Fatalf("the target is not subscribed to the teacher's microphone: %v", state)
	}
	if state[testStudentB]["TR_T_MIC"] {
		t.Fatalf("a non-target student is subscribed to the teacher's microphone: %v", state)
	}
	if len(fake.subscriptions) != 2 {
		t.Fatalf("UpdateSubscriptions calls = %d, want one per student", len(fake.subscriptions))
	}
	if len(enforcement.Granted) != 1 || enforcement.Granted[0].StudentIdentity != testStudentA {
		t.Fatalf("granted = %+v, want the target", enforcement.Granted)
	}
	if len(enforcement.Revoked) != 1 || enforcement.Revoked[0].StudentIdentity != testStudentB {
		t.Fatalf("revoked = %+v, want the other student", enforcement.Revoked)
	}
	if len(enforcement.TrackSids) != 1 || enforcement.TrackSids[0] != "TR_T_MIC" {
		t.Fatalf("track sids = %v, want the teacher's microphone", enforcement.TrackSids)
	}
}

// TestEnforcePrivateTalkWithoutATargetUnsubscribesEveryone is the convergence rule: with
// no target (IDLE, or after a restart that lost the selection) nobody may receive the
// teacher's microphone. This is the property that makes a process-local state machine safe.
func TestEnforcePrivateTalkWithoutATargetUnsubscribesEveryone(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})

	enforcement, err := client.EnforcePrivateTalk(context.Background(), testRoom,
		[]string{testStudentA, testStudentB}, observed, []string{testTeacher}, "")
	if err != nil {
		t.Fatalf("EnforcePrivateTalk(): %v", err)
	}

	state := subscribedTo(fake)
	if state[testStudentA]["TR_T_MIC"] || state[testStudentB]["TR_T_MIC"] {
		t.Fatalf("a student stays subscribed with no target: %v", state)
	}
	if len(enforcement.Granted) != 0 {
		t.Fatalf("granted = %+v, want none", enforcement.Granted)
	}
	if len(enforcement.Revoked) != 2 {
		t.Fatalf("revoked = %+v, want both students", enforcement.Revoked)
	}
}

// TestEnforcePrivateTalkSwitchRevokesTheOldTarget is §31's "张三 → 结束 → 李四" path: the
// new target is granted and the old one is revoked IN THE SAME PASS, so there is no window
// in which two students hear the teacher.
func TestEnforcePrivateTalkSwitchRevokesTheOldTarget(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})
	students := []string{testStudentA, testStudentB}

	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
		t.Fatalf("start with student A: %v", err)
	}
	fake.subscriptions = nil

	enforcement, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentB)
	if err != nil {
		t.Fatalf("switch to student B: %v", err)
	}

	state := subscribedTo(fake)
	if state[testStudentA]["TR_T_MIC"] {
		t.Fatalf("the OLD target is still subscribed after a switch: %v", state)
	}
	if !state[testStudentB]["TR_T_MIC"] {
		t.Fatalf("the NEW target is not subscribed after a switch: %v", state)
	}
	if len(enforcement.Granted) != 1 || enforcement.Granted[0].StudentIdentity != testStudentB {
		t.Fatalf("granted = %+v, want the new target", enforcement.Granted)
	}
	if len(enforcement.Revoked) != 1 || enforcement.Revoked[0].StudentIdentity != testStudentA {
		t.Fatalf("revoked = %+v, want the old target", enforcement.Revoked)
	}
	// §31 spells the switch out as "张三 → 结束 → 李四": the REVOCATION is issued first, so
	// the two students never overlap as listeners in the moment between the two calls.
	if len(fake.subscriptions) != 2 {
		t.Fatalf("switch calls = %d, want two (revoke, then grant)", len(fake.subscriptions))
	}
	if first := fake.subscriptions[0]; first.GetIdentity() != testStudentA || first.GetSubscribe() {
		t.Fatalf("first call of a switch = %+v, want the OLD target's revocation", first)
	}
	if second := fake.subscriptions[1]; second.GetIdentity() != testStudentB || !second.GetSubscribe() {
		t.Fatalf("second call of a switch = %+v, want the NEW target's grant", second)
	}
}

// TestEnforcePrivateTalkIsIssuedOncePerConnection is the RPC-noise rule the monitor depends
// on: a poll every few seconds must not re-send the same subscription state. A student who
// RECONNECTS, however, is a new connection and gets one fresh pass — their client has
// nothing subscribed.
func TestEnforcePrivateTalkIsIssuedOncePerConnection(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})
	students := []string{testStudentA, testStudentB}

	for i := 0; i < 3; i++ {
		if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	if len(fake.subscriptions) != 2 {
		t.Fatalf("UpdateSubscriptions calls = %d, want 2 (one per student, then silence)", len(fake.subscriptions))
	}

	// The target reconnects: same identity, new connection.
	observed[testStudentA] = ParticipantTracks{
		ParticipantSid: "PA_" + testStudentA + "_2",
		Tracks:         []ObservedTrack{{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
	}
	fake.subscriptions = nil
	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
		t.Fatalf("after reconnect: %v", err)
	}
	if len(fake.subscriptions) != 1 {
		t.Fatalf("UpdateSubscriptions calls after reconnect = %d, want 1 for the new connection", len(fake.subscriptions))
	}
	if req := fake.subscriptions[0]; req.GetIdentity() != testStudentA || !req.GetSubscribe() {
		t.Fatalf("reconnect pass = %+v, want a grant for the target's new connection", req)
	}
}

// TestEnforcePrivateTalkForgetsWhenTheMicrophoneIsGone: a track that is no longer published
// cannot be subscribed to, and its bookkeeping must not survive it. When the teacher turns
// the microphone back on (a NEW sid), the target is granted again.
func TestEnforcePrivateTalkForgetsWhenTheMicrophoneIsGone(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
	})
	students := []string{testStudentA}

	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	fake.subscriptions = nil

	// The teacher unpublishes the microphone: nothing to reconcile.
	observed[testTeacher] = ParticipantTracks{ParticipantSid: "PA_" + testTeacher}
	enforcement, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA)
	if err != nil {
		t.Fatalf("without a microphone: %v", err)
	}
	if len(fake.subscriptions) != 0 {
		t.Fatalf("UpdateSubscriptions calls = %d, want none: there is no track", len(fake.subscriptions))
	}
	if len(enforcement.TrackSids) != 0 {
		t.Fatalf("track sids = %v, want none: this is what TEACHER_MIC_REQUIRED is decided on", enforcement.TrackSids)
	}

	// The microphone comes back with a new sid: the grant is owed again.
	observed[testTeacher] = ParticipantTracks{
		ParticipantSid: "PA_" + testTeacher,
		Tracks:         []ObservedTrack{{Sid: "TR_T_MIC_2", Source: PublishMicrophone}},
	}
	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
		t.Fatalf("after republish: %v", err)
	}
	if len(fake.subscriptions) != 1 || fake.subscriptions[0].GetTrackSids()[0] != "TR_T_MIC_2" {
		t.Fatalf("republish pass = %+v, want a grant for the new sid", fake.subscriptions)
	}
}

// TestEnforcePrivateTalkFailureIsReportedAndRetried: one student's failure must not stop
// the others, the error must reach the caller (a start is a promise), and the failed change
// must stay outstanding so the next pass retries it.
func TestEnforcePrivateTalkFailureIsReportedAndRetried(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})
	students := []string{testStudentA, testStudentB}
	fake.failFor[testStudentA] = errors.New("livekit: unavailable")

	_, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA)
	if err == nil {
		t.Fatal("a failed subscription change was not reported: a teacher would be told the talk started")
	}
	// The other student's revocation was still attempted and applied.
	state := subscribedTo(fake)
	if state[testStudentB]["TR_T_MIC"] {
		t.Fatalf("the failure of one student stopped the others: %v", state)
	}

	// The retry re-issues the failed student's grant (and not the one that succeeded).
	fake.failFor = map[string]error{}
	fake.subscriptions = nil
	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom, students, observed, []string{testTeacher}, testStudentA); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(fake.subscriptions) != 1 || fake.subscriptions[0].GetIdentity() != testStudentA {
		t.Fatalf("retry calls = %+v, want exactly the previously failed student", fake.subscriptions)
	}
}

// TestEnforcePrivateTalkOnlyTouchesStudentsInTheRoom: a student who is not in the
// observation cannot hold a subscription, and asking LiveKit about them can only produce a
// NotFound.
func TestEnforcePrivateTalkOnlyTouchesStudentsInTheRoom(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := talkObservation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
	})

	if _, err := client.EnforcePrivateTalk(context.Background(), testRoom,
		[]string{testStudentA, testStudentB}, observed, []string{testTeacher}, testStudentB); err != nil {
		t.Fatalf("EnforcePrivateTalk(): %v", err)
	}
	if len(fake.subscriptions) != 1 || fake.subscriptions[0].GetIdentity() != testStudentA {
		t.Fatalf("calls = %+v, want only the student who is present", fake.subscriptions)
	}
}

// TestTeacherMicrophoneTracksReadsPublicationsNotFlowingAudio pins the helper the
// precondition (TEACHER_MIC_REQUIRED) is decided on: a MUTED microphone is still a
// publication somebody can be subscribed to, and it is still what the teacher must have
// before a talk can start.
func TestTeacherMicrophoneTracksReadsPublicationsNotFlowingAudio(t *testing.T) {
	observed := map[string]ParticipantTracks{
		testTeacher: {
			ParticipantSid: "PA_teacher",
			// Microphone false: the track is muted, so no media flows right now.
			Microphone: false,
			Tracks: []ObservedTrack{
				{Sid: "TR_T_CAM", Source: PublishCamera},
				{Sid: "TR_T_MIC", Source: PublishMicrophone},
			},
		},
	}
	sids := TeacherMicrophoneTracks(observed, testTeacher)
	if len(sids) != 1 || sids[0] != "TR_T_MIC" {
		t.Fatalf("track sids = %v, want [TR_T_MIC]", sids)
	}
	if got := TeacherMicrophoneTracks(observed, testStudentA); len(got) != 0 {
		t.Fatalf("a participant with no microphone reported %v", got)
	}
}

// TestEnforcePrivateTalkRejectsANonOpaqueRoomName is the §44 safety net at this new entry
// point too: a room name that leaks a person must never reach the media plane.
func TestEnforcePrivateTalkRejectsANonOpaqueRoomName(t *testing.T) {
	client, fake := newEnforcementClient()
	if _, err := client.EnforcePrivateTalk(context.Background(), "class-3-math", nil, nil, nil, ""); err == nil {
		t.Fatal("a non-opaque room name was accepted")
	}
	if len(fake.subscriptions) != 0 {
		t.Fatalf("calls = %+v, want none", fake.subscriptions)
	}
}
