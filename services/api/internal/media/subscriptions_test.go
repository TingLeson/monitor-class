package media

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
)

// The §26/§73 rule at the media boundary: what the Control Plane asks LiveKit to
// revoke, how often it asks, and what it does when LiveKit refuses.
//
// The fake stands in for the RoomService client, which is what makes the interesting
// cases testable: a student who reloads mid-lesson, a publisher who vanishes between
// the observation and the call, a room where nobody has anything to revoke. A real SFU
// cannot produce any of those on demand.

const (
	testRoom     = "lk_2c81a3e0-1111-2222-3333-444455556666"
	testStudentA = "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f"
	testStudentB = "8d917e5c-2222-3333-4444-555566667777"
	testTeacher  = "1f0a6b3d-9999-8888-7777-666655554444"
)

// fakeRoomService records the calls this package makes, and fails the ones a test
// scripts to fail.
type fakeRoomService struct {
	subscriptions []*livekit.UpdateSubscriptionsRequest
	// failFor maps a subscriber identity to the error its call returns.
	failFor map[string]error
}

func newFakeRoomService() *fakeRoomService {
	return &fakeRoomService{failFor: map[string]error{}}
}

func (f *fakeRoomService) UpdateSubscriptions(_ context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	f.subscriptions = append(f.subscriptions, req)
	if err := f.failFor[req.GetIdentity()]; err != nil {
		return nil, err
	}
	return &livekit.UpdateSubscriptionsResponse{}, nil
}

func (f *fakeRoomService) CreateRoom(context.Context, *livekit.CreateRoomRequest) (*livekit.Room, error) {
	return nil, errors.New("fakeRoomService: CreateRoom was not expected")
}

func (f *fakeRoomService) ListRooms(context.Context, *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error) {
	return nil, errors.New("fakeRoomService: ListRooms was not expected")
}

func (f *fakeRoomService) DeleteRoom(context.Context, *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	return nil, errors.New("fakeRoomService: DeleteRoom was not expected")
}

func (f *fakeRoomService) ListParticipants(context.Context, *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	return nil, errors.New("fakeRoomService: ListParticipants was not expected")
}

func (f *fakeRoomService) RemoveParticipant(context.Context, *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	return nil, errors.New("fakeRoomService: RemoveParticipant was not expected")
}

func newEnforcementClient() (*Client, *fakeRoomService) {
	fake := newFakeRoomService()
	return &Client{rooms: fake}, fake
}

// observation builds one room snapshot: every key is a participant identity, and the
// tracks are given as (sid, source) pairs.
func observation(participants map[string][]ObservedTrack) map[string]ParticipantTracks {
	out := make(map[string]ParticipantTracks, len(participants))
	for identity, tracks := range participants {
		observed := ParticipantTracks{
			// A connection id per identity: the production observation always has one, and
			// the bookkeeping is keyed by it (see revokedSubscription).
			ParticipantSid: "PA_" + identity,
			Tracks:         tracks,
		}
		for _, track := range tracks {
			switch track.Source {
			case PublishScreenShare:
				observed.ScreenShare = true
			case PublishCamera:
				observed.Camera = true
			case PublishMicrophone:
				observed.Microphone = true
			}
		}
		out[identity] = observed
	}
	return out
}

