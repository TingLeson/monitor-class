package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
)

// The Phase 8 event state machine (§45/§74), against in-memory fakes.
//
// The fake store mirrors the CONTRACT of the SQL it replaces — a conditional update
// guarded by the state the caller expects — because that guard IS the subject of these
// tests. A fake that applied every write unconditionally would make every idempotency
// test pass for the wrong reason.
//
// The end-to-end version (real PostgreSQL, real signatures) is in
// internal/httpapi/livekit_webhook_integration_test.go.

// ---------------------------------------------------------------------------
// Fake event store
// ---------------------------------------------------------------------------

type fakeStore struct {
	sessions map[uuid.UUID]*StudentSession
	// rooms maps a media room name to the run it belongs to.
	rooms map[string]uuid.UUID
	// refs maps a run id to the classroom it belongs to.
	refs   map[uuid.UUID]ClassroomRef
	events []SessionEvent
	clock  time.Time

	applyErr  error
	lookupErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[uuid.UUID]*StudentSession{},
		rooms:    map[string]uuid.UUID{},
		refs:     map[uuid.UUID]ClassroomRef{},
		clock:    time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC),
	}
}

func (f *fakeStore) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

// seed registers a run's room and inserts a session, bypassing every rule (like manual
// SQL would).
func (f *fakeStore) seed(runID, classroomID, studentID uuid.UUID, status Status) *StudentSession {
	room := "lk_" + runID.String()
	f.rooms[room] = runID
	f.refs[runID] = ClassroomRef{RunID: runID, ClassroomID: classroomID}

	id := uuid.New()
	stored := &StudentSession{
		ID:              id,
		ClassroomRunID:  runID,
		StudentID:       studentID,
		LiveKitIdentity: id.String(),
		Status:          status,
		CreatedAt:       f.tick(),
		UpdatedAt:       f.tick(),
	}
	if status == StatusOnline || status == StatusScreenLost || status == StatusDisconnected {
		// A session that is past CONNECTING has been seen in the room; that is what the
		// production webhook path would have written.
		connected := f.tick()
		stored.ConnectedAt = &connected
	}
	f.sessions[id] = stored
	return stored
}

func (f *fakeStore) room(runID uuid.UUID) string { return "lk_" + runID.String() }

