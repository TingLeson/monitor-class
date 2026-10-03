// Package media wraps the LiveKit server SDK.
//
// This package is the entire Media Plane boundary. Everything the Control Plane
// needs to know about LiveKit goes through it, which keeps the two planes
// separable: LiveKit moves media, PostgreSQL decides what is true (§33).
//
// A LiveKit room existing means only that some media infrastructure allocated a
// name. It never means "classroom is open", "this student may join" or "this
// student is being supervised" — those are rows in PostgreSQL, evaluated
// server-side on every request.
package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// roomService is the slice of the LiveKit RoomService API this package uses.
//
// WHY an interface instead of the concrete *lksdk.RoomServiceClient: the rules that
// matter here are about what the Control Plane DOES with a room, and the interesting
// cases (a participant who vanished between the observation and the call, a room
// whose subscriptions cannot be updated) are the ones that are hard to produce on
// demand against a real SFU. A narrow interface keeps those cases unit-testable with
// a fake, the same way internal/session fakes this whole package.
//
// *lksdk.RoomServiceClient satisfies it as-is, so production wiring is unchanged.
type roomService interface {
	CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error)
	ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error)
	DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error)
	ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error)
	RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error)
	UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error)
}

// Client is a thin wrapper around the LiveKit RoomService API.
//
// It exposes exactly what the Control Plane needs from the Media Plane: room
// lifecycle (Phase 0/3/6), participant observation (Phase 6), participant-scoped
// token minting (Phase 6) and the student-isolation reconciliation of §26 (Phase 7).
// Participant administration beyond "remove" and "unsubscribe", and webhook handling,
// arrive with the phases that actually need them (§74).
type Client struct {
	rooms roomService
	// apiKey/apiSecret sign tokens. They exist only in this backend process and must
	// never be logged, serialised into a response, or copied into a frontend bundle
	// (§44/§59) — see NewClient.
	apiKey    string
	apiSecret string

	// metrics is the §77 instrumentation. Optional; every recording is a no-op
	// when it is nil (see WithMetrics).
	metrics *metrics.Metrics

	// revoked remembers which peer-track subscriptions were already revoked, per room.
	// WHY the media client holds state at all: §26 enforcement is a RECONCILIATION
	// (§26/§73), and re-issuing the same UpdateSubscriptions on every 10s poll would be
	// a stream of pointless RPCs against LiveKit Cloud. The map is guarded by mu — the
	// monitor endpoint is served concurrently — and it is best-effort by design: it is
	// not persisted, so a restart (or a second API instance) re-issues one revocation
	// per live track, which is harmless and actually desirable after a redeploy.
	//
	// talkApplied is the same kind of memory for §31's second, narrower rule: which
	// private-talk subscription changes (the target's grant, everybody else's revocation)
	// are already in effect for one (connection, track, subscribe) key. It is a book of
	// its own and NOT part of `revoked`, because the two rules reconcile different tracks
	// — sharing one map would make each pass prune the other's work and re-issue it on
	// every poll.
	mu          sync.Mutex
	revoked     subscriptionBook[revokedSubscription]
	talkApplied subscriptionBook[talkSubscription]
}

