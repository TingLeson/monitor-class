package session

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
)

// Processor is the runtime event path of §74: it turns media-plane observations and
// control-plane lifecycle calls into session state transitions, session_events rows and
// business messages.
//
// # Why this is not part of Service
//
// Service answers requests (join, leave, monitor). Processor consumes events that
// arrive with no request behind them — a signed webhook from LiveKit, the tail of a
// close transaction — and it must be testable without a media plane, a hub or an HTTP
// server. Splitting them keeps the polling state machine and the event state machine
// readable side by side, sharing exactly one thing: the Status vocabulary.
//
// # The rule that governs every method below
//
// The CLIENT is never believed (§45/§46). A browser says "I am online" for a fast UI;
// what writes ONLINE is a SCREEN_SHARE track the media plane reported. And because
// webhooks are delivered at least once and out of order, no method here decides a state
// from what it just read — every write is a conditional update guarded by the state it
// expects (see EventStore.ApplyTransition).
type Processor struct {
	store  EventStore
	events SessionEvents
}

// NewProcessor wires the processor. events may be nil in a deployment without a
// realtime layer: the state machine and the event log then still work, and only the
// WebSocket messages are skipped — which is the honest behaviour for an API running
// with no hub (see internal/realtime for why the hub is process-local in Phase 8).
func NewProcessor(store EventStore, events SessionEvents) *Processor {
	return &Processor{store: store, events: events}
}

// ProcessWebhook applies one verified LiveKit webhook (§45).
//
// A nil error means "handled", including every case where nothing was changed: an
// unknown event, an identity that is not a student session of this room (the teacher's
// own participant is exactly that), a duplicate delivery and an out-of-order one. The
// HTTP layer answers 200 for all of them, because a 4xx would make LiveKit retry an
// event that can never succeed.
//
// An error means the control plane could not record what it observed (the database is
// unreachable, a constraint rejected the write). The HTTP layer answers 500 there and
// LiveKit retries: every transition is idempotent, so a retry is safe, and silently
// dropping an observation would leave a row the teacher's wall trusts in a state the
// media plane already left.
func (p *Processor) ProcessWebhook(ctx context.Context, ev *livekit.WebhookEvent) error {
	if p == nil || p.store == nil || ev == nil {
		return nil
	}
	room := ev.GetRoom().GetName()
	identity := ev.GetParticipant().GetIdentity()

	switch ev.GetEvent() {
	case webhook.EventRoomStarted:
		// §33: a LiveKit room existing means some media infrastructure allocated a
		// name. It does NOT mean a lesson is open — that is a classroom_runs row, and
		// the teacher's open call wrote it. Logged and dropped.
		logging.FromContext(ctx).Debug("livekit room started",
			"action", "livekit_room_started",
			"room", room,
			"webhook_event_id", ev.GetId(),
			"note", "a live media room is not a classroom state",
		)
		return nil

	case webhook.EventRoomFinished:
		return p.roomFinished(ctx, ev)

	case webhook.EventParticipantJoined:
		return p.participantJoined(ctx, ev, room, identity)

	case webhook.EventParticipantLeft, webhook.EventParticipantConnectionAborted:
		return p.participantGone(ctx, ev, room, identity)

	case webhook.EventTrackPublished:
		return p.trackPublished(ctx, ev, room, identity)

	case webhook.EventTrackUnpublished:
		return p.trackUnpublished(ctx, ev, room, identity)

	default:
		// Egress, ingress and every future event: this project records none of them
		// (§53: V1 does not record). Debug, not Warn: an event we do not consume is not
		// an incident, and a Warn here would fire on every LiveKit upgrade that adds a
		// type.
		logging.FromContext(ctx).Debug("livekit webhook ignored",
			"action", "livekit_webhook_ignored",
			"room", room,
			"livekit_event", ev.GetEvent(),
			"webhook_event_id", ev.GetId(),
		)
		return nil
	}
}

