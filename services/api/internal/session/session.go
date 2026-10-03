// Package session implements the student media session of §12: one student's
// connection to one ClassroomRun.
//
// # What this is, and what it is not
//
// It is NOT the login session of §41 (see internal/auth/sessionstore). A login
// session authenticates a browser; a student session records that a browser is (or
// was) in a media room of one lesson. Both are UUIDs and both are opaque, but only
// the login session is a credential — which is why this package never touches a
// cookie and never mints a login token.
//
// # The one rule that shapes everything here
//
// The session is advanced by SERVER-SIDE OBSERVATION, never by what the client says
// (§45). A browser that reports "I am online, my screen is shared" is a UX hint; the
// control plane believes only what it can see itself — in Phase 6 by asking the media
// plane (ListParticipants), from Phase 8 by consuming signed LiveKit webhooks (§74).
// The student frontend therefore has no "report my status" endpoint at all, which is
// the strongest version of this rule: there is nothing to lie to.
//
// # Where the state lives
//
// `student_sessions` in PostgreSQL (§33). A LiveKit room being alive says nothing
// about a lesson; a row saying ONLINE is what the teacher's wall renders, and it
// survived a LiveKit restart because it was never derived from LiveKit.
package session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// Status is the V1 session state of §12. There are exactly six, and there is no
// PRE_JOIN: that one belongs to the student's browser (the screen gate of §16/§18),
// and writing it to the database would record a media connection that nobody has
// attempted yet.
type Status string

const (
	// StatusConnecting means a session row exists and the control plane has not yet
	// observed the participant publishing a screen. It is the state a join leaves
	// behind, and it is honest: a token was issued, a connection may or may not
	// follow.
	StatusConnecting Status = "CONNECTING"
	// StatusOnline means the invariant of §21 holds as of the last observation:
	// ONLINE ⇒ a screen-share track exists. Nothing may set ONLINE from a client
	// claim, and the monitor's transition function is the only writer.
	StatusOnline Status = "ONLINE"
	// StatusScreenLost means the participant is in the room but is no longer sharing
	// a screen (§22). The student is still supervised; the media that was being
	// supervised is gone, and the teacher must see exactly that.
	StatusScreenLost Status = "SCREEN_LOST"
	// StatusDisconnected means the participant is not in the room. It is still an
	// ACTIVE state (§50): a student whose wifi dropped has not left the lesson, and
	// their rejoin reuses the same session row and the same media identity.
	StatusDisconnected Status = "DISCONNECTED"
	// StatusLeft means the student said so (the leave endpoint). Terminal.
	StatusLeft Status = "LEFT"
	// StatusRoomClosed means the teacher ended the lesson. Terminal. Phase 8 sets it
	// inside the close flow (§49); Phase 6 leaves the column free for it.
	StatusRoomClosed Status = "ROOM_CLOSED"
)

// Active reports whether a session in this state still occupies the student's one
// active session slot for the run, which is what the partial unique index and the
// reuse rule of §50 are written against.
//
// DISCONNECTED counts as active on purpose: treating a dropped connection as "gone"
// would let a reconnecting student insert a second row, and the wall would grow a
// second tile of the same person.
func (s Status) Active() bool {
	switch s {
	case StatusConnecting, StatusOnline, StatusScreenLost, StatusDisconnected:
		return true
	default:
		return false
	}
}

// Terminal reports whether the state is final for this run.
//
// WHY the distinction exists in code and not only in a comment: the monitor folds new
// observations into sessions, and a terminal session must ignore them. A student who
// left keeps LEFT even if their browser is still connected for a few more seconds,
// because "left" is a product decision that already happened (§22/§50).
func (s Status) Terminal() bool { return s == StatusLeft || s == StatusRoomClosed }

// Connection is the coarse quality of the media connection as the control plane can
// currently attest to it (§51).
//
// WHY there are only two values in Phase 6: the honest answers are "the last
// observation looked complete" and "there is no current observation". Latency, jitter
// and packet loss are Phase 7/8 work (LiveKit reports them per track), and inventing
// a "POOR" now would mean the teacher's wall shows a quality judgement that nothing
// measured.
type Connection string