// NewClient builds a RoomService client from the API URL, key and secret.
//
// apiURL is the server-to-server endpoint (http:// or https://, and ws:// is
// accepted). It is a different value from the browser-facing LIVEKIT_URL in
// Docker, where the API talks to http://livekit:7880 while browsers connect to
// ws://localhost:7880.
//
// SECURITY: apiSecret authenticates full room administration. It must exist only
// in the backend process — never in a frontend bundle, never in a mobile build,
// never in a log line, never in an API response (§44/§59). The browser only ever
// receives a short-lived, participant-scoped token minted server-side (Phase 6).
func NewClient(apiURL, apiKey, apiSecret string) (*Client, error) {
	if strings.TrimSpace(apiURL) == "" {
		return nil, fmt.Errorf("livekit: api url is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("livekit: api key is required")
	}
	if strings.TrimSpace(apiSecret) == "" {
		// Report presence, never the value.
		return nil, fmt.Errorf("livekit: api secret is required")
	}
	return &Client{
		rooms:     lksdk.NewRoomServiceClient(apiURL, apiKey, apiSecret),
		apiKey:    apiKey,
		apiSecret: apiSecret,
	}, nil
}

// HealthCheck verifies the LiveKit server is reachable by listing rooms.
//
// It is used by the readiness probe, so it must be cheap and must not mutate
// anything. The timeout belongs to the caller: readiness needs 2s, a boot check
// may want more.
func (c *Client) HealthCheck(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { c.observeCall(metrics.MediaOperationHealthCheck, start, err) }()
	if c == nil || c.rooms == nil {
		return fmt.Errorf("livekit: not connected")
	}
	if _, err := c.rooms.ListRooms(ctx, &livekit.ListRoomsRequest{}); err != nil {
		return fmt.Errorf("livekit: list rooms: %w", err)
	}
	return nil
}

// CreateRoom creates a room with an explicit, caller-supplied opaque name.
//
// The name is opaque by contract. Phase 3 will pass "lk_<classroom_run_uuid>",
// i.e. one room per OPEN period, and one run is never reused for a later lesson.
//
// NEVER use a student name, an account name, a real class name or any other
// human-meaningful string as a LiveKit room name or participant identity
// (§8/§26/§44). Room names and identities reach every participant's client and
// the LiveKit dashboard; a name there would tell a student exactly who else is
// being monitored. All media-layer identities are opaque UUIDs.
func (c *Client) CreateRoom(ctx context.Context, roomName string) (err error) {
	start := time.Now()
	defer func() { c.observeCall(metrics.MediaOperationCreateRoom, start, err) }()
	return c.createRoom(ctx, roomName)
}

// createRoom is CreateRoom without the instrumentation.
//
// WHY the split: EnsureRoom calls it, and counting both methods would attribute one
// logical "make sure the room exists" to two operations — the `create_room` counter
// would then be a mix of direct calls and joins, and neither number would answer a
// question. Each public method measures exactly one operation.
func (c *Client) createRoom(ctx context.Context, roomName string) error {
	if err := validateOpaqueRoomName(roomName); err != nil {
		return err
	}
	// EmptyRoomTimeout keeps an abandoned room from lingering forever: a classroom
	// closed while LiveKit was unreachable should not leave a room that a late
	// student could still connect to. Room state is bookkept in PostgreSQL, so
	// LiveKit expiring a room is safe.
	_, err := c.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:             roomName,
		EmptyTimeout:     emptyRoomTimeoutSeconds,
		DepartureTimeout: departureTimeoutSeconds,
		MaxParticipants:  maxParticipantsPerRoom,
	})
	if err != nil {
		return fmt.Errorf("livekit: create room: %w", err)
	}
	return nil
}

// TerminateRoom closes a room and disconnects everyone in it.
//
// WHY termination is driven by the Control Plane and not by LiveKit: when a
// teacher closes a classroom, the authoritative state change is the PostgreSQL
// transaction. Terminating the LiveKit room afterwards is cleanup that makes the
// media plane catch up with the database; if it fails, the database is still
// correct and the media plane is merely behind. Never the other way round.
func (c *Client) TerminateRoom(ctx context.Context, roomName string) (err error) {
	start := time.Now()
	defer func() { c.observeCall(metrics.MediaOperationTerminateRoom, start, err) }()
	if err := validateOpaqueRoomName(roomName); err != nil {
		return err
	}
	if _, err := c.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: roomName}); err != nil {
		return fmt.Errorf("livekit: delete room: %w", err)
	}
	return nil
}

// EnsureRoom creates the room if it is not already there, and reports success if it
// is. It is the idempotent form of CreateRoom, and the one callers should use (§43).
//
// WHY idempotent, and why it matters more than it looks: every student of a lesson
// joins the SAME room, so "create the room" is executed once per join. A second
// create must not be able to fail the second student's join — the room they need is
// right there. Two ways this shows up in practice:
//
//   - The LiveKit server may answer `already exists` for a name that exists
//     (AlreadyExists, or the same thing expressed as a gRPC status depending on the
//     transport). That is the desired end state, so it is success, not an error.
//   - A teacher's media token (which also calls this) and a student's join can race.
//     Both want a room with the same name; both must succeed.
//
// Treating it as an error would make "the room already exists" — the normal case for
// everyone except the first participant — look like an outage, and the join endpoint
// would answer MEDIA_TOKEN_FAILED to a student whose room is perfectly fine.
func (c *Client) EnsureRoom(ctx context.Context, roomName string) (err error) {
	start := time.Now()
	defer func() { c.observeCall(metrics.MediaOperationEnsureRoom, start, err) }()
	err = c.createRoom(ctx, roomName)
	if err == nil || isAlreadyExists(err) {
		return nil
	}
	return err
}