// participantJoined handles participant_joined (§45).
//
// It must NOT set ONLINE: §21 makes ONLINE mean "a screen-share track exists", and the
// participant event says nothing about tracks. What it does record is that the person
// arrived, which is the difference between a browser that never made it and a
// connection that dropped.
func (p *Processor) participantJoined(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	stored, err := p.lookup(ctx, room, identity, ev)
	if err != nil || stored == nil {
		return err
	}
	if stored.Status.Terminal() {
		// A late event for a session that already left or was closed by the teacher.
		// Terminal states are never re-entered (§74).
		logDebugSkip(ctx, ev, stored, "session is terminal")
		return nil
	}

	payload := p.payload(ev, room)
	payload["participantSid"] = ev.GetParticipant().GetSid()

	// Case 1: the participant is BACK. DISCONNECTED means "not in the room", and this
	// event says they are — leaving the row claiming a disconnect would make the wall
	// show "连接已断开" for somebody who is sitting in the room. CONNECTING (not ONLINE)
	// because no screen track has been observed yet.
	reconnected, err := p.store.ApplyTransition(ctx, Transition{
		SessionID:     stored.ID,
		From:          []Status{StatusDisconnected},
		To:            StatusConnecting,
		MarkConnected: true,
		Event:         EventConnectionRestored,
		Payload:       payload,
	})
	if err != nil {
		return err
	}
	if reconnected.Session != nil {
		logging.FromContext(ctx).Info("student media connection restored",
			"action", "student_connection_restored",
			"room", room,
			"session_id", stored.ID.String(),
			"student_id", stored.StudentID.String(),
			"run_id", stored.ClassroomRunID.String(),
			"from", string(StatusDisconnected),
			"to", string(StatusConnecting),
		)
		return nil
	}

	// Case 2: the FIRST observation of this participant. The status does not move, so
	// the guard is the timestamp: `connected_at IS NULL` is what tells a first delivery
	// from a duplicate one.
	connected, err := p.store.ApplyTransition(ctx, Transition{
		SessionID:            stored.ID,
		From:                 []Status{StatusConnecting},
		To:                   StatusConnecting,
		MarkConnected:        true,
		OnlyIfNeverConnected: true,
		Event:                EventParticipantConnected,
		Payload:              payload,
	})
	if err != nil {
		return err
	}
	if connected.Session != nil {
		logging.FromContext(ctx).Info("student participant observed in the media room",
			"action", "participant_connected",
			"room", room,
			"session_id", stored.ID.String(),
			"student_id", stored.StudentID.String(),
			"run_id", stored.ClassroomRunID.String(),
			"note", "no state change: ONLINE requires an observed SCREEN_SHARE track (§45)",
		)
		return nil
	}

	// Neither: the participant is already connected (or is sharing and went straight to
	// ONLINE). A duplicate delivery of an event that was already applied.
	logDebugSkip(ctx, ev, stored, "participant_joined already recorded")
	return nil
}

// participantGone handles participant_left and participant_connection_aborted.
//
// Both mean the same business thing — the participant is no longer in the room — and
// both are guarded to the ACTIVE states that can observe a participant, so a duplicate
// (or a `left` that arrives after the student pressed leave, whose row is already LEFT)
// changes nothing.
func (p *Processor) participantGone(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	stored, err := p.lookup(ctx, room, identity, ev)
	if err != nil || stored == nil {
		return err
	}

	result, err := p.store.ApplyTransition(ctx, Transition{
		SessionID: stored.ID,
		From:      []Status{StatusConnecting, StatusOnline, StatusScreenLost},
		To:        StatusDisconnected,
		Event:     EventConnectionLost,
		Payload:   p.payload(ev, room),
	})
	if err != nil {
		return err
	}
	if result.Session == nil {
		logDebugSkip(ctx, ev, stored, "no active session to disconnect")
		return nil
	}

	logging.FromContext(ctx).Info("student media connection lost",
		"action", "connection_lost",
		"room", room,
		"session_id", stored.ID.String(),
		"student_id", stored.StudentID.String(),
		"run_id", stored.ClassroomRunID.String(),
		"from", string(stored.Status),
		"to", string(StatusDisconnected),
		"livekit_event", ev.GetEvent(),
	)
	return p.notifyOffline(ctx, stored, OfflineDisconnected)
}

