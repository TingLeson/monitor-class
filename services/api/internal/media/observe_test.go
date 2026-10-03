package media

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

// The observation half of the camera path (§24/§51): what ObserveRoom reports about a
// CAMERA track, and what it deliberately refuses to report.
//
// WHY this is worth its own file: `camera.active` in the teacher's monitor DTO is defined
// as "the media plane reported an UNMUTED camera track" (§51), and the mute test is the
// only thing standing between "the student's camera is on" and "the student published a
// camera once and switched it off". The control plane never guesses this from a session
// status (§24: a camera changes nothing), so this function is the single source of the
// answer.

// roomWithParticipants is the room service of this file: ListParticipants is scripted,
// every other RPC keeps failing loudly through the embedded fake.
type roomWithParticipants struct {
	*fakeRoomService
	participants []*livekit.ParticipantInfo
	err          error
}

func (f *roomWithParticipants) ListParticipants(context.Context, *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &livekit.ListParticipantsResponse{Participants: f.participants}, nil
}

func observedRoom(t *testing.T, participants ...*livekit.ParticipantInfo) *Client {
	t.Helper()
	return &Client{rooms: &roomWithParticipants{fakeRoomService: newFakeRoomService(), participants: participants}}
}

func participant(identity string, tracks ...*livekit.TrackInfo) *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{
		Identity: identity,
		Sid:      "PA_" + identity,
		State:    livekit.ParticipantInfo_ACTIVE,
		Tracks:   tracks,
	}
}

func videoTrack(sid string, source livekit.TrackSource, muted bool) *livekit.TrackInfo {
	return &livekit.TrackInfo{Sid: sid, Source: source, Type: livekit.TrackType_VIDEO, Muted: muted}
}

func TestObserveRoomReportsAnUnmutedCamera(t *testing.T) {
	client := observedRoom(t,
		participant(testStudentA,
			videoTrack("TR_screen", livekit.TrackSource_SCREEN_SHARE, false),
			videoTrack("TR_cam", livekit.TrackSource_CAMERA, false),
		),
	)

	observed, err := client.ObserveRoom(context.Background(), testRoom)
	if err != nil {
		t.Fatalf("ObserveRoom(): %v", err)
	}
	tracks, ok := observed[testStudentA]
	if !ok {
		t.Fatalf("observed = %+v, want the participant", observed)
	}
	if !tracks.Camera || !tracks.ScreenShare {
		t.Fatalf("tracks = %+v, want screen and camera active", tracks)
	}
	if tracks.Microphone {
		t.Fatal("microphone = true: Phase 9 grants no microphone (§76)")
	}
	// Both publications are listed even though only one is flowing: §26 revocation works
	// on publications, and a muted one is still something a classmate could subscribe to.
	if len(tracks.Tracks) != 2 {
		t.Fatalf("publications = %+v, want both tracks listed", tracks.Tracks)
	}
}

// TestObserveRoomTreatsAMutedCameraAsInactive is the difference between "published a
// camera" and "the camera is on": §51's tile must be dark for a muted track, while the
// publication stays visible to the §26 reconciliation.
func TestObserveRoomTreatsAMutedCameraAsInactive(t *testing.T) {
	client := observedRoom(t,
		participant(testStudentA,
			videoTrack("TR_cam", livekit.TrackSource_CAMERA, true),
			videoTrack("TR_mic", livekit.TrackSource_MICROPHONE, true),
		),
	)

	observed, err := client.ObserveRoom(context.Background(), testRoom)
	if err != nil {
		t.Fatalf("ObserveRoom(): %v", err)
	}
	tracks := observed[testStudentA]
	if tracks.Camera || tracks.Microphone || tracks.ScreenShare {
		t.Fatalf("tracks = %+v, want every flag false for muted tracks", tracks)
	}
	if len(tracks.Tracks) != 2 {
		t.Fatalf("publications = %+v, want the muted tracks still listed", tracks.Tracks)
	}
}