// ObservedTrack is one track the media plane reported as published.
//
// It exists for the subscription rule of §26/§73 rather than for the teacher's wall:
// a revocation names a track by its LiveKit sid, and the Phase 10 whitelist ("keep the
// teacher's microphone") is expressed with a source. Nothing else about a track is
// carried, because nothing else may become a business fact.
type ObservedTrack struct {
	// Sid is LiveKit's id for this publication. It is opaque and short-lived: a track
	// that is unpublished and published again gets a new sid, which is why a revocation
	// is remembered per sid and never per (participant, source) pair.
	Sid string
	// Source is the kind of media in the vocabulary the token grants already use
	// (PublishSource): one word for "a kind of media" means a grant and an observation
	// can never disagree about what "microphone" is. An unrecognised source is reported
	// as "" — it matches no whitelist entry, so an unknown track fails towards
	// revocation rather than towards being kept.
	Source PublishSource
}

// ParticipantTracks is what the media plane reports about one participant: the media
// it is publishing now, and the tracks somebody could be subscribed to.
//
// It is a derived view (booleans per track source, plus a short track list) rather
// than a list of LiveKit track objects on purpose: the Control Plane needs to answer
// "is the screen up?" (§21/§51) and "which tracks must this student not be receiving?"
// (§26), and handing SDK structs to the session logic would make every future LiveKit
// field a potential business fact.
type ParticipantTracks struct {
	// ParticipantSid is LiveKit's id for this participant's CONNECTION — not its
	// identity and not a track. It changes when the same identity reconnects, which is
	// what lets the §26 bookkeeping notice "this student is on a new connection and may
	// have subscribed again". It is never rendered anywhere; identities are (§44).
	ParticipantSid string
	// ScreenShare is true when a screen-share track is published and not muted. §21
	// makes it the invariant behind ONLINE: a student who is ONLINE is sharing.
	ScreenShare bool
	// Camera and Microphone are always false in Phase 6 (only screen sharing is in
	// scope, §72). They are observed anyway because the monitor DTO of §51 reports
	// them today, and deriving them from the same snapshot later means the two cannot
	// disagree about what the room looked like at one instant.
	Camera     bool
	Microphone bool
	// Tracks is every track the participant has published, MUTED ONES INCLUDED, and it
	// answers a different question from the three booleans above: those say "is media
	// flowing right now?" (what the wall draws), this says "what could somebody be
	// subscribed to?" (what §26 must revoke). A muted track still carries a
	// subscription, and leaving it out would mean a peer subscription reappears the
	// moment somebody unmutes.
	Tracks []ObservedTrack
}

// ObserveRoom lists the participants of a room and what each one publishes.
//
// The result is keyed by livekit identity, which for this project is always
// `student_sessions.id` / the teacher's login session id (§44) — opaque UUIDs, never
// names. Callers map it onto business rows; it must never be rendered directly (§51).
//
// WHAT this is NOT: an authority. It is an observation of the media plane at one
// moment, and the control plane decides what it means (§33). A participant missing
// from this map is "not observed", which is why the caller must not treat a FAILED
// call as "everybody disconnected" — see internal/session's monitor.
func (c *Client) ObserveRoom(ctx context.Context, roomName string) (map[string]ParticipantTracks, error) {
	if err := validateOpaqueRoomName(roomName); err != nil {
		return nil, err
	}
	resp, err := c.rooms.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: roomName})
	if err != nil {
		if isRoomNotFound(err) {
			// A room that does not exist has nobody in it, which is an OBSERVATION and
			// not a failure.
			//
			// WHY this matters more than it looks (Phase 8 fix for a Phase 7 report
			// item): the teacher's console polls the monitor every few seconds, and
			// before the first student joins there is no LiveKit room at all — so the
			// naive version of this function turned "the room has not been created yet"
			// into a Warn on every poll. That trains operators to ignore the level, and
			// it makes a real media-plane outage indistinguishable from an empty
			// classroom. An empty map is also the honest answer for the state machine:
			// nobody is connected, so a session that claims ONLINE is not.
			//
			// The distinction is preserved where it matters: a genuine failure (the API
			// key was rotated, LiveKit is down, the network is broken) still returns an
			// error, and the monitor then reports connection=UNKNOWN instead of
			// inventing a disconnect (§33).
			return map[string]ParticipantTracks{}, nil
		}
		return nil, fmt.Errorf("livekit: list participants: %w", err)
	}
	observed := make(map[string]ParticipantTracks, len(resp.GetParticipants()))
	for _, participant := range resp.GetParticipants() {
		// LiveKit keeps a participant in the list for a short while after its
		// connection dropped, with state DISCONNECTED. Counting that as "present"
		// would keep a student who closed their laptop ONLINE until the entry is
		// reaped, which is exactly the kind of false "still being supervised" the
		// monitoring wall must not display.
		if !isConnectedState(participant.GetState()) {
			continue
		}
		tracks := ParticipantTracks{ParticipantSid: participant.GetSid()}
		for _, track := range participant.GetTracks() {
			// The track list is built first and unconditionally: §26 revocation works on
			// PUBLICATIONS (something a client can be subscribed to), not on flowing media.
			// See ParticipantTracks.Tracks.
			if sid := track.GetSid(); sid != "" {
				tracks.Tracks = append(tracks.Tracks, ObservedTrack{Sid: sid, Source: observedSource(track.GetSource())})
			}
			// A muted track is published but carries no media — a muted camera or a
			// muted screen share is not "active" in any sense the teacher's wall
			// cares about.
			if track.GetMuted() {
				continue
			}
			switch observedSource(track.GetSource()) {
			case PublishScreenShare:
				tracks.ScreenShare = true
			case PublishCamera:
				tracks.Camera = true
			case PublishMicrophone:
				tracks.Microphone = true
			}
		}
		observed[participant.GetIdentity()] = tracks
	}
	return observed, nil
}