// TestEnforceNoPeerSubscriptionsRevokesClassmatesOnly is the core §26 assertion: a
// student is unsubscribed from a classmate's track, the whitelisted owner is left
// alone, and a participant cannot be asked to unsubscribe from itself.
func TestEnforceNoPeerSubscriptionsRevokesClassmatesOnly(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := observation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}, {Sid: "TR_B_MIC", Source: PublishMicrophone}},
		// The teacher is the whitelist of Phase 7 (the only non-student identity this
		// control plane mints a token for). Phase 7 gives the teacher no tracks at all,
		// so this one exists to prove the whitelist is honoured rather than assumed.
		testTeacher: {{Sid: "TR_T_MIC", Source: PublishMicrophone}},
	})

	revoked, err := client.EnforceNoPeerSubscriptions(context.Background(), testRoom,
		[]string{testStudentA, testStudentB}, observed, []string{testTeacher})
	if err != nil {
		t.Fatalf("EnforceNoPeerSubscriptions(): %v", err)
	}

	if len(fake.subscriptions) != 2 {
		t.Fatalf("UpdateSubscriptions calls = %d, want one per student", len(fake.subscriptions))
	}
	byIdentity := map[string]*livekit.UpdateSubscriptionsRequest{}
	for _, request := range fake.subscriptions {
		byIdentity[request.GetIdentity()] = request
	}

	aRequest := byIdentity[testStudentA]
	if aRequest == nil {
		t.Fatal("student A was not asked to unsubscribe from anything")
	}
	if aRequest.GetSubscribe() {
		t.Error("subscribe = true, want false: the call must REVOKE")
	}
	if aRequest.GetRoom() != testRoom {
		t.Errorf("room = %q, want %q", aRequest.GetRoom(), testRoom)
	}
	if sids := aRequest.GetTrackSids(); len(sids) != 2 || sids[0] != "TR_B_MIC" || sids[1] != "TR_B_SCREEN" {
		t.Errorf("student A track sids = %v, want B's two tracks sorted and only those", sids)
	}
	if ids := aRequest.GetParticipantTracks(); len(ids) != 0 {
		t.Errorf("participant_tracks = %v, want the flat sid form", ids)
	}

	bRequest := byIdentity[testStudentB]
	if bRequest == nil {
		t.Fatal("student B was not asked to unsubscribe from anything")
	}
	if sids := bRequest.GetTrackSids(); len(sids) != 1 || sids[0] != "TR_A_SCREEN" {
		t.Errorf("student B track sids = %v, want only A's screen", sids)
	}

	// The teacher's track is never part of a revocation, and no student's OWN track is.
	for identity, request := range byIdentity {
		for _, sid := range request.GetTrackSids() {
			if sid == "TR_T_MIC" {
				t.Errorf("the whitelisted owner's track was revoked for %s", identity)
			}
			if identity == testStudentA && sid == "TR_A_SCREEN" {
				t.Error("student A was asked to unsubscribe from its own track")
			}
			if identity == testStudentB && strings.HasPrefix(sid, "TR_B_") {
				t.Error("student B was asked to unsubscribe from its own track")
			}
		}
	}

	if len(revoked) != 3 {
		t.Fatalf("revocations = %+v, want three (B's two tracks and A's screen)", revoked)
	}
	first := revoked[0]
	if first.ObserverIdentity != testStudentA || first.TrackOwnerIdentity != testStudentB || first.TrackSid != "TR_B_MIC" {
		t.Errorf("first revocation = %+v, want A ← B/TR_B_MIC (deterministic order)", first)
	}
}

// TestEnforceNoPeerSubscriptionsKeepsTheWhitelistWhole: a room where every publisher
// is whitelisted costs nothing at all — no call, no log-worthy revocation.
func TestEnforceNoPeerSubscriptionsKeepsTheWhitelistWhole(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := observation(map[string][]ObservedTrack{
		testStudentA: {},
		testTeacher:  {{Sid: "TR_T_MIC", Source: PublishMicrophone}},
	})

	revoked, err := client.EnforceNoPeerSubscriptions(context.Background(), testRoom,
		[]string{testStudentA}, observed, []string{testTeacher})
	if err != nil {
		t.Fatalf("EnforceNoPeerSubscriptions(): %v", err)
	}
	if len(revoked) != 0 {
		t.Errorf("revocations = %+v, want none", revoked)
	}
	if len(fake.subscriptions) != 0 {
		t.Errorf("UpdateSubscriptions calls = %d, want none", len(fake.subscriptions))
	}
}

// TestEnforceNoPeerSubscriptionsIsIdempotent is the "minimal calls" rule: the monitor
// polls every 10 seconds, and an unchanged room must not produce a stream of identical
// revocations. A NEW track, a NEW connection or a track that came back with a new sid
// must each produce exactly one.
func TestEnforceNoPeerSubscriptionsIsIdempotent(t *testing.T) {
	client, fake := newEnforcementClient()
	ctx := context.Background()
	students := []string{testStudentA, testStudentB}
	observed := observation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})

	if _, err := client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(fake.subscriptions) != 2 {
		t.Fatalf("first pass calls = %d, want 2", len(fake.subscriptions))
	}

	// Same room, same tracks: nothing to do.
	revoked, err := client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(revoked) != 0 || len(fake.subscriptions) != 2 {
		t.Errorf("second pass issued %d revocations / %d calls, want none", len(revoked), len(fake.subscriptions)-2)
	}

	// Student B publishes a camera: only that new track is revoked (for A only).
	observed[testStudentB] = ParticipantTracks{
		ParticipantSid: observed[testStudentB].ParticipantSid,
		Camera:         true,
		Tracks: []ObservedTrack{
			{Sid: "TR_B_SCREEN", Source: PublishScreenShare},
			{Sid: "TR_B_CAM", Source: PublishCamera},
		},
	}
	revoked, err = client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if len(revoked) != 1 || revoked[0].TrackSid != "TR_B_CAM" || revoked[0].ObserverIdentity != testStudentA {
		t.Fatalf("revocations = %+v, want exactly A ← B/TR_B_CAM", revoked)
	}
	if len(fake.subscriptions) != 3 {
		t.Fatalf("calls = %d, want 3 (one new revocation)", len(fake.subscriptions))
	}

	// Student A reloads the page: same identity, new connection. A client that
	// subscribes on connect must be revoked again on the new connection.
	reconnected := observed[testStudentA]
	reconnected.ParticipantSid = "PA_A_RECONNECTED"
	observed[testStudentA] = reconnected
	revoked, err = client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if err != nil {
		t.Fatalf("fourth pass: %v", err)
	}
	issuedForA := 0
	for _, revocation := range revoked {
		if revocation.ObserverIdentity == testStudentA {
			issuedForA++
		}
	}
	if issuedForA != 2 {
		t.Errorf("revocations after A reconnected = %d, want 2 (B's two tracks)", issuedForA)
	}

	// A track that disappears and is published again gets a new sid, which is a new
	// revocation: the bookkeeping must not remember the old one forever.
	observed[testStudentB] = ParticipantTracks{
		ParticipantSid: observed[testStudentB].ParticipantSid,
		Tracks:         []ObservedTrack{{Sid: "TR_B_SCREEN_2", Source: PublishScreenShare}},
	}
	revoked, err = client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if err != nil {
		t.Fatalf("fifth pass: %v", err)
	}
	if len(revoked) != 1 || revoked[0].TrackSid != "TR_B_SCREEN_2" {
		t.Errorf("revocations = %+v, want the republished track to be revoked", revoked)
	}
	if _, stale := client.revoked[testRoom][revokedSubscription{participantSid: "PA_" + testStudentB, trackSid: "TR_B_CAM"}]; stale {
		t.Error("the bookkeeping kept a track that is no longer published")
	}
}