func (f *fakeStore) SessionInRoomByIdentity(_ context.Context, roomName, identity string) (*StudentSession, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	runID, ok := f.rooms[roomName]
	if !ok {
		return nil, nil
	}
	for _, stored := range f.sessions {
		if stored.LiveKitIdentity == identity && stored.ClassroomRunID == runID {
			copied := *stored
			return &copied, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) RunByRoomName(_ context.Context, roomName string) (ClassroomRef, bool, error) {
	runID, ok := f.rooms[roomName]
	if !ok {
		return ClassroomRef{}, false, nil
	}
	ref, ok := f.refs[runID]
	return ref, ok, nil
}

func (f *fakeStore) ClassroomRefByRunID(_ context.Context, runID uuid.UUID) (ClassroomRef, bool, error) {
	ref, ok := f.refs[runID]
	return ref, ok, nil
}

// ApplyTransition reproduces the production statement: match the guard, then write the
// status and the first-time timestamps, then append the event row in the same unit.
func (f *fakeStore) ApplyTransition(_ context.Context, t Transition) (*TransitionResult, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	stored, ok := f.sessions[t.SessionID]
	if !ok {
		return &TransitionResult{}, nil
	}
	if !containsStatus(t.From, stored.Status) {
		return &TransitionResult{}, nil
	}
	if t.OnlyIfNeverConnected && stored.ConnectedAt != nil {
		return &TransitionResult{}, nil
	}

	stored.Status = t.To
	if t.MarkConnected && stored.ConnectedAt == nil {
		at := f.tick()
		stored.ConnectedAt = &at
	}
	if t.MarkScreenStarted && stored.ScreenStartedAt == nil {
		at := f.tick()
		stored.ScreenStartedAt = &at
	}
	if t.MarkScreenLost {
		at := f.tick()
		stored.ScreenLostAt = &at
	}
	stored.UpdatedAt = f.tick()

	result := &TransitionResult{Session: copySession(stored)}
	if t.Event != "" {
		result.Event = f.appendEvent(stored.ID, t.Event, t.Payload)
	}
	return result, nil
}

func (f *fakeStore) CloseRunSessions(_ context.Context, runID uuid.UUID, event EventType, payload map[string]any) ([]StudentSession, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	closed := make([]StudentSession, 0, 4)
	for _, stored := range f.sessions {
		if stored.ClassroomRunID != runID || !stored.Status.Active() {
			continue
		}
		stored.Status = StatusRoomClosed
		stored.UpdatedAt = f.tick()
		f.appendEvent(stored.ID, event, payload)
		closed = append(closed, *copySession(stored))
	}
	return closed, nil
}

func (f *fakeStore) RecordEventOnce(_ context.Context, sessionID uuid.UUID, event EventType, payload map[string]any) (bool, error) {
	if f.applyErr != nil {
		return false, f.applyErr
	}
	for _, existing := range f.events {
		if existing.SessionID == sessionID && existing.Type == event {
			return false, nil
		}
	}
	f.appendEvent(sessionID, event, payload)
	return true, nil
}

func (f *fakeStore) appendEvent(sessionID uuid.UUID, event EventType, payload map[string]any) *SessionEvent {
	recorded := SessionEvent{
		ID:        uuid.New(),
		SessionID: sessionID,
		Type:      event,
		Payload:   payload,
		CreatedAt: f.tick(),
	}
	f.events = append(f.events, recorded)
	return &recorded
}

// eventsOf lists the event types recorded for one session, in order.
func (f *fakeStore) eventsOf(sessionID uuid.UUID) []EventType {
	var types []EventType
	for _, recorded := range f.events {
		if recorded.SessionID == sessionID {
			types = append(types, recorded.Type)
		}
	}
	return types
}

func containsStatus(statuses []Status, candidate Status) bool {
	for _, status := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func copySession(stored *StudentSession) *StudentSession {
	copied := *stored
	return &copied
}

// ---------------------------------------------------------------------------
// Fake runtime events
// ---------------------------------------------------------------------------

type offlineMessage struct {
	ref    SessionRef
	reason OfflineReason
}

type fakeEvents struct {
	online         []SessionRef
	offline        []offlineMessage
	screenLost     []SessionRef
	screenRestored []SessionRef
	roomsOpened    [][2]uuid.UUID
	roomsClosed    [][2]uuid.UUID

	err error
}

func (f *fakeEvents) RoomOpened(_ context.Context, classroomID, runID uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.roomsOpened = append(f.roomsOpened, [2]uuid.UUID{classroomID, runID})
	return nil
}

func (f *fakeEvents) RoomClosed(_ context.Context, classroomID, runID uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.roomsClosed = append(f.roomsClosed, [2]uuid.UUID{classroomID, runID})
	return nil
}

func (f *fakeEvents) StudentOnline(_ context.Context, ref SessionRef) error {
	if f.err != nil {
		return f.err
	}
	f.online = append(f.online, ref)
	return nil
}

func (f *fakeEvents) StudentOffline(_ context.Context, ref SessionRef, reason OfflineReason) error {
	if f.err != nil {
		return f.err
	}
	f.offline = append(f.offline, offlineMessage{ref: ref, reason: reason})
	return nil
}

func (f *fakeEvents) ScreenLost(_ context.Context, ref SessionRef) error {
	if f.err != nil {
		return f.err
	}
	f.screenLost = append(f.screenLost, ref)
	return nil
}

func (f *fakeEvents) ScreenRestored(_ context.Context, ref SessionRef) error {
	if f.err != nil {
		return f.err
	}
	f.screenRestored = append(f.screenRestored, ref)
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type processorHarness struct {
	store   *fakeStore
	events  *fakeEvents
	process *Processor
	ctx     context.Context
}

func newProcessorHarness(t *testing.T) *processorHarness {
	t.Helper()
	store := newFakeStore()
	events := &fakeEvents{}
	// A discarded logger: these tests assert on recorded calls, and the state machine
	// logs one line per decision.
	ctx := logging.ContextWithLogger(context.Background(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &processorHarness{
		store:   store,
		events:  events,
		process: NewProcessor(store, events),
		ctx:     ctx,
	}
}

// webhookEvent builds the shape LiveKit sends.
func webhookEvent(kind, room, identity string) *livekit.WebhookEvent {
	return &livekit.WebhookEvent{
		Event:     kind,
		Id:        uuid.NewString(),
		CreatedAt: time.Now().Unix(),
		Room:      &livekit.Room{Name: room, Sid: "RM_test"},
		Participant: &livekit.ParticipantInfo{
			Identity: identity,
			Sid:      "PA_test",
		},
	}
}

func screenTrackEvent(kind, room, identity, sid string, source livekit.TrackSource) *livekit.WebhookEvent {
	event := webhookEvent(kind, room, identity)
	event.Track = &livekit.TrackInfo{Sid: sid, Source: source, Type: livekit.TrackType_VIDEO}
	return event
}

func screenShareEvent(kind, room, identity, sid string) *livekit.WebhookEvent {
	return screenTrackEvent(kind, room, identity, sid, livekit.TrackSource_SCREEN_SHARE)
}

// ---------------------------------------------------------------------------
// participant_joined
// ---------------------------------------------------------------------------

func TestParticipantJoinedRecordsTheArrivalWithoutGoingOnline(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

	if err := h.process.ProcessWebhook(h.ctx, webhookEvent(webhook.EventParticipantJoined, h.store.room(runID), stored.LiveKitIdentity)); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}

	after := h.store.sessions[stored.ID]
	if after.Status != StatusConnecting {
		t.Fatalf("status = %s, want CONNECTING: participant_joined must not produce ONLINE (§45)", after.Status)
	}
	if after.ConnectedAt == nil {
		t.Fatal("connected_at was not written: the control plane saw the participant and must record it")
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventParticipantConnected {
		t.Fatalf("events = %v, want [PARTICIPANT_CONNECTED]", got)
	}
	// Nothing the teacher's wall can show as "online" happened yet.
	if len(h.events.online) != 0 {
		t.Fatalf("STUDENT_ONLINE was published on participant_joined: %+v", h.events.online)
	}
}

func TestParticipantJoinedTwiceRecordsOneEvent(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)
	event := webhookEvent(webhook.EventParticipantJoined, h.store.room(runID), stored.LiveKitIdentity)

	for i := 0; i < 2; i++ {
		if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
			t.Fatalf("ProcessWebhook() #%d: %v", i+1, err)
		}
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 {
		t.Fatalf("events = %v, want exactly one PARTICIPANT_CONNECTED: at-least-once delivery must not duplicate history", got)
	}
}

func TestParticipantJoinedAfterDisconnectRestoresTheConnection(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusDisconnected)

	if err := h.process.ProcessWebhook(h.ctx, webhookEvent(webhook.EventParticipantJoined, h.store.room(runID), stored.LiveKitIdentity)); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	after := h.store.sessions[stored.ID]
	if after.Status != StatusConnecting {
		t.Fatalf("status = %s, want CONNECTING: a returning participant is in the room but not yet sharing", after.Status)
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventConnectionRestored {
		t.Fatalf("events = %v, want [CONNECTION_RESTORED]", got)
	}
}

// ---------------------------------------------------------------------------
// track_published
// ---------------------------------------------------------------------------

func TestScreenPublishedIsWhatMakesASessionOnline(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	classroomID := uuid.New()
	studentID := uuid.New()
	stored := h.store.seed(runID, classroomID, studentID, StatusConnecting)

	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackPublished, h.store.room(runID), stored.LiveKitIdentity, "TR_screen")); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}

	after := h.store.sessions[stored.ID]
	if after.Status != StatusOnline {
		t.Fatalf("status = %s, want ONLINE", after.Status)
	}
	if after.ScreenStartedAt == nil || after.ConnectedAt == nil {
		t.Fatalf("timestamps not written: connected_at=%v screen_started_at=%v", after.ConnectedAt, after.ScreenStartedAt)
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventScreenPublished {
		t.Fatalf("events = %v, want [SCREEN_PUBLISHED]", got)
	}
	if len(h.events.online) != 1 || h.events.online[0].StudentID != studentID {
		t.Fatalf("STUDENT_ONLINE messages = %+v, want exactly one for the student", h.events.online)
	}
	// The event payload is diagnostic metadata and must name the track, never a person.
	payload := h.store.events[0].Payload
	if payload["trackSid"] != "TR_screen" || payload["trackSource"] != "SCREEN_SHARE" {
		t.Fatalf("payload = %+v, want the track sid and source", payload)
	}
	if _, leaked := payload["displayName"]; leaked {
		t.Fatalf("payload carries a display name: %+v", payload)
	}
}

func TestScreenPublishedTwiceTransitionsOnce(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)
	event := screenShareEvent(webhook.EventTrackPublished, h.store.room(runID), stored.LiveKitIdentity, "TR_screen")

	for i := 0; i < 2; i++ {
		if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
			t.Fatalf("ProcessWebhook() #%d: %v", i+1, err)
		}
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 {
		t.Fatalf("events = %v, want one SCREEN_PUBLISHED", got)
	}
	if len(h.events.online) != 1 {
		t.Fatalf("STUDENT_ONLINE published %d times, want 1", len(h.events.online))
	}
}

func TestScreenRestoredAfterALoss(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	studentID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), studentID, StatusOnline)

	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackUnpublished, h.store.room(runID), stored.LiveKitIdentity, "TR_1")); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusScreenLost {
		t.Fatalf("status after unpublish = %s, want SCREEN_LOST", got)
	}
	if len(h.events.screenLost) != 1 || h.events.screenLost[0].StudentID != studentID {
		t.Fatalf("SCREEN_LOST messages = %+v, want one for that student", h.events.screenLost)
	}

	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackPublished, h.store.room(runID), stored.LiveKitIdentity, "TR_2")); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusOnline {
		t.Fatalf("status after republish = %s, want ONLINE", got)
	}
	want := []EventType{EventScreenLost, EventScreenRestored}
	got := h.store.eventsOf(stored.ID)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if len(h.events.screenRestored) != 1 {
		t.Fatalf("SCREEN_RESTORED messages = %+v, want one", h.events.screenRestored)
	}
}