// trackPublished handles track_published (§45).
//
// The source decides which half of the runtime path runs, and the split is the whole
// point of §21/§24:
//
//   - SCREEN_SHARE is the ONLY observation that can turn a session ONLINE (see
//     screenPublished).
//   - CAMERA is the student's optional second track (§75). It is recorded and announced
//     to the owner teacher, and it must not touch the session status at all — a student
//     who turns their camera on while their screen is lost stays SCREEN_LOST, because
//     §21 makes the SCREEN the mandatory track and a camera is not a substitute.
//   - anything else (today: the microphone of Phase 10) is logged and dropped.
func (p *Processor) trackPublished(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	switch ev.GetTrack().GetSource() {
	case livekit.TrackSource_SCREEN_SHARE:
		return p.screenPublished(ctx, ev, room, identity)
	case livekit.TrackSource_CAMERA:
		return p.cameraObserved(ctx, ev, room, identity, true)
	default:
		// Deliberately Quiet: Phase 9 grants a student token the camera as well as the
		// screen, so the only source that still lands here is the microphone of Phase 10
		// (§76) — a non-event today, not an incident.
		logging.FromContext(ctx).Debug("track source without a session rule; session state unchanged",
			"action", "track_published_ignored",
			"room", room,
			"track_source", ev.GetTrack().GetSource().String(),
			"track_sid", ev.GetTrack().GetSid(),
		)
		return nil
	}
}

// screenPublished is the §45/§21 rule: an observed SCREEN_SHARE track is what makes a
// session ONLINE, and nothing else is.
func (p *Processor) screenPublished(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	source := ev.GetTrack().GetSource()
	stored, err := p.lookup(ctx, room, identity, ev)
	if err != nil || stored == nil {
		return err
	}

	payload := p.payload(ev, room)
	payload["trackSid"] = ev.GetTrack().GetSid()
	payload["trackSource"] = source.String()
	payload["participantSid"] = ev.GetParticipant().GetSid()

	// Case 1 (restoration): SCREEN_LOST → ONLINE. §22 gives this its own event, because
	// "the screen came back" is the answer to the teacher's question "did they fix it?".
	restored, err := p.store.ApplyTransition(ctx, Transition{
		SessionID:         stored.ID,
		From:              []Status{StatusScreenLost},
		To:                StatusOnline,
		MarkConnected:     true,
		MarkScreenStarted: true,
		Event:             EventScreenRestored,
		Payload:           payload,
	})
	if err != nil {
		return err
	}
	if restored.Session != nil {
		logging.FromContext(ctx).Info("student screen restored",
			"action", "screen_restored",
			"room", room,
			"session_id", stored.ID.String(),
			"student_id", stored.StudentID.String(),
			"run_id", stored.ClassroomRunID.String(),
			"track_sid", ev.GetTrack().GetSid(),
		)
		return p.notifyScreen(ctx, stored, true)
	}

	// Case 2: the first time a screen is observed for this session (CONNECTING, or a
	// reconnect that has not published yet).
	published, err := p.store.ApplyTransition(ctx, Transition{
		SessionID:         stored.ID,
		From:              []Status{StatusConnecting, StatusDisconnected},
		To:                StatusOnline,
		MarkConnected:     true,
		MarkScreenStarted: true,
		Event:             EventScreenPublished,
		Payload:           payload,
	})
	if err != nil {
		return err
	}
	if published.Session != nil {
		logging.FromContext(ctx).Info("student session online",
			"action", "session_online",
			"room", room,
			"session_id", stored.ID.String(),
			"student_id", stored.StudentID.String(),
			"run_id", stored.ClassroomRunID.String(),
			"from", string(stored.Status),
			"to", string(StatusOnline),
			"track_sid", ev.GetTrack().GetSid(),
		)
		return p.notifyOnline(ctx, stored)
	}

	// Already ONLINE (a re-publish of the screen, or a duplicate delivery), or terminal.
	logDebugSkip(ctx, ev, stored, "screen publication already recorded")
	return nil
}

// trackUnpublished handles track_unpublished (§22/§24/§45).
//
// Only a SCREEN_SHARE track can move ONLINE → SCREEN_LOST. That guard is what makes a
// webhook that arrives BEFORE its own track_published harmless: the session is still
// CONNECTING, so there is nothing to lose, and the late publication then moves it to
// ONLINE — the true end state. Without the guard, the out-of-order pair would leave the
// session in SCREEN_LOST forever.
//
// A CAMERA track is handled on its own path for the same reason: the camera going away is
// not the screen going away, and §21's invariant is about the screen.
func (p *Processor) trackUnpublished(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	switch ev.GetTrack().GetSource() {
	case livekit.TrackSource_SCREEN_SHARE:
		return p.screenUnpublished(ctx, ev, room, identity)
	case livekit.TrackSource_CAMERA:
		return p.cameraObserved(ctx, ev, room, identity, false)
	default:
		logging.FromContext(ctx).Debug("track source without a session rule; session state unchanged",
			"action", "track_unpublished_ignored",
			"room", room,
			"track_source", ev.GetTrack().GetSource().String(),
			"track_sid", ev.GetTrack().GetSid(),
		)
		return nil
	}
}

