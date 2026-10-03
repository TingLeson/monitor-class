package session

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EventType is the event vocabulary of §13.
//
// It is one of three places that must agree: this list, the CHECK constraint in
// migrations/0008_session_events.sql, and the frontend's SessionEventType. Adding a
// type therefore means a migration, which is the review conversation a new kind of
// "what happened" deserves — an event row is a fact somebody will be asked to
// explain months later.
type EventType string

const (
	// EventSessionCreated records that a student entered a lesson (a session row was
	// created or a terminal one replaced). It is written by the join endpoint, not by
	// the webhook: the browser asked to enter, and the control plane recorded it.
	EventSessionCreated EventType = "SESSION_CREATED"
	// EventParticipantConnected records the first time the media plane reported the
	// participant in the room (§45). It does NOT mean ONLINE: only a screen track does.
	EventParticipantConnected EventType = "PARTICIPANT_CONNECTED"
	// EventScreenPublished records the transition into ONLINE caused by a SCREEN_SHARE
	// track being published — the authoritative observation of §45/§21.
	EventScreenPublished EventType = "SCREEN_PUBLISHED"
	// EventScreenLost records ONLINE → SCREEN_LOST (§22): the student is still in the
	// room, the media being supervised is gone.
	EventScreenLost EventType = "SCREEN_LOST"
	// EventScreenRestored records SCREEN_LOST → ONLINE after a new whole-screen share.
	EventScreenRestored EventType = "SCREEN_RESTORED"
	// EventCameraStarted and EventCameraStopped belong to Phase 9 (§75). They are part
	// of the vocabulary now because the CHECK constraint is the contract the frontend
	// is written against, and a later migration to widen it would break that contract
	// in the middle of a lesson.
	EventCameraStarted EventType = "CAMERA_STARTED"
	EventCameraStopped EventType = "CAMERA_STOPPED"
	// EventMicStarted and EventMicStopped belong to Phase 10 (§76), same reasoning.
	EventMicStarted EventType = "MIC_STARTED"
	EventMicStopped EventType = "MIC_STOPPED"
	// EventConnectionLost records that an active session lost its media connection
	// (participant_left / participant_connection_aborted, §45).
	EventConnectionLost EventType = "CONNECTION_LOST"
	// EventConnectionRestored records that a DISCONNECTED participant came back into
	// the room. It says the CONNECTION returned, not that the screen is up.
	EventConnectionRestored EventType = "CONNECTION_RESTORED"
	// EventTeacherTalkStarted and EventTeacherTalkEnded belong to Phase 10 (§76).
	EventTeacherTalkStarted EventType = "TEACHER_TALK_STARTED"
	EventTeacherTalkEnded   EventType = "TEACHER_TALK_ENDED"
	// EventStudentLeft records that the student said they were leaving (the leave
	// endpoint, §43/§50). Terminal for the session row.
	EventStudentLeft EventType = "STUDENT_LEFT"
	// EventRoomClosed records that the lesson ended for this session: the teacher
	// closed the classroom (§49) or the media room finished (§45). Terminal.
	EventRoomClosed EventType = "ROOM_CLOSED"
)

// SessionEvent is one row of `session_events` (§13).
//
// It carries no student name, no room secret and no media: see the migration's
// comment on `payload` for what may and may not be stored (§59).
type SessionEvent struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	Type      EventType
	// Payload is diagnostic metadata (opaque room name, track sid/source, participant
	// sid, webhook event id, the reason a transition applied or was skipped). It is
	// never rendered to a user and never carries media (§13/§53).
	Payload   map[string]any
	CreatedAt time.Time
}