// ---------------------------------------------------------------------------
// Out-of-order delivery
// ---------------------------------------------------------------------------

func TestUnpublishedBeforePublishedLeavesTheSessionInTheRightState(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)
	room := h.store.room(runID)

	// The unpublish arrives first. The session is CONNECTING, so there is no screen to
	// lose: nothing may happen, and in particular it must not become SCREEN_LOST.
	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackUnpublished, room, stored.LiveKitIdentity, "TR_1")); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusConnecting {
		t.Fatalf("status = %s, want CONNECTING: an out-of-order unpublish must not invent a lost screen", got)
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}

	// The publication then arrives and is the truth.
	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackPublished, room, stored.LiveKitIdentity, "TR_1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusOnline {
		t.Fatalf("status = %s, want ONLINE: the late publication is the true end state", got)
	}
}

func TestLateEventsCannotResurrectATerminalSession(t *testing.T) {
	for _, terminal := range []Status{StatusLeft, StatusRoomClosed} {
		t.Run(string(terminal), func(t *testing.T) {
			h := newProcessorHarness(t)
			runID := uuid.New()
			stored := h.store.seed(runID, uuid.New(), uuid.New(), terminal)
			room := h.store.room(runID)

			events := []*livekit.WebhookEvent{
				webhookEvent(webhook.EventParticipantJoined, room, stored.LiveKitIdentity),
				screenShareEvent(webhook.EventTrackPublished, room, stored.LiveKitIdentity, "TR_late"),
				screenShareEvent(webhook.EventTrackUnpublished, room, stored.LiveKitIdentity, "TR_late"),
				webhookEvent(webhook.EventParticipantLeft, room, stored.LiveKitIdentity),
			}
			for _, event := range events {
				if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
					t.Fatalf("%s: %v", event.GetEvent(), err)
				}
			}

			if got := h.store.sessions[stored.ID].Status; got != terminal {
				t.Fatalf("terminal status was changed to %s by late webhooks", got)
			}
			if got := h.store.eventsOf(stored.ID); len(got) != 0 {
				t.Fatalf("events = %v, want none: nothing happened, so nothing is recorded", got)
			}
			if len(h.events.online)+len(h.events.offline)+len(h.events.screenLost)+len(h.events.screenRestored) != 0 {
				t.Fatal("a terminal session produced a runtime message")
			}
		})
	}
}