// screenUnpublished is the §22 rule: the screen went away while the student is still in
// the room.
func (p *Processor) screenUnpublished(ctx context.Context, ev *livekit.WebhookEvent, room, identity string) error {
	source := ev.GetTrack().GetSource()
	stored, err := p.lookup(ctx, room, identity, ev)
	if err != nil || stored == nil {
		return err
	}

	payload := p.payload(ev, room)
	payload["trackSid"] = ev.GetTrack().GetSid()
	payload["trackSource"] = source.String()

	lost, err := p.store.ApplyTransition(ctx, Transition{
		SessionID:      stored.ID,
		From:           []Status{StatusOnline},
		To:             StatusScreenLost,
		MarkScreenLost: true,
		Event:          EventScreenLost,
		Payload:        payload,
	})
	if err != nil {
		return err
	}
	if lost.Session == nil {
		logDebugSkip(ctx, ev, stored, "session was not sharing a screen")
		return nil
	}

	logging.FromContext(ctx).Info("student screen lost",
		"action", "screen_lost",
		"room", room,
		"session_id", stored.ID.String(),
		"student_id", stored.StudentID.String(),
		"run_id", stored.ClassroomRunID.String(),
		"track_sid", ev.GetTrack().GetSid(),
	)
	return p.notifyScreen(ctx, stored, false)
}

// cameraObserved handles track_published/track_unpublished for a CAMERA track (§24/§75).
//
// # What the camera may and may not do
//
// It records CAMERA_STARTED / CAMERA_STOPPED and tells the OWNER teacher. It does not
// touch `student_sessions.status`, and that is the hard rule of this phase: §24 says a
// camera does not affect ONLINE, §21 says only a screen track is mandatory, and §45 says
// only SCREEN_SHARE produces ONLINE. A student who turns their camera on while their
// screen share is lost must stay SCREEN_LOST — the teacher's wall answers "is this
// student being supervised?", and a camera is not the supervision.
//
// # Why the state is read from the event log
//
// The camera has no column, so "is it already on?" is answered by the newest CAMERA_*
// event of this session (see TrackStateChange). The processor does not read it: the STORE
// makes the decision inside the transaction that appends the row, and reports back whether
// history changed. Broadcasting on that answer is what makes at-least-once delivery
// produce exactly one message per real change — a duplicate delivery, a retry of an
// already-stopped publication and an out-of-order stop all write nothing, so nothing is
// sent (see ApplyTrackState for the two halves of the guard).
func (p *Processor) cameraObserved(ctx context.Context, ev *livekit.WebhookEvent, room, identity string, active bool) error {
	stored, err := p.lookup(ctx, room, identity, ev)
	if err != nil || stored == nil {
		return err
	}
	if stored.Status.Terminal() {
		// A late event for a session that already left or was closed (§74). Terminal
		// states are never re-entered, so the camera cannot be started in one either.
		logDebugSkip(ctx, ev, stored, "session is terminal")
		return nil
	}

	payload := p.payload(ev, room)
	payload["trackSid"] = ev.GetTrack().GetSid()
	payload["trackSource"] = livekit.TrackSource_CAMERA.String()
	payload["participantSid"] = ev.GetParticipant().GetSid()

	applied, err := p.store.ApplyTrackState(ctx, TrackStateChange{
		SessionID: stored.ID,
		On:        EventCameraStarted,
		Off:       EventCameraStopped,
		Active:    active,
		TrackSid:  ev.GetTrack().GetSid(),
		Payload:   payload,
	})
	if err != nil {
		return err
	}
	if !applied {
		logDebugSkip(ctx, ev, stored, "camera state already recorded")
		return nil
	}

	logging.FromContext(ctx).Info("student camera changed",
		"action", "camera_changed",
		"room", room,
		"session_id", stored.ID.String(),
		"student_id", stored.StudentID.String(),
		"run_id", stored.ClassroomRunID.String(),
		"track_sid", ev.GetTrack().GetSid(),
		"active", active,
		"session_status", string(stored.Status),
		"note", "a camera never changes the session status (§24/§21)",
	)
	return p.notifyCamera(ctx, stored, active)
}