// Transition is one guarded state change of a session row, plus the event row that
// records it.
//
// WHY the guard is a list of statuses and not just one: several media-plane
// observations mean the same thing (a screen track appearing moves CONNECTING,
// DISCONNECTED and SCREEN_LOST all to ONLINE), and expressing that as one statement
// keeps the compare-and-set atomic — "read the status, decide, write" would race with
// the leave endpoint.
type Transition struct {
	SessionID uuid.UUID
	// From is the CAS guard: the update applies only if the row is still in one of
	// these states. An observation computed from a state the row has left is stale and
	// must not overwrite the newer decision (§74).
	From []Status
	To   Status
	// MarkConnected and MarkScreenStarted write a timestamp only if it is still NULL:
	// both answer "when did this FIRST happen?", and a reconnect must not move them.
	MarkConnected     bool
	MarkScreenStarted bool
	// MarkScreenLost OVERWRITES: the teacher's wall shows the most recent loss (§22).
	MarkScreenLost bool
	// OnlyIfNeverConnected narrows the guard to a session whose connected_at is NULL.
	// It is what makes a repeated `participant_joined` a no-op instead of a second
	// PARTICIPANT_CONNECTED row (the state is unchanged, so the status guard alone
	// would match twice).
	OnlyIfNeverConnected bool
	// Event is the event type appended in the same transaction, and Payload its
	// diagnostic metadata. An empty Event appends nothing, which is only correct for a
	// transition that is not worth recording as an event (nothing needs it today).
	Event   EventType
	Payload map[string]any
}

// Applied reports whether the transition is worth writing at all.
func (t Transition) Applied() bool { return len(t.From) > 0 && t.To != "" }

// TransitionResult is the outcome of one Transition.
//
// A nil Session with no error is "the guard did not match": the row moved on, or the
// observation had already been applied. That is a normal outcome of duplicate and
// out-of-order webhook delivery, not a failure (§45/§74).
type TransitionResult struct {
	Session *StudentSession
	// Event is the event row that was appended, or nil when the guard did not match
	// (nothing happened, so nothing is recorded).
	Event *SessionEvent
}

// EventStore is the persistence the runtime event path needs.
//
// WHY it is separate from Repository: the polling half of this package (join, leave,
// roster, monitor) has no business appending events, and a single wide interface would
// force every fake in the existing tests to grow methods it never calls. *Postgres
// implements both.
type EventStore interface {
	// SessionInRoomByIdentity resolves a media identity to the session it belongs to,
	// INSIDE one media room, or nil when the identity is not a student session of that
	// room.
	//
	// The room is part of the lookup on purpose: a teacher's identity is a login
	// session id that is not in this table, and an identity that belongs to another
	// run must never be able to move a state in this one. A nil result is the answer
	// for both, and it is what the webhook handler logs and ignores (§45).
	SessionInRoomByIdentity(ctx context.Context, roomName, identity string) (*StudentSession, error)

	// RunByRoomName returns the run a media room belongs to, or false when the room is
	// not ours. room_started/room_finished name a room and nothing else, so this is how
	// those two events find their way to a lesson.
	RunByRoomName(ctx context.Context, roomName string) (ClassroomRef, bool, error)

	// ClassroomRefByRunID is the same lookup from the other side, for the close flow,
	// which starts from the run the teacher just closed.
	ClassroomRefByRunID(ctx context.Context, runID uuid.UUID) (ClassroomRef, bool, error)

	// ApplyTransition persists one guarded change and appends its event row in the
	// same transaction. A guard that does not match returns an empty result.
	ApplyTransition(ctx context.Context, t Transition) (*TransitionResult, error)

	// CloseRunSessions marks every ACTIVE session of a run ROOM_CLOSED and appends one
	// event per row, in one transaction (§49/§45). It returns the sessions it closed,
	// which is empty when they were already terminal — the normal case for the
	// room_finished webhook that follows a teacher close.
	CloseRunSessions(ctx context.Context, runID uuid.UUID, event EventType, payload map[string]any) ([]StudentSession, error)

	// RecordEventOnce appends one event of a given type for a session, unless that
	// (session, type) pair already has one. It reports whether THIS call appended it.
	//
	// WHY "once per type" is the right guard for SESSION_CREATED and STUDENT_LEFT: both
	// describe something that can happen at most once in a session's life (a session
	// row is created once; LEFT is terminal and a re-entry creates a NEW row, §50). The
	// guard therefore cannot suppress a legitimate second event, and it makes a
	// double-clicked join, a retried leave or a retried request a no-op instead of
	// duplicate history.
	RecordEventOnce(ctx context.Context, sessionID uuid.UUID, event EventType, payload map[string]any) (bool, error)
}

