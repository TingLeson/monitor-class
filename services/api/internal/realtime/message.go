// Package realtime is the Business Realtime layer of §47: the WebSocket connections
// the two frontends keep open, the frozen message envelope they receive, and the
// scoping rules that decide who may receive what (§26).
//
// # Why business messages do not travel over WebRTC DataChannels
//
// The media plane and the control plane are separate (§33), and a control message that
// rides the media plane inherits the media plane's failure modes: it needs a LiveKit
// room to exist, a participant to be connected and a data channel to be negotiated —
// so "the teacher closed this lesson" could not be delivered to a student whose room
// had already been terminated (which is exactly when it matters most). A WebSocket
// authenticated by the same session cookie as the rest of the API keeps the control
// plane independent: it works before a media connection exists, after it is gone, and
// while it is broken.
//
// # What lives here
//
//   - Message: the envelope, frozen by contract, that the frontends are written
//     against.
//   - Hub: the process-local registry of live connections and the only thing that
//     writes to a socket. Its API is scoped by construction — there is no method that
//     sends to "everyone", so a broadcast cannot leak a student's state to another
//     student (§26).
//   - Service: resolves WHO a fact is about (the classroom roster, the owner teacher)
//     and asks the Hub to deliver. This is where a message's audience is decided.
//
// # Phase 8 limitation, stated up front
//
// The Hub is in-process (§52 assumes a single API instance). With two API replicas, a
// webhook handled by replica A reaches only the clients connected to replica A. The
// seam for fixing that is Broadcaster: replacing the Hub with a Redis pub/sub adapter
// (or NATS, which the dependency graph already carries through LiveKit) is a wiring
// change in cmd/api, not a change to the services. Phase 11/12 does that; doing it now
// would add a broker dependency to a phase whose subject is the state machine, and the
// single-instance assumption is explicit in §52.
package realtime

import (
	"time"

	"github.com/google/uuid"
)

// MessageType is the `type` field of the envelope, and the frozen contract of §47.
//
// The names are the message vocabulary, not the state vocabulary: SCREEN_LOST is sent
// both when a screen is lost and (as SCREEN_RESTORED) when it comes back, and
// STUDENT_OFFLINE carries the reason as data. The frontend switches on these strings,
// so renaming one is a breaking change for two applications.
type MessageType string

const (
	// TypeRoomOpened tells a classroom's authorized students that a new run started
	// (§48). Data: {classroomId, classroomName, runId, openedAt}.
	TypeRoomOpened MessageType = "ROOM_OPENED"
	// TypeRoomClosed tells the students and the owner that a run ended (§49). Data:
	// {classroomId, runId, closedAt}.
	TypeRoomClosed MessageType = "ROOM_CLOSED"
	// TypeStudentOnline tells the owner teacher that a student is being supervised.
	// Data: {studentId, displayName, sessionId}.
	TypeStudentOnline MessageType = "STUDENT_ONLINE"
	// TypeStudentOffline tells the owner teacher that a student is not, and why. Data:
	// {studentId, sessionId, reason} with reason ∈ DISCONNECTED | LEFT | ROOM_CLOSED.
	TypeStudentOffline MessageType = "STUDENT_OFFLINE"
	// TypeScreenLost is sent to the owner teacher AND to the student themself (§22/§46).
	// Data: {studentId, sessionId} to the teacher, {sessionId} to the student.
	TypeScreenLost MessageType = "SCREEN_LOST"
	// TypeScreenRestored is the counterpart of TypeScreenLost. Same audiences, same
	// data.
	TypeScreenRestored MessageType = "SCREEN_RESTORED"
	// TypeCameraChanged and TypeMicChanged are defined by the contract and are not sent
	// in Phase 8: the camera is Phase 9 (§75) and the microphone Phase 10 (§76). They
	// exist here so the frontend can write its switch statement once. Data:
	// {studentId, sessionId, active}.
	TypeCameraChanged MessageType = "CAMERA_CHANGED"
	TypeMicChanged    MessageType = "MIC_CHANGED"
	// TypePrivateTalkRequest, TypePrivateTalkStarted and TypePrivateTalkEnded belong to
	// Phase 10 (§76) and are addressed to ONE student. Nothing sends them in Phase 8.
	TypePrivateTalkRequest MessageType = "PRIVATE_TALK_REQUEST"
	TypePrivateTalkStarted MessageType = "PRIVATE_TALK_STARTED"
	TypePrivateTalkEnded   MessageType = "PRIVATE_TALK_ENDED"
	// TypePing and TypePong are the application-level heartbeat of the contract.
	//
	// WHY there are two heartbeats: the server also sends WebSocket control PING frames
	// (see Hub), which is what actually detects a dead client — a browser answers those
	// automatically, without any JavaScript, and a client that has been suspended by the
	// OS stops answering. The application-level PING exists because the contract
	// promises a PONG, which gives the frontend a way to measure round-trip time and to
	// probe the pipe synchronously. Both are read-only: neither carries data and neither
	// is interpreted as a command.
	TypePing MessageType = "PING"
	TypePong MessageType = "PONG"
)

// Message is the envelope every server→client message uses (§47, frozen contract):
//
//	{ "type": "SCREEN_LOST", "at": "2026-10-03T19:31:42Z", "data": { ... } }
//
// WHY an envelope and not one struct per message type: the frontend gets a single
// `onmessage` that switches on `type`, and adding a message type does not change the
// shape of anything already deployed. `data` is an object (never null) so a client can
// read `msg.data.x` without a guard.
type Message struct {
	// Type is the message name. Only the constants above are ever sent.
	Type MessageType `json:"type"`
	// At is when the server produced the message, in UTC, RFC3339.
	At time.Time `json:"at"`
	// Data is the message-specific payload. It holds identifiers, statuses and names of
	// the ONE student the message is about — never another student's data (§26).
	Data map[string]any `json:"data"`
}

// newMessage builds an envelope with a non-nil data object and a UTC timestamp.
//
// Every constructor below goes through it, which is what keeps "data is always an
// object" true: a nil map marshals to `null`, and `msg.data.sessionId` would then throw
// in the client for exactly the messages that carry no data.
func newMessage(kind MessageType, data map[string]any) Message {
	if data == nil {
		data = map[string]any{}
	}
	return Message{Type: kind, At: time.Now().UTC(), Data: data}
}

// nowRFC3339 renders the timestamp fields INSIDE data (openedAt, closedAt).
//
// WHY those are strings and not time.Time: they are part of the frozen contract that the
// frontends are being written against in parallel, and an explicit RFC3339 string is
// unambiguous — a nested time.Time would be serialised with nanosecond precision, which
// is still RFC3339 but surprises a client that compares it with a value it produced
// itself. The envelope's own `at` keeps the default encoding, which is the same instant
// with the same offset.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// studentRef is the per-student block two messages share.
//
// It is built from a resolved student (id + display name), so a message can never
// carry a student id nobody authorized: the caller had to find the student in the
// classroom roster first.
type studentRef struct {
	StudentID   uuid.UUID
	DisplayName string
}