// roomFinished handles room_finished: every active session of that run ends (§45/§49).
//
// It is deliberately the same code path as the teacher's close, minus the classroom
// state change: which of the two arrives first is a race (the close transaction
// commits, then TerminateRoom makes LiveKit emit this event), and both must leave the
// same rows behind. The second one matches nothing, because the sessions are already
// terminal.
func (p *Processor) roomFinished(ctx context.Context, ev *livekit.WebhookEvent) error {
	room := ev.GetRoom().GetName()
	ref, ours, err := p.store.RunByRoomName(ctx, room)
	if err != nil {
		return err
	}
	if !ours {
		// Another deployment's room, a room from a previous installation, or a test
		// room. Not an error: this endpoint is public and must survive anything.
		logging.FromContext(ctx).Info("room_finished for a room that is not ours",
			"action", "room_finished_ignored",
			"room", room,
			"webhook_event_id", ev.GetId(),
		)
		return nil
	}

	closed, err := p.closeRun(ctx, ref, "ROOM_FINISHED", room)
	if err != nil {
		return err
	}
	if len(closed) == 0 {
		logging.FromContext(ctx).Info("room_finished with no active sessions",
			"action", "room_finished_no_sessions",
			"room", room,
			"run_id", ref.RunID.String(),
			"classroom_id", ref.ClassroomID.String(),
		)
		return nil
	}
	// The lesson's media plane is gone, so the clients that are still on the page must
	// stop sharing. The classroom row may still be OPEN (LiveKit's empty timeout can
	// finish a room nobody closed); that is a control-plane fact the teacher's next
	// action resolves, and telling the browsers "this run is over" is still true.
	return p.broadcastRoomClosed(ctx, ref, room)
}

// CloseRunSessions implements the classroom lifecycle's port (§49): after the close
// transaction commits, every active session of that run becomes ROOM_CLOSED and each
// one produces an event and an offline message.
//
// WHY it lives here and not in the classroom transaction: the classroom transaction
// must stay short and must not depend on a broadcast, and the session states belong to
// this package. The order is the one §49 requires — the database first, the clients
// after — and a failure of this step is reported to the caller, which logs it and still
// answers the teacher (the classroom IS closed).
func (p *Processor) CloseRunSessions(ctx context.Context, runID uuid.UUID) (int, error) {
	if p == nil || p.store == nil {
		return 0, nil
	}
	ref, err := p.runRef(ctx, runID)
	if err != nil {
		return 0, err
	}
	if ref.RunID == uuid.Nil {
		// Unknown run: nothing of ours to close. Not an error — the caller has already
		// committed its own state change.
		return 0, nil
	}
	closed, err := p.closeRun(ctx, ref, "CLASSROOM_CLOSED", "")
	if err != nil {
		return 0, err
	}
	return len(closed), nil
}

// RoomOpened implements the classroom lifecycle's port (§48).
func (p *Processor) RoomOpened(ctx context.Context, classroomID, runID uuid.UUID) error {
	if p == nil || p.events == nil {
		return nil
	}
	return p.events.RoomOpened(ctx, classroomID, runID)
}

// RoomClosed implements the classroom lifecycle's port (§49).
func (p *Processor) RoomClosed(ctx context.Context, classroomID, runID uuid.UUID) error {
	if p == nil || p.events == nil {
		return nil
	}
	return p.events.RoomClosed(ctx, classroomID, runID)
}

// SessionCreated implements LifecycleEvents (§13): the join endpoint records that a
// student entered a lesson. It is recorded once per session row, so a double-clicked
// join or a page reload that reuses the row does not grow the log.
func (p *Processor) SessionCreated(ctx context.Context, stored *StudentSession) error {
	if p == nil || p.store == nil || stored == nil {
		return nil
	}
	recorded, err := p.store.RecordEventOnce(ctx, stored.ID, EventSessionCreated, map[string]any{
		"status": string(stored.Status),
		"runId":  stored.ClassroomRunID.String(),
	})
	if err != nil {
		return err
	}
	if recorded {
		logging.FromContext(ctx).Debug("session created event recorded",
			"action", "session_event_recorded",
			logging.FieldSessionID, stored.ID.String(),
			"event_type", string(EventSessionCreated),
		)
	}
	return nil
}