// TestObserveRoomSkipsDisconnectedParticipants: LiveKit keeps a participant in the list
// for a while after the connection dropped, and a camera on a connection that is gone is
// not a camera.
func TestObserveRoomSkipsDisconnectedParticipants(t *testing.T) {
	gone := participant(testStudentA, videoTrack("TR_cam", livekit.TrackSource_CAMERA, false))
	gone.State = livekit.ParticipantInfo_DISCONNECTED

	observed, err := observedRoom(t, gone).ObserveRoom(context.Background(), testRoom)
	if err != nil {
		t.Fatalf("ObserveRoom(): %v", err)
	}
	if len(observed) != 0 {
		t.Fatalf("observed = %+v, want nobody: a disconnected participant is not in the room", observed)
	}
}

// TestObserveRoomReportsAnAbsentRoomAsEmpty: the teacher's console polls before the first
// student joins, when LiveKit has no room at all. That is an observation ("nobody"), not a
// failure — and the camera flag of every tile is false because there is nobody to observe.
func TestObserveRoomReportsAnAbsentRoomAsEmpty(t *testing.T) {
	client := &Client{rooms: &roomWithParticipants{
		fakeRoomService: newFakeRoomService(),
		err:             twirp.NewError(twirp.NotFound, "room not found"),
	}}
	observed, err := client.ObserveRoom(context.Background(), testRoom)
	if err != nil {
		t.Fatalf("ObserveRoom(): %v", err)
	}
	if len(observed) != 0 {
		t.Fatalf("observed = %+v, want an empty observation", observed)
	}
}

// TestObserveRoomReportsAGenuineFailure: the distinction the whole §33 rule rests on. A
// rotated key or a LiveKit outage must NOT be reported as "nobody has a camera", because
// the monitor would then answer a media-plane outage with a business fact.
func TestObserveRoomReportsAGenuineFailure(t *testing.T) {
	client := &Client{rooms: &roomWithParticipants{
		fakeRoomService: newFakeRoomService(),
		err:             twirp.NewError(twirp.Internal, "upstream is down"),
	}}
	if _, err := client.ObserveRoom(context.Background(), testRoom); err == nil {
		t.Fatal("ObserveRoom() returned nil: a failure must stay a failure, not an empty room")
	}
}

// TestObserveRoomRejectsAForeignRoom keeps the media boundary honest: only opaque
// `lk_<id>` rooms of this project are queried at all (§8/§44).
func TestObserveRoomRejectsAForeignRoom(t *testing.T) {
	client := observedRoom(t)
	for _, room := range []string{"", "class-3-math", "lk_"} {
		if _, err := client.ObserveRoom(context.Background(), room); err == nil {
			t.Errorf("ObserveRoom(%q) = nil, want a refusal", room)
		}
	}
}

// TestObserveRoomUnknownSourceIsNotCamera: a source this project does not know becomes
// "", so a future LiveKit track type cannot silently count as the camera of a student.
func TestObserveRoomUnknownSourceIsNotCamera(t *testing.T) {
	unknown := videoTrack("TR_x", livekit.TrackSource_UNKNOWN, false)
	observed, err := observedRoom(t, participant(testStudentA, unknown)).ObserveRoom(context.Background(), testRoom)
	if err != nil {
		t.Fatalf("ObserveRoom(): %v", err)
	}
	tracks := observed[testStudentA]
	if tracks.Camera || tracks.ScreenShare || tracks.Microphone {
		t.Fatalf("tracks = %+v, want no flag set for an unknown source", tracks)
	}
	if len(tracks.Tracks) != 1 || tracks.Tracks[0].Source != "" {
		t.Fatalf("publications = %+v, want the track listed with an empty source", tracks.Tracks)
	}
}

// compile-time reminder that the fake in this file is a roomService.
var _ roomService = (*roomWithParticipants)(nil)