// observedSource maps a LiveKit track source onto the project's own vocabulary.
//
// It is the inverse of token.go's trackSources, and it is deliberately a total
// function with an unnamed default: a source this project does not publish (for
// example a data track) becomes "", which matches no publish source and therefore no
// whitelist entry. Failing towards "unknown" is what keeps a future LiveKit source
// from silently counting as media some role is allowed to keep (§26).
func observedSource(source livekit.TrackSource) PublishSource {
	switch source {
	case livekit.TrackSource_SCREEN_SHARE:
		return PublishScreenShare
	case livekit.TrackSource_CAMERA:
		return PublishCamera
	case livekit.TrackSource_MICROPHONE:
		return PublishMicrophone
	default:
		return ""
	}
}

// RemoveParticipant disconnects one participant from a room.
//
// It is the media-plane half of "student leaves" (§50/§43): with the identity gone,
// the room's tiles and subscriptions disappear immediately instead of waiting for the
// client to close its own peer connection. A participant who is already gone is
// success — the caller asked for an end state, not for an action.
//
// WHY this cannot be the control-plane truth: disconnecting a participant says
// nothing about whether the student left the LESSON. That is the `student_sessions`
// row, written by the leave endpoint before this call (§33/§49).
func (c *Client) RemoveParticipant(ctx context.Context, roomName, identity string) error {
	if err := validateOpaqueRoomName(roomName); err != nil {
		return err
	}
	if !isOpaqueIdentity(identity) {
		// Refusing here is a safety net for §44: a name, account or phone number must
		// never be able to reach the media plane, not even as a lookup key.
		return fmt.Errorf("livekit: participant identity must be an opaque uuid")
	}
	if _, err := c.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     roomName,
		Identity: identity,
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("livekit: remove participant: %w", err)
	}
	return nil
}

// isConnectedState reports whether a participant still holds a media connection.
//
// JOINING and JOINED are the transient states of a participant that is connecting;
// treating them as absent would make a student who is one second away from being
// online look disconnected on every poll.
func isConnectedState(state livekit.ParticipantInfo_State) bool {
	switch state {
	case livekit.ParticipantInfo_JOINING, livekit.ParticipantInfo_JOINED, livekit.ParticipantInfo_ACTIVE:
		return true
	default:
		return false
	}
}

// Room name and participant policy, named so the reasoning is greppable.
const (
	// emptyRoomTimeoutSeconds: a room with no participants is dropped after this
	// many seconds, so a crash between "create room" and "students join" cannot
	// leave a joinable room behind.
	emptyRoomTimeoutSeconds = 300
	// departureTimeoutSeconds: after the last participant leaves, wait a short
	// while before destroying the room. This lets a student who reloaded the page
	// rejoin the same run instead of creating a second media session.
	departureTimeoutSeconds = 20
	// maxParticipantsPerRoom is a safety bound, not an authorization rule: the
	// real gate is classroom assignment, checked in the Control Plane.
	maxParticipantsPerRoom = 200
)