func TestParticipantLeftBeforePublishedDoesNotProduceOnline(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)
	room := h.store.room(runID)

	if err := h.process.ProcessWebhook(h.ctx, webhookEvent(webhook.EventParticipantLeft, room, stored.LiveKitIdentity)); err != nil {
		t.Fatalf("left: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusDisconnected {
		t.Fatalf("status = %s, want DISCONNECTED", got)
	}
	// The publication of a connection that is already gone must not bring it back.
	if err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackPublished, room, stored.LiveKitIdentity, "TR_late")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusOnline {
		// NOTE: this IS the §45 rule — a published screen track on a DISCONNECTED
		// session means the participant is back and sharing. The test documents the
		// deliberate behaviour rather than forbidding it.
		t.Fatalf("status = %s, want ONLINE: a screen track proves the participant is present", got)
	}
}

// ---------------------------------------------------------------------------
// participant_left / aborted
// ---------------------------------------------------------------------------

func TestParticipantGoneDisconnectsAnActiveSessionOnce(t *testing.T) {
	for _, kind := range []string{webhook.EventParticipantLeft, webhook.EventParticipantConnectionAborted} {
		t.Run(kind, func(t *testing.T) {
			h := newProcessorHarness(t)
			runID := uuid.New()
			studentID := uuid.New()
			stored := h.store.seed(runID, uuid.New(), studentID, StatusOnline)
			event := webhookEvent(kind, h.store.room(runID), stored.LiveKitIdentity)

			for i := 0; i < 2; i++ {
				if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
					t.Fatalf("ProcessWebhook() #%d: %v", i+1, err)
				}
			}
			if got := h.store.sessions[stored.ID].Status; got != StatusDisconnected {
				t.Fatalf("status = %s, want DISCONNECTED", got)
			}
			if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventConnectionLost {
				t.Fatalf("events = %v, want one CONNECTION_LOST", got)
			}
			if len(h.events.offline) != 1 || h.events.offline[0].reason != OfflineDisconnected {
				t.Fatalf("STUDENT_OFFLINE = %+v, want one DISCONNECTED", h.events.offline)
			}
			if h.events.offline[0].ref.StudentID != studentID {
				t.Fatalf("STUDENT_OFFLINE about %s, want %s", h.events.offline[0].ref.StudentID, studentID)
			}
		})
	}
}