// ClassroomRef is the lesson a media room belongs to, as the webhook path needs it:
// the run whose sessions must be closed, and the classroom whose students and owner
// must be told.
type ClassroomRef struct {
	RunID       uuid.UUID
	ClassroomID uuid.UUID
}

// SessionRef identifies the session a runtime message is about.
//
// WHY a type and not three uuid arguments: the three ids look alike, are all UUIDs,
// and swapping two of them produces a message about the wrong student that no
// compiler would catch. Naming them keeps the mistake impossible at the call site and
// visible in the logs.
type SessionRef struct {
	SessionID uuid.UUID
	StudentID uuid.UUID
	RunID     uuid.UUID
}

// Ref builds the reference of a session.
func (s *StudentSession) Ref() SessionRef {
	if s == nil {
		return SessionRef{}
	}
	return SessionRef{SessionID: s.ID, StudentID: s.StudentID, RunID: s.ClassroomRunID}
}

// OfflineReason is why a student is no longer being supervised, as the WebSocket
// contract spells it (§47).
type OfflineReason string

const (
	// OfflineDisconnected means the media connection dropped (§45).
	OfflineDisconnected OfflineReason = "DISCONNECTED"
	// OfflineLeft means the student chose to leave (§43/§50).
	OfflineLeft OfflineReason = "LEFT"
	// OfflineRoomClosed means the lesson ended (§49).
	OfflineRoomClosed OfflineReason = "ROOM_CLOSED"
)

// SessionEvents is the runtime layer as the session domain sees it (§47/§74).
//
// The methods are the messages a state change produces, named after the FACT and not
// after the message type: which envelope a fact becomes, and who is allowed to receive
// it, is the realtime layer's decision (§26) — this package must not be able to widen
// an audience by passing a recipient list.
//
// Every method returns an error because a broadcast can fail (the audience is resolved
// from the database). Callers log it and carry on: the state change is already
// committed and the media plane's failure must not roll back the control plane (§33).
type SessionEvents interface {
	// RoomOpened announces a new ClassroomRun to the students it authorizes (§48).
	RoomOpened(ctx context.Context, classroomID, runID uuid.UUID) error
	// RoomClosed announces the end of a run to its students and to its owner (§49).
	RoomClosed(ctx context.Context, classroomID, runID uuid.UUID) error
	// StudentOnline tells the owner that a student is being supervised (§47).
	StudentOnline(ctx context.Context, ref SessionRef) error
	// StudentOffline tells the owner that a student no longer is, and why.
	StudentOffline(ctx context.Context, ref SessionRef, reason OfflineReason) error
	// ScreenLost and ScreenRestored tell the owner AND the student themself (§47/§26).
	ScreenLost(ctx context.Context, ref SessionRef) error
	ScreenRestored(ctx context.Context, ref SessionRef) error
}

// LifecycleEvents is what the join and leave endpoints tell the runtime layer (§74).
//
// Both are fire-and-forget from the caller's point of view: the endpoint has already
// answered from the database, and a failing event insert must not turn a successful
// join into an error the student cannot act on.
type LifecycleEvents interface {
	// SessionCreated records that a student entered a lesson (§13).
	SessionCreated(ctx context.Context, session *StudentSession) error
	// SessionLeft records that a student left and tells the teacher (§13/§47).
	SessionLeft(ctx context.Context, session *StudentSession) error
}