// validateOpaqueRoomName rejects names that would leak human identity into the
// media plane, and empty names, which the LiveKit API would otherwise auto-fill.
func validateOpaqueRoomName(roomName string) error {
	if strings.TrimSpace(roomName) == "" {
		return fmt.Errorf("livekit: room name is required")
	}
	if roomName != strings.TrimSpace(roomName) {
		return fmt.Errorf("livekit: room name must not contain surrounding whitespace")
	}
	// The expected Phase 3 shape is lk_<uuid>. Enforcing a prefix now keeps a
	// future change from quietly switching to "class-3-math-zhangsan".
	if !strings.HasPrefix(roomName, roomNamePrefix) {
		return fmt.Errorf("livekit: room name must start with %q and stay opaque", roomNamePrefix)
	}
	// The prefix alone is not a room: a name that carries nothing after it would be a
	// single shared room for the whole deployment, which is precisely the collision the
	// per-run naming exists to prevent.
	if strings.TrimPrefix(roomName, roomNamePrefix) == "" {
		return fmt.Errorf("livekit: room name must be %s<opaque id>, not the prefix alone", roomNamePrefix)
	}
	return nil
}

// roomNamePrefix marks every ClassWatch media room so LiveKit-side tooling can
// tell our rooms apart from anything else sharing the deployment.
const roomNamePrefix = "lk_"

// isOpaqueIdentity accepts only a UUID-shaped participant identity (§44).
//
// WHY a UUID check and not "not empty": identity is the one string of ours visible
// to every participant in the room and to the LiveKit dashboard. The rule "identities
// are opaque UUIDs" is worth more as a check than as a comment, because the call site
// that would break it (a debugging shortcut passing a student's account) looks
// harmless at the moment it is written.
func isOpaqueIdentity(identity string) bool {
	_, err := uuid.Parse(identity)
	return err == nil
}

// isAlreadyExists recognises "the room is already there" across the two error
// shapes the LiveKit client can produce.
//
// WHY both a code check and a message check: the RoomService API is invoked over
// Twirp (HTTP) in this SDK, and the server may express the condition as an
// AlreadyExists code or merely as text, depending on version and transport. The code
// check is the real rule; the substring check exists so a version that changes only
// the code mapping cannot turn a routine second join into a failed one. A false
// positive here is harmless — the room does exist, which is all the caller asserts.
func isAlreadyExists(err error) bool {
	return matchesServerError(err,
		func(err twirp.Error) bool { return err.Code() == twirp.AlreadyExists },
		func(code codes.Code) bool { return code == codes.AlreadyExists },
		"already exists",
	)
}

// isNotFound recognises "there is no such participant", which for
// RemoveParticipant is the desired end state rather than a failure.
func isNotFound(err error) bool {
	return matchesServerError(err,
		func(err twirp.Error) bool { return err.Code() == twirp.NotFound },
		func(code codes.Code) bool { return code == codes.NotFound },
		"not found", "does not exist",
	)
}

// isRoomNotFound recognises "there is no such room" from ListParticipants.
//
// It is deliberately narrower than isNotFound: the message fallback mentions the room,
// so an unrelated NotFound (a track, a participant, a future endpoint) cannot be read as
// "the classroom is empty". The code check is the primary rule — LiveKit answers this
// condition with NotFound — and the text is the fallback for a transport that expresses
// it differently.
func isRoomNotFound(err error) bool {
	return matchesServerError(err,
		func(err twirp.Error) bool { return err.Code() == twirp.NotFound },
		func(code codes.Code) bool { return code == codes.NotFound },
		"room not found", "no such room", "room does not exist",
	)
}

// matchesServerError reports whether err is a Twirp/gRPC error the caller
// classifies as "this specific, expected condition".
//
// The message check is case-insensitive and deliberately last: it is the fallback for
// a mapping change, not the primary rule.
func matchesServerError(err error, byTwirp func(twirp.Error) bool, byCode func(codes.Code) bool, messages ...string) bool {
	if err == nil {
		return false
	}
	var twirpErr twirp.Error
	if errors.As(err, &twirpErr) && byTwirp(twirpErr) {
		return true
	}
	if byCode(status.Code(err)) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, message := range messages {
		if strings.Contains(text, message) {
			return true
		}
	}
	return false
}

// NOTE on token issuance (Phase 6): minting LiveKit join tokens is deliberately
// NOT implemented here. A token's grants are the security-critical part — room
// name, participant identity, and only the publish/subscribe permissions the role
// requires — and shipping a half-configured issuer now would invite a later
// caller to reuse it with the wrong grants. It lands in Phase 6 together with
// session creation, so identity, permissions and expiry are decided in one place.