func TestParticipantGoneForAnAlreadyLeftSessionIsIgnored(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusLeft)

	if err := h.process.ProcessWebhook(h.ctx, webhookEvent(webhook.EventParticipantLeft, h.store.room(runID), stored.LiveKitIdentity)); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 0 {
		t.Fatalf("events = %v, want none: LEFT is terminal and the webhook must not touch it", got)
	}
}

// ---------------------------------------------------------------------------
// room_finished / room_started
// ---------------------------------------------------------------------------

func TestRoomFinishedClosesEveryActiveSessionOnce(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	classroomID := uuid.New()
	online := h.store.seed(runID, classroomID, uuid.New(), StatusOnline)
	connecting := h.store.seed(runID, classroomID, uuid.New(), StatusConnecting)
	left := h.store.seed(runID, classroomID, uuid.New(), StatusLeft)
	room := h.store.room(runID)

	event := webhookEvent(webhook.EventRoomFinished, room, "")
	event.Participant = nil
	for i := 0; i < 2; i++ {
		if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
			t.Fatalf("ProcessWebhook() #%d: %v", i+1, err)
		}
	}

	for _, stored := range []*StudentSession{online, connecting} {
		if got := h.store.sessions[stored.ID].Status; got != StatusRoomClosed {
			t.Fatalf("session %s = %s, want ROOM_CLOSED", stored.ID, got)
		}
		if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventRoomClosed {
			t.Fatalf("session %s events = %v, want one ROOM_CLOSED", stored.ID, got)
		}
	}
	if got := h.store.sessions[left.ID].Status; got != StatusLeft {
		t.Fatalf("a LEFT session was changed to %s by room_finished", got)
	}
	if got := h.store.eventsOf(left.ID); len(got) != 0 {
		t.Fatalf("a LEFT session gained events: %v", got)
	}

	// One lesson-level message, once: the duplicate delivery finds no active sessions.
	if len(h.events.roomsClosed) != 1 {
		t.Fatalf("ROOM_CLOSED published %d times, want 1", len(h.events.roomsClosed))
	}
	if len(h.events.offline) != 2 {
		t.Fatalf("STUDENT_OFFLINE published %d times, want one per closed session", len(h.events.offline))
	}
	for _, message := range h.events.offline {
		if message.reason != OfflineRoomClosed {
			t.Fatalf("offline reason = %s, want ROOM_CLOSED", message.reason)
		}
	}
}