// SessionLeft implements LifecycleEvents (§13/§47): the leave endpoint records
// STUDENT_LEFT and tells the teacher the student is offline.
//
// The event row is the idempotency key: it is inserted only if this session has no
// STUDENT_LEFT yet, so a retried leave (a double click, a page that retried after a
// timeout) produces neither a second row nor a second message. LEFT is terminal and a
// re-entry creates a new session row, so the guard can never suppress a real second
// leave.
func (p *Processor) SessionLeft(ctx context.Context, stored *StudentSession) error {
	if p == nil || p.store == nil || stored == nil {
		return nil
	}
	payload := map[string]any{
		"reason": string(OfflineLeft),
		"runId":  stored.ClassroomRunID.String(),
	}
	if stored.LeftAt != nil {
		payload["leftAt"] = stored.LeftAt.UTC().Format(time.RFC3339)
	}
	recorded, err := p.store.RecordEventOnce(ctx, stored.ID, EventStudentLeft, payload)
	if err != nil {
		return err
	}
	if !recorded {
		return nil
	}
	logging.FromContext(ctx).Info("student session event recorded",
		"action", "session_event_recorded",
		logging.FieldSessionID, stored.ID.String(),
		"event_type", string(EventStudentLeft),
	)
	return p.notifyOffline(ctx, stored, OfflineLeft)
}

// closeRun is the shared body of "this run is over for the students in it": mark the
// active sessions ROOM_CLOSED (one event row each) and tell the teacher about each one.
//
// ROOM_CLOSED is broadcast by the caller, because only the caller knows whether the
// lesson-level message is due (the teacher's close sends it; a room_finished with no
// active sessions sends nothing).
func (p *Processor) closeRun(ctx context.Context, ref ClassroomRef, source, room string) ([]StudentSession, error) {
	payload := map[string]any{
		"reason": string(OfflineRoomClosed),
		"source": source,
	}
	if room != "" {
		payload["room"] = room
	}
	closed, err := p.store.CloseRunSessions(ctx, ref.RunID, EventRoomClosed, payload)
	if err != nil {
		return nil, err
	}
	for i := range closed {
		stored := closed[i]
		logging.FromContext(ctx).Info("student session closed with the run",
			"action", "session_room_closed",
			logging.FieldSessionID, stored.ID.String(),
			logging.FieldRunID, stored.ClassroomRunID.String(),
			"student_id", stored.StudentID.String(),
			"source", source,
		)
		if err := p.notifyOffline(ctx, &stored, OfflineRoomClosed); err != nil {
			return closed, err
		}
	}
	return closed, nil
}

// broadcastRoomClosed announces the end of a run to its students and its owner.
func (p *Processor) broadcastRoomClosed(ctx context.Context, ref ClassroomRef, room string) error {
	if p.events == nil {
		return nil
	}
	return p.events.RoomClosed(ctx, ref.ClassroomID, ref.RunID)
}

// lookup resolves a webhook identity to a session of that room.
//
// A nil session is not an error and not a warning: it is the answer for the teacher's
// own participant (a login session id, which is not in student_sessions by design),
// for a room this deployment does not own, and for an identity somebody made up. All
// three are logged at Info with the identity omitted — it is either a UUID we cannot
// map to a person or, worse, a string we did not choose, and §59 forbids putting
// unvetted client-supplied text into the logs.
func (p *Processor) lookup(ctx context.Context, room, identity string, ev *livekit.WebhookEvent) (*StudentSession, error) {
	if identity == "" {
		logging.FromContext(ctx).Info("livekit webhook without a participant identity",
			"action", "livekit_webhook_ignored",
			"room", room,
			"livekit_event", ev.GetEvent(),
			"webhook_event_id", ev.GetId(),
		)
		return nil, nil
	}
	stored, err := p.store.SessionInRoomByIdentity(ctx, room, identity)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		logging.FromContext(ctx).Info("livekit webhook identity is not a student session of this room",
			"action", "livekit_webhook_unmatched_identity",
			"room", room,
			"livekit_event", ev.GetEvent(),
			"webhook_event_id", ev.GetId(),
			"note", "a teacher participant, a foreign room or an unknown identity; no session state is written",
		)
		return nil, nil
	}
	return stored, nil
}