const (
	// ConnectionGood means the participant was observed in the room with a screen
	// track published.
	ConnectionGood Connection = "GOOD"
	// ConnectionUnknown means the control plane has no usable observation, either
	// because the media plane could not be queried at all or because nothing of this
	// participant was seen. The field exists so a failure of the MEDIA plane is never
	// rendered as a fact about the STUDENT (§33).
	ConnectionUnknown Connection = "UNKNOWN"
)

// StudentSession is one row of `student_sessions`.
type StudentSession struct {
	ID             uuid.UUID
	ClassroomRunID uuid.UUID
	StudentID      uuid.UUID
	// LiveKitIdentity is the participant identity in the media room. It equals ID
	// (the database enforces it), and the duplication is deliberate: the session id
	// identifies a control-plane record, the identity identifies a participant, and a
	// future phase that needs them to differ has to notice this comment.
	LiveKitIdentity string
	Status          Status
	// ConnectedAt is when the participant was first observed in the room. NULL means
	// "never actually connected", which is how a session stuck in CONNECTING is
	// recognisable — the difference between "the browser never made it" and "the
	// connection dropped" matters when a student says "I was in the whole time".
	ConnectedAt     *time.Time
	ScreenStartedAt *time.Time
	ScreenLostAt    *time.Time
	LeftAt          *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NOTE: a StudentSession carries no display name. The name belongs to the PERSON, not to
// one of their sessions, and Phase 7's monitoring read gets both from the roster
// (RosterEntry.DisplayName) — a field here would be a second copy that only one query
// fills, which is the kind of empty column a later reader mistakes for data.

// MonitorStudent is one tile of the teacher's monitoring wall (§51).
//
// It is the JOIN of two sources and the type says so: identity and status come from
// PostgreSQL, screen/camera/microphone/connection come from the last media-plane
// observation. The teacher UI must never receive a raw LiveKit participant as its
// model — a participant has no display name, no lesson, and no history, so a wall
// built from participants shows the wrong things after a reconnect.
//
// # Why SessionID and Status are pointers
//
// The list is the roster, not the session table (§29's "18 / 25"): a student who was
// authorized for the lesson but never pressed "进入课堂" is a tile the teacher must see
// — "who has not come in yet" is half of what a supervision wall answers. That student
// has no session, so the two session-shaped members are null rather than a zero UUID or
// an invented seventh status. A nil here is not "unknown": it is "there is none".
type MonitorStudent struct {
	StudentID   uuid.UUID
	DisplayName string
	// SessionID is the student's session in this run, or nil when they have none. It is
	// also the LiveKit identity (§44), which is how the frontend finds the participant.
	SessionID *uuid.UUID
	// Status is the session state, or nil when SessionID is nil. It is never a second
	// vocabulary for "not in": the six states of §12 are the whole set.
	Status *Status
	// ScreenActive, CameraActive and MicrophoneActive are the observed tracks: the
	// media plane reported a publication of that source and did not report it muted.
	// Camera and microphone can be true next to any session status (§24/§25/§21: neither
	// changes the status — only the screen does), and a student without a session has all
	// three false.
	ScreenActive     bool
	CameraActive     bool
	MicrophoneActive bool
	Connection       Connection
	// JoinedAt is when the control plane first observed this student in the room, or
	// nil while that has not happened (see StudentSession.ConnectedAt).
	JoinedAt *time.Time
	// LastEventAt is the time of the most recent recorded change to this session. In
	// Phase 6 that is `updated_at`; Phase 8 replaces it with the newest
	// `session_events.created_at` for the session (§74), which is why the field is
	// named after the event and not after the column.
	LastEventAt *time.Time
}

// MonitorView is the whole response of the monitoring endpoint.
//
// MediaObserved is not part of the HTTP contract; it is what the service reports to
// its own caller and its tests. It exists because "the media plane answered and
// nobody is connected" and "the media plane did not answer" are the same JSON today
// and must never be the same decision (§33).
type MonitorView struct {
	Students      []MonitorStudent
	MediaObserved bool
}

// RosterEntry is one line of the monitoring wall's source: a student the classroom
// authorizes, and — when they have one — their session in the current run.
//
// WHY the roster drives the wall and not the session table (§29): the header of the
// console is "18 / 25", and a denominator cannot be computed from the students who
// already joined. The console also has to render the tile of somebody who has not come
// in yet ("未进入"), which is a state a session-first read simply cannot express.
//
// Session is a pointer for exactly that reason: nil is "this student was authorized for
// this lesson and never entered", which is a fact about the lesson, not missing data.
type RosterEntry struct {
	StudentID   uuid.UUID
	DisplayName string
	// Session is the student's session in this run, or nil when there is none.
	Session *StudentSession
	// LastEventAt is when the newest `session_events` row of that session was written,
	// or nil when the session has no events yet (or no session at all).
	//
	// WHY it lives on the ENTRY and not on StudentSession: it is an extra column of the
	// monitoring query, not a property of the session row, and putting it on the struct
	// every other query returns would mean a field that is nil in every context except
	// one — which is exactly the kind of half-populated field a later reader trusts.
	LastEventAt *time.Time
}

// Sentinel errors, mapped to the API error codes of §58 in internal/httpapi.
var (
	// ErrClassroomClosed means the classroom is not OPEN or has no current run. It is
	// the answer of BOTH the join and the teacher-token endpoint (§43): no media
	// session can exist outside an open lesson, whatever the client believes.
	ErrClassroomClosed = errors.New("session: classroom is not open")

	// ErrSessionNotFound means the caller does not own that session, or it does not
	// exist. The two collapse on purpose: a student who could tell them apart could
	// probe other students' session ids, and there is no product question that needs
	// the difference (§58).
	ErrSessionNotFound = errors.New("session: session not found")

	// ErrMediaUnavailable means the control plane could not reach the media plane, so
	// it will not hand out a token it cannot honour. The state that produced it is not
	// the caller's fault and retrying can genuinely succeed — hence 502
	// MEDIA_TOKEN_FAILED and not 4xx (§58).
	ErrMediaUnavailable = errors.New("session: media plane unavailable")
)

// Config is the media-plane configuration this package needs.
//
// The LiveKit URL and the token TTL are configuration of the MEDIA plane handed out
// by the CONTROL plane, which is why they arrive as values rather than being read
// from the environment here: one validated source of configuration (internal/config)
// and one place that decides what a token may do (this package).
type Config struct {
	// LiveKitURL is the browser-facing WebSocket endpoint (wss://...). It is returned
	// to the client next to the token because the client needs both to connect, and
	// it is NOT a secret — the token is.
	LiveKitURL string
	// TokenTTL is how long an issued token stays valid. Short by policy (§63), long
	// enough to outlive one lesson: see the note on refresh in docs/media.
	TokenTTL time.Duration
}

// Directory is the slice of the classroom domain a session needs.
//
// WHY an interface declared here, implemented by *classroom.Service: this package
// must not be able to open, close or edit a classroom, and the compiler is a better
// reviewer than a comment. It also lets the session rules be unit tested against a
// fake classroom, which is what keeps the tests fast and the failures precise.
type Directory interface {
	// StudentEntry is the roster-authorized read of a classroom and its current run,
	// including the LiveKit room name.
	StudentEntry(ctx context.Context, studentID, classroomID uuid.UUID) (*classroom.StudentEntry, error)
	// Get loads a classroom for its owner, or returns classroom.ErrNotFound /
	// classroom.ErrNotOwner.
	Get(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error)
	// RunByID resolves the run a session belongs to, which is how the leave path
	// learns the media room to disconnect the participant from.
	RunByID(ctx context.Context, runID uuid.UUID) (*classroom.Run, error)
}

// MediaPlane is the slice of the media plane a session needs.
//
// It maps one-to-one onto media.Client, and it is declared here so a unit test can
// drive the state machine of §51 without a LiveKit server — which matters, because
// the interesting failures (the room list is unavailable, a participant vanished, a
// track disappeared) are exactly the ones that are hard to produce on demand against
// a real SFU.
type MediaPlane interface {
	// EnsureRoom creates the room if it does not exist yet; it is idempotent.
	EnsureRoom(ctx context.Context, roomName string) error
	// ObserveRoom reports which participants are in the room and what they publish.
	ObserveRoom(ctx context.Context, roomName string) (map[string]media.ParticipantTracks, error)
	// RemoveParticipant disconnects one participant, tolerating "already gone".
	RemoveParticipant(ctx context.Context, roomName, identity string) error
	// SignToken mints a participant-scoped token.
	SignToken(req media.TokenRequest) (string, error)
	// EnforceNoPeerSubscriptions revokes the subscriptions students still hold to
	// classmates' tracks (§26). It takes the observation the caller already made, so
	// the reconciliation costs no extra room query, and it returns what it revoked (and
	// what it failed to revoke) instead of logging: the caller owns the classroom
	// context and the project's log vocabulary.
	EnforceNoPeerSubscriptions(
		ctx context.Context,
		roomName string,
		students []string,
		observed map[string]media.ParticipantTracks,
		allowedTrackOwners []string,
	) ([]media.PeerSubscriptionRevocation, error)
	// EnforcePrivateTalk converges the room on §31's rule: the target student (when there
	// is one) subscribes to the teacher's microphone tracks, and every other student does
	// not. It takes the same observation as the call above — one room query per poll — and
	// it is the ONLY way this control plane can take the teacher's audio away from a
	// student, whose token grants `canSubscribe=true` room-wide (§28).
	//
	// An empty targetIdentity is not "skip": it means the room has no private talk, which
	// is the state every student must be converged to.
	EnforcePrivateTalk(
		ctx context.Context,
		roomName string,
		students []string,
		observed map[string]media.ParticipantTracks,
		teacherIdentities []string,
		targetIdentity string,
	) (media.PrivateTalkEnforcement, error)
}

// CreateOrReuseParams is the input of Repository.CreateOrReuse.
//
// SessionID is minted by the service and not by the database for two reasons: it is
// logged even when the INSERT fails (the same reasoning as auth's session creation),
// and it is the value media identities are made of, so it must exist before the row
// does.
type CreateOrReuseParams struct {
	SessionID      uuid.UUID
	ClassroomRunID uuid.UUID
	StudentID      uuid.UUID
}

// ObservationChange is one state advance the service decided to persist.
//
// It carries the state it read (From) as well as the one it wants (To) because the
// write is a compare-and-set: if the row moved in between — a student pressed leave
// while the monitor was polling — the update must match nothing and leave the newer
// decision alone. Without that guard, a poll that started before a leave could
// resurrect a finished session.
type ObservationChange struct {
	SessionID uuid.UUID
	From      Status
	To        Status
	// MarkConnected and MarkScreenStarted write a timestamp only if the column is
	// still NULL: they record the FIRST time something happened, and overwriting them
	// would erase "when did this student's screen first come up?" — the question a
	// lesson report asks.
	MarkConnected     bool
	MarkScreenStarted bool
	// MarkScreenLost OVERWRITES the column, because the teacher's wall shows the most
	// recent loss ("屏幕共享已停止 20:31:42" §22), not the first one.
	MarkScreenLost bool
}

// Repository is the persistence contract of the session domain.
type Repository interface {
	// CreateOrReuse returns the student's active session for the run, creating one if
	// there is none. It is the §50 rule expressed as a single statement: one active
	// session per (run, student), reused across reconnects, never duplicated.
	CreateOrReuse(ctx context.Context, params CreateOrReuseParams) (*StudentSession, error)
	// Leave marks the caller's own session LEFT and returns it. It is idempotent: a
	// session that is already LEFT is returned unchanged, because a retried request
	// (or a double click) is not an error. A session that is not the caller's is
	// ErrSessionNotFound.
	Leave(ctx context.Context, sessionID, studentID uuid.UUID) (*StudentSession, error)
	// ListRosterByRun returns every student the classroom authorizes, each with their
	// session in this run or nil when they have none (§29/§51). The order is by account
	// and is stable across polls: a supervision wall whose tiles move while a teacher is
	// looking at it is worse than useless.
	ListRosterByRun(ctx context.Context, classroomID, runID uuid.UUID) ([]RosterEntry, error)
	// ApplyObservation persists one transition, guarded by the status it was computed
	// from. It returns the stored row, or nil when the guard did not match (the row
	// moved on) — nil is not an error, it is "your observation is stale".
	ApplyObservation(ctx context.Context, change ObservationChange) (*StudentSession, error)
}