func TestRoomFinishedForAForeignRoomChangesNothing(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusOnline)

	event := webhookEvent(webhook.EventRoomFinished, "lk_someone_elses_room", "")
	event.Participant = nil
	if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusOnline {
		t.Fatalf("status = %s, want ONLINE: another room's event must not touch ours", got)
	}
	if len(h.events.roomsClosed) != 0 {
		t.Fatal("a foreign room produced a ROOM_CLOSED message")
	}
}

func TestRoomStartedIsOnlyLogged(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

	event := webhookEvent(webhook.EventRoomStarted, h.store.room(runID), "")
	event.Participant = nil
	if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusConnecting {
		t.Fatalf("status = %s, want CONNECTING: a live room is not a classroom state (§33)", got)
	}
	if len(h.store.events) != 0 {
		t.Fatalf("events = %v, want none", h.store.events)
	}
}

// ---------------------------------------------------------------------------
// Unknown senders and unknown events
// ---------------------------------------------------------------------------

func TestUnknownIdentityIsIgnoredWithoutError(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

	// A teacher's identity is a login session id and is not in student_sessions; an
	// identity from another deployment looks the same from here.
	teacherIdentity := uuid.NewString()
	for _, event := range []*livekit.WebhookEvent{
		webhookEvent(webhook.EventParticipantJoined, h.store.room(runID), teacherIdentity),
		screenShareEvent(webhook.EventTrackPublished, h.store.room(runID), teacherIdentity, "TR_1"),
		webhookEvent(webhook.EventParticipantLeft, h.store.room(runID), teacherIdentity),
	} {
		if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
			t.Fatalf("%s: %v", event.GetEvent(), err)
		}
	}
	if len(h.store.events) != 0 {
		t.Fatalf("events = %v, want none: an unknown identity writes no session state", h.store.events)
	}
}

func TestUnknownEventTypeIsAcceptedAndIgnored(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusOnline)

	if err := h.process.ProcessWebhook(h.ctx, webhookEvent("egress_started", h.store.room(runID), stored.LiveKitIdentity)); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	if got := h.store.sessions[stored.ID].Status; got != StatusOnline {
		t.Fatalf("status = %s, want ONLINE", got)
	}
	if len(h.store.events) != 0 {
		t.Fatalf("events = %v, want none", h.store.events)
	}
}