// payload builds the diagnostic block every event row of one webhook shares.
//
// It holds identifiers only: the opaque room name, LiveKit's event uuid, the event
// name, the participant sid and (by the callers) the track sid/source. No name, no
// account, no token, no media (§13/§53/§59).
func (p *Processor) payload(ev *livekit.WebhookEvent, room string) map[string]any {
	payload := map[string]any{
		"room":           room,
		"livekitEvent":   ev.GetEvent(),
		"webhookEventId": ev.GetId(),
	}
	if ev.GetCreatedAt() > 0 {
		payload["webhookCreatedAt"] = time.Unix(ev.GetCreatedAt(), 0).UTC().Format(time.RFC3339)
	}
	return payload
}

// notifyOnline tells the owner teacher that a student is now being supervised.
func (p *Processor) notifyOnline(ctx context.Context, stored *StudentSession) error {
	if p.events == nil {
		return nil
	}
	return p.events.StudentOnline(ctx, stored.Ref())
}

// notifyOffline tells the owner teacher that a student no longer is, and why.
func (p *Processor) notifyOffline(ctx context.Context, stored *StudentSession, reason OfflineReason) error {
	if p.events == nil {
		return nil
	}
	return p.events.StudentOffline(ctx, stored.Ref(), reason)
}

// notifyScreen tells the owner AND the student themself that the screen went away or
// came back (§47). It is the one message that reaches a student's browser about their
// own session — and it is addressed to that student's id alone (§26).
func (p *Processor) notifyScreen(ctx context.Context, stored *StudentSession, restored bool) error {
	if p.events == nil {
		return nil
	}
	if restored {
		return p.events.ScreenRestored(ctx, stored.Ref())
	}
	return p.events.ScreenLost(ctx, stored.Ref())
}

// notifyCamera tells the OWNER teacher that a student's camera went on or off (§47/§75).
//
// WHY only the owner, and why not the student: §26 forbids one student from learning
// anything about a classmate, and "classmate turned their camera on" is exactly such a
// fact — a classroom-wide CAMERA_CHANGED would tell every student who is being watched by
// whom. The student themself does not need it either: they pressed the button and their
// own page renders the local track, so a server copy could only arrive late and disagree
// with what is already on screen. The teacher's wall, which cannot see anybody's button, is
// the one audience that needs the fact.
func (p *Processor) notifyCamera(ctx context.Context, stored *StudentSession, active bool) error {
	if p.events == nil {
		return nil
	}
	return p.events.CameraChanged(ctx, stored.Ref(), active)
}

// runRef resolves the run and classroom a close is about.
//
// A run that cannot be found is not an error: it means the close path was called with
// an id this database does not know, and the caller has nothing to close. The empty
// ref then closes no sessions, which is the correct answer.
func (p *Processor) runRef(ctx context.Context, runID uuid.UUID) (ClassroomRef, error) {
	ref, ok, err := p.store.ClassroomRefByRunID(ctx, runID)
	if err != nil {
		return ClassroomRef{}, err
	}
	if !ok {
		return ClassroomRef{}, nil
	}
	return ref, nil
}

// logDebugSkip records a webhook that was correctly ignored.
//
// Debug, and one line, because this is the NORMAL outcome of at-least-once delivery:
// LiveKit retries, and the retry of an applied event changes nothing. Logging it at
// Warn would teach operators to ignore the level.
func logDebugSkip(ctx context.Context, ev *livekit.WebhookEvent, stored *StudentSession, reason string) {
	logging.FromContext(ctx).Debug("livekit webhook made no change",
		"action", "livekit_webhook_no_change",
		"livekit_event", ev.GetEvent(),
		"webhook_event_id", ev.GetId(),
		"session_id", stored.ID.String(),
		"session_status", string(stored.Status),
		"reason", reason,
	)
}

// Compile-time assertions.
//
// The first is the contract with the classroom lifecycle (§48/§49): the classroom service
// holds a RuntimeHooks and this type is what main hands it, so a signature change has to
// be a deliberate one. The second says the processor is the LifecycleEvents the session
// service reports into, and the third that it is the media-event processor the webhook
// endpoint calls — both are structural interfaces declared by their consumers.
var (
	_ classroom.RuntimeHooks = (*Processor)(nil)
	_ LifecycleEvents        = (*Processor)(nil)
)