// TestEnforceNoPeerSubscriptionsSkipsStudentsWhoAreNotInTheRoom: an RPC for an absent
// participant would only produce a NotFound, and a student who is not connected cannot
// be receiving anything.
func TestEnforceNoPeerSubscriptionsSkipsStudentsWhoAreNotInTheRoom(t *testing.T) {
	client, fake := newEnforcementClient()
	observed := observation(map[string][]ObservedTrack{
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})

	revoked, err := client.EnforceNoPeerSubscriptions(context.Background(), testRoom,
		[]string{testStudentA, testStudentB}, observed, nil)
	if err != nil {
		t.Fatalf("EnforceNoPeerSubscriptions(): %v", err)
	}
	if len(revoked) != 0 {
		t.Errorf("revocations = %+v, want none (A is not connected)", revoked)
	}
	if len(fake.subscriptions) != 0 {
		t.Errorf("UpdateSubscriptions calls = %d, want none", len(fake.subscriptions))
	}
}

// TestEnforceNoPeerSubscriptionsReportsFailuresWithoutStopping: the §33 discipline.
// One student's failed revocation must not stop the others, the error is returned so
// the caller can log it, and it is NOT remembered — the next poll retries.
func TestEnforceNoPeerSubscriptionsReportsFailuresWithoutStopping(t *testing.T) {
	client, fake := newEnforcementClient()
	ctx := context.Background()
	students := []string{testStudentA, testStudentB}
	observed := observation(map[string][]ObservedTrack{
		testStudentA: {{Sid: "TR_A_SCREEN", Source: PublishScreenShare}},
		testStudentB: {{Sid: "TR_B_SCREEN", Source: PublishScreenShare}},
	})
	failure := errors.New("livekit: update subscriptions: unavailable")
	fake.failFor[testStudentA] = failure

	revoked, err := client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the media failure", err)
	}
	if len(revoked) != 1 || revoked[0].ObserverIdentity != testStudentB {
		t.Fatalf("revocations = %+v, want student B's revocation to have been attempted anyway", revoked)
	}

	// The failed student is retried on the next poll; the successful one is not.
	fake.failFor = map[string]error{}
	revoked, err = client.EnforceNoPeerSubscriptions(ctx, testRoom, students, observed, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(revoked) != 1 || revoked[0].ObserverIdentity != testStudentA {
		t.Fatalf("retry revocations = %+v, want only the previously failed student", revoked)
	}
}

// TestEnforceNoPeerSubscriptionsRejectsANonProjectRoom keeps the §8 contract: this
// method only ever touches rooms this control plane named.
func TestEnforceNoPeerSubscriptionsRejectsANonProjectRoom(t *testing.T) {
	client, fake := newEnforcementClient()
	if _, err := client.EnforceNoPeerSubscriptions(context.Background(), "class-3-math",
		[]string{testStudentA}, nil, nil); err == nil {
		t.Fatal("a non-project room name was accepted")
	}
	if len(fake.subscriptions) != 0 {
		t.Error("the media plane was called with a foreign room name")
	}
}

// TestObservedSourceIsTotal: an unknown LiveKit source becomes "", which matches no
// publish source and therefore no whitelist entry — an unrecognised track is revoked
// rather than kept.
func TestObservedSourceIsTotal(t *testing.T) {
	cases := map[livekit.TrackSource]PublishSource{
		livekit.TrackSource_SCREEN_SHARE:       PublishScreenShare,
		livekit.TrackSource_CAMERA:             PublishCamera,
		livekit.TrackSource_MICROPHONE:         PublishMicrophone,
		livekit.TrackSource_UNKNOWN:            "",
		livekit.TrackSource_SCREEN_SHARE_AUDIO: "",
	}
	for source, want := range cases {
		if got := observedSource(source); got != want {
			t.Errorf("observedSource(%s) = %q, want %q", source, got, want)
		}
	}
}