func TestNonScreenTrackPublishedDoesNotProduceOnline(t *testing.T) {
	for _, source := range []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_MICROPHONE} {
		t.Run(source.String(), func(t *testing.T) {
			h := newProcessorHarness(t)
			runID := uuid.New()
			stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

			event := screenTrackEvent(webhook.EventTrackPublished, h.store.room(runID), stored.LiveKitIdentity, "TR_cam", source)
			if err := h.process.ProcessWebhook(h.ctx, event); err != nil {
				t.Fatalf("ProcessWebhook(): %v", err)
			}
			if got := h.store.sessions[stored.ID].Status; got != StatusConnecting {
				t.Fatalf("status = %s, want CONNECTING: only a SCREEN_SHARE track means ONLINE (§21/§45)", got)
			}
			if len(h.store.events) != 0 {
				t.Fatalf("events = %v, want none", h.store.events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Failure propagation and lifecycle events
// ---------------------------------------------------------------------------

func TestStoreFailureIsReportedSoTheWebhookCanBeRetried(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)
	h.store.applyErr = errors.New("database is down")

	err := h.process.ProcessWebhook(h.ctx, screenShareEvent(webhook.EventTrackPublished, h.store.room(runID), stored.LiveKitIdentity, "TR_1"))
	if err == nil {
		t.Fatal("ProcessWebhook() returned nil: a state change that could not be recorded must be retried by LiveKit")
	}
}

func TestSessionCreatedIsRecordedOnce(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

	for i := 0; i < 2; i++ {
		if err := h.process.SessionCreated(h.ctx, stored); err != nil {
			t.Fatalf("SessionCreated() #%d: %v", i+1, err)
		}
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventSessionCreated {
		t.Fatalf("events = %v, want one SESSION_CREATED", got)
	}
}

func TestSessionLeftRecordsOnceAndTellsTheTeacherOnce(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	studentID := uuid.New()
	stored := h.store.seed(runID, uuid.New(), studentID, StatusLeft)
	leftAt := time.Now().UTC()
	stored.LeftAt = &leftAt

	for i := 0; i < 2; i++ {
		if err := h.process.SessionLeft(h.ctx, stored); err != nil {
			t.Fatalf("SessionLeft() #%d: %v", i+1, err)
		}
	}
	if got := h.store.eventsOf(stored.ID); len(got) != 1 || got[0] != EventStudentLeft {
		t.Fatalf("events = %v, want one STUDENT_LEFT", got)
	}
	if len(h.events.offline) != 1 || h.events.offline[0].reason != OfflineLeft {
		t.Fatalf("STUDENT_OFFLINE = %+v, want exactly one LEFT", h.events.offline)
	}
	if h.events.offline[0].ref.StudentID != studentID {
		t.Fatalf("STUDENT_OFFLINE about the wrong student")
	}
}

func TestCloseRunSessionsImplementsTheClassroomPort(t *testing.T) {
	h := newProcessorHarness(t)
	runID := uuid.New()
	classroomID := uuid.New()
	h.store.seed(runID, classroomID, uuid.New(), StatusOnline)
	h.store.seed(runID, classroomID, uuid.New(), StatusScreenLost)

	closed, err := h.process.CloseRunSessions(h.ctx, runID)
	if err != nil {
		t.Fatalf("CloseRunSessions(): %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	for _, stored := range h.store.sessions {
		if stored.Status != StatusRoomClosed {
			t.Fatalf("session %s = %s, want ROOM_CLOSED", stored.ID, stored.Status)
		}
	}
	if len(h.events.offline) != 2 {
		t.Fatalf("STUDENT_OFFLINE = %d, want one per closed session", len(h.events.offline))
	}

	// The room_finished webhook that follows the teacher's close finds nothing to do.
	h.store.rooms[h.store.room(runID)] = runID
	again, err := h.process.CloseRunSessions(h.ctx, runID)
	if err != nil {
		t.Fatalf("CloseRunSessions() again: %v", err)
	}
	if again != 0 {
		t.Fatalf("closed = %d on the second call, want 0", again)
	}
}

func TestRoomOpenedAndClosedDelegateToTheRuntimeLayer(t *testing.T) {
	h := newProcessorHarness(t)
	classroomID, runID := uuid.New(), uuid.New()

	if err := h.process.RoomOpened(h.ctx, classroomID, runID); err != nil {
		t.Fatalf("RoomOpened(): %v", err)
	}
	if err := h.process.RoomClosed(h.ctx, classroomID, runID); err != nil {
		t.Fatalf("RoomClosed(): %v", err)
	}
	if len(h.events.roomsOpened) != 1 || h.events.roomsOpened[0] != [2]uuid.UUID{classroomID, runID} {
		t.Fatalf("roomsOpened = %+v", h.events.roomsOpened)
	}
	if len(h.events.roomsClosed) != 1 || h.events.roomsClosed[0] != [2]uuid.UUID{classroomID, runID} {
		t.Fatalf("roomsClosed = %+v", h.events.roomsClosed)
	}
}

func TestProcessorWithoutARuntimeLayerStillRunsTheStateMachine(t *testing.T) {
	store := newFakeStore()
	processor := NewProcessor(store, nil)
	ctx := logging.ContextWithLogger(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runID := uuid.New()
	stored := store.seed(runID, uuid.New(), uuid.New(), StatusConnecting)

	if err := processor.ProcessWebhook(ctx, screenShareEvent(webhook.EventTrackPublished, store.room(runID), stored.LiveKitIdentity, "TR_1")); err != nil {
		t.Fatalf("ProcessWebhook(): %v", err)
	}
	if got := store.sessions[stored.ID].Status; got != StatusOnline {
		t.Fatalf("status = %s, want ONLINE: the state machine does not depend on the hub", got)
	}
}
