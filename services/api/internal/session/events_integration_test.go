package session_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
)

// The Phase 8 event log and the webhook-driven state machine against a real PostgreSQL
// server (§13/§45/§74).
//
// WHAT only the database can prove here: that the CHECK constraint really enumerates
// §13's vocabulary, that `payload` really is jsonb that a query can filter on, that the
// migration is idempotent, and that a webhook transition and its event row land in ONE
// transaction — the properties a fake silently assumes.
//
// Skipped when TEST_DATABASE_URL is unset, so `go test ./...` stays green without a
// server.

// eventTypesOf lists the event rows of one session, oldest first.
func eventTypesOf(t *testing.T, f *fixture, sessionID uuid.UUID) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT type FROM session_events WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read session_events: %v", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		types = append(types, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return types
}

// ---------------------------------------------------------------------------
// The migration
// ---------------------------------------------------------------------------

func TestSessionEventsMigrationIsIdempotent(t *testing.T) {
	// A brand-new database with every migration applied from scratch, so this test
	// observes the real first run rather than a database somebody else migrated.
	pool := dbtest.FreshPool(t)

	applied, err := migrate.Up(context.Background(), pool)
	if err != nil {
		t.Fatalf("second migrate.Up(): %v", err)
	}
	if len(applied) != 0 {
		names := make([]string, 0, len(applied))
		for _, m := range applied {
			names = append(names, m.Filename)
		}
		t.Fatalf("second run applied %v, want nothing: migrations must be idempotent (§60)", names)
	}
}

func TestSessionEventsTableShape(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	// The columns of §4.6, by name and type. A test that only inserted rows would pass
	// against a table that had grown an extra NOT NULL column nothing writes.
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, is_nullable
		  FROM information_schema.columns
		 WHERE table_name = 'session_events'
		 ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	type column struct{ name, dataType, nullable string }
	var columns []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.dataType, &c.nullable); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns = append(columns, c)
	}
	if len(columns) != 5 {
		t.Fatalf("columns = %+v, want exactly the five of schema §4.6", columns)
	}
	want := []column{
		{"id", "uuid", "NO"},
		{"session_id", "uuid", "NO"},
		{"type", "text", "NO"},
		{"payload", "jsonb", "NO"},
		{"created_at", "timestamp with time zone", "NO"},
	}
	for i, expected := range want {
		if columns[i] != expected {
			t.Fatalf("column %d = %+v, want %+v", i, columns[i], expected)
		}
	}

	// The two documented indexes, by name: they are the access paths the replay and the
	// operational queries rely on.
	for _, index := range []string{"session_events_session_idx", "session_events_type_time_idx"} {
		var found bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'session_events' AND indexname = $1)`,
			index).Scan(&found); err != nil {
			t.Fatalf("look up index %s: %v", index, err)
		}
		if !found {
			t.Errorf("index %s is missing (schema §4.6)", index)
		}
	}
}

func TestSessionEventTypeCheckAcceptsTheVocabularyAndRejectsAnythingElse(t *testing.T) {
	f := newFixture(t)
	stored := f.create(t, f.studentA.ID)
	ctx := context.Background()

	vocabulary := []string{
		"SESSION_CREATED", "PARTICIPANT_CONNECTED",
		"SCREEN_PUBLISHED", "SCREEN_LOST", "SCREEN_RESTORED",
		"CAMERA_STARTED", "CAMERA_STOPPED",
		"MIC_STARTED", "MIC_STOPPED",
		"CONNECTION_LOST", "CONNECTION_RESTORED",
		"TEACHER_TALK_STARTED", "TEACHER_TALK_ENDED",
		"STUDENT_LEFT", "ROOM_CLOSED",
	}
	for _, kind := range vocabulary {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO session_events (session_id, type, payload) VALUES ($1, $2, '{}'::jsonb)`,
			stored.ID, kind); err != nil {
			t.Fatalf("type %s was rejected by the CHECK constraint: %v", kind, err)
		}
	}

	// An unknown type is refused by the DATABASE and not only by Go: the constraint is
	// what makes "the event vocabulary is three places that agree" true (§4.6).
	_, err := f.pool.Exec(ctx,
		`INSERT INTO session_events (session_id, type) VALUES ($1, 'SCREEN_VIBES')`, stored.ID)
	if err == nil {
		t.Fatal("an unknown event type was accepted")
	}
	if !strings.Contains(err.Error(), "session_events_type_check") {
		t.Fatalf("error = %v, want the CHECK constraint to be named", err)
	}
}

func TestSessionEventPayloadIsQueryableJSONB(t *testing.T) {
	f := newFixture(t)
	stored := f.create(t, f.studentA.ID)
	ctx := context.Background()

	// A payload shaped like the ones the webhook path writes: identifiers only, never
	// media and never a person's name (§13/§59).
	payload := `{"room":"lk_abc","trackSid":"TR_x","trackSource":"SCREEN_SHARE","reason":"unpublished"}`
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO session_events (session_id, type, payload) VALUES ($1, 'SCREEN_LOST', $2::jsonb)`,
		stored.ID, payload); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	var reason, trackSource string
	if err := f.pool.QueryRow(ctx, `
		SELECT payload->>'reason', payload->>'trackSource' FROM session_events
		 WHERE session_id = $1 AND type = 'SCREEN_LOST'`, stored.ID).Scan(&reason, &trackSource); err != nil {
		t.Fatalf("query jsonb: %v", err)
	}
	if reason != "unpublished" || trackSource != "SCREEN_SHARE" {
		t.Fatalf("payload fields = %q / %q", reason, trackSource)
	}

	// The default is an object, not NULL: a reader can always assume `payload->>'x'` is
	// either a value or NULL, and never an error.
	var isObject bool
	if err := f.pool.QueryRow(ctx,
		`SELECT jsonb_typeof(payload) = 'object' FROM session_events WHERE session_id = $1 LIMIT 1`,
		stored.ID).Scan(&isObject); err != nil {
		t.Fatalf("jsonb_typeof: %v", err)
	}
	if !isObject {
		t.Fatal("payload is not a JSON object")
	}
}

// ---------------------------------------------------------------------------
// The webhook-driven state machine
// ---------------------------------------------------------------------------

func webhookFor(kind, room, identity string) *livekit.WebhookEvent {
	return &livekit.WebhookEvent{
		Event: kind,
		Id:    uuid.NewString(),
		Room:  &livekit.Room{Name: room, Sid: "RM_test"},
		Participant: &livekit.ParticipantInfo{
			Identity: identity,
			Sid:      "PA_test",
		},
	}
}

func screenWebhook(kind, room, identity, sid string) *livekit.WebhookEvent {
	event := webhookFor(kind, room, identity)
	event.Track = &livekit.TrackInfo{Sid: sid, Source: livekit.TrackSource_SCREEN_SHARE}
	return event
}

// cameraWebhook is the same shape for the camera source (§24/§75).
func cameraWebhook(kind, room, identity, sid string) *livekit.WebhookEvent {
	event := webhookFor(kind, room, identity)
	event.Track = &livekit.TrackInfo{Sid: sid, Source: livekit.TrackSource_CAMERA, Type: livekit.TrackType_VIDEO}
	return event
}

// TestWebhookTransitionsArePersistedWithTheirEvents is the §45/§74 acceptance path at the
// persistence layer: an at-least-once, out-of-order delivery stream leaves exactly the
// rows the task book describes.
func TestWebhookTransitionsArePersistedWithTheirEvents(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	identity := stored.LiveKitIdentity
	room := f.roomName

	// --- duplicate participant_joined: one event, no state change -------------------
	joined := webhookFor(webhook.EventParticipantJoined, room, identity)
	for i := 0; i < 2; i++ {
		if err := processor.ProcessWebhook(ctx, joined); err != nil {
			t.Fatalf("participant_joined #%d: %v", i+1, err)
		}
	}
	after := f.session(t, stored.ID)
	if after.Status != "CONNECTING" {
		t.Fatalf("status = %s, want CONNECTING: participant_joined is not evidence of a screen (§45)", after.Status)
	}
	if after.ConnectedAt == nil {
		t.Fatal("connected_at was not recorded")
	}

	// --- out-of-order track_unpublished BEFORE track_published ----------------------
	if err := processor.ProcessWebhook(ctx, screenWebhook(webhook.EventTrackUnpublished, room, identity, "TR_early")); err != nil {
		t.Fatalf("early unpublish: %v", err)
	}
	if got := f.session(t, stored.ID).Status; got != "CONNECTING" {
		t.Fatalf("status = %s, want CONNECTING: there was no screen to lose", got)
	}

	// --- track_published: the authoritative ONLINE ----------------------------------
	published := screenWebhook(webhook.EventTrackPublished, room, identity, "TR_screen")
	for i := 0; i < 2; i++ {
		if err := processor.ProcessWebhook(ctx, published); err != nil {
			t.Fatalf("track_published #%d: %v", i+1, err)
		}
	}
	after = f.session(t, stored.ID)
	if after.Status != "ONLINE" {
		t.Fatalf("status = %s, want ONLINE", after.Status)
	}
	if after.ScreenStartedAt == nil {
		t.Fatal("screen_started_at was not recorded")
	}

	if got := eventTypesOf(t, f, stored.ID); len(got) != 2 ||
		got[0] != "PARTICIPANT_CONNECTED" || got[1] != "SCREEN_PUBLISHED" {
		t.Fatalf("events = %v, want [PARTICIPANT_CONNECTED SCREEN_PUBLISHED] (duplicates must not be recorded)", got)
	}

	// --- track_unpublished: SCREEN_LOST ---------------------------------------------
	if err := processor.ProcessWebhook(ctx, screenWebhook(webhook.EventTrackUnpublished, room, identity, "TR_screen")); err != nil {
		t.Fatalf("track_unpublished: %v", err)
	}
	after = f.session(t, stored.ID)
	if after.Status != "SCREEN_LOST" || after.ScreenLostAt == nil {
		t.Fatalf("status = %s screen_lost_at = %v", after.Status, after.ScreenLostAt)
	}

	// --- the screen comes back ------------------------------------------------------
	if err := processor.ProcessWebhook(ctx, screenWebhook(webhook.EventTrackPublished, room, identity, "TR_screen_2")); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if got := f.session(t, stored.ID).Status; got != "ONLINE" {
		t.Fatalf("status = %s, want ONLINE", got)
	}

	// --- participant_left -----------------------------------------------------------
	if err := processor.ProcessWebhook(ctx, webhookFor(webhook.EventParticipantLeft, room, identity)); err != nil {
		t.Fatalf("participant_left: %v", err)
	}
	if got := f.session(t, stored.ID).Status; got != "DISCONNECTED" {
		t.Fatalf("status = %s, want DISCONNECTED", got)
	}

	want := []string{
		"PARTICIPANT_CONNECTED", "SCREEN_PUBLISHED", "SCREEN_LOST",
		"SCREEN_RESTORED", "CONNECTION_LOST",
	}
	got := eventTypesOf(t, f, stored.ID)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The camera path (§24/§75) against the real event log
// ---------------------------------------------------------------------------

// cameraEventRows reads the camera rows of one session, oldest first, WITH their payload —
// the shape of the stored row is what a later "why does the wall say the camera was on?"
// question is answered from.
func cameraEventRows(t *testing.T, f *fixture, sessionID uuid.UUID) []struct {
	Type    string
	Sid     string
	Source  string
	Payload string
} {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT type, payload->>'trackSid', payload->>'trackSource', payload::text
		  FROM session_events
		 WHERE session_id = $1 AND type IN ('CAMERA_STARTED', 'CAMERA_STOPPED')
		 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read camera events: %v", err)
	}
	defer rows.Close()

	var out []struct {
		Type    string
		Sid     string
		Source  string
		Payload string
	}
	for rows.Next() {
		var row struct {
			Type    string
			Sid     string
			Source  string
			Payload string
		}
		if err := rows.Scan(&row.Type, &row.Sid, &row.Source, &row.Payload); err != nil {
			t.Fatalf("scan camera event: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate camera events: %v", err)
	}
	return out
}

// TestCameraEventsArePersistedWithoutMovingTheSession is the Phase 9 acceptance path at the
// persistence layer: the camera writes CAMERA_STARTED / CAMERA_STOPPED rows with an
// identifier-only payload, and the `student_sessions` row does not move at all.
func TestCameraEventsArePersistedWithoutMovingTheSession(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	room := f.roomName
	identity := stored.LiveKitIdentity

	// A screen to begin with, so the session is ONLINE and the camera can be observed
	// next to a state that must survive it (§24/§21).
	if err := processor.ProcessWebhook(ctx, screenWebhook(webhook.EventTrackPublished, room, identity, "TR_screen")); err != nil {
		t.Fatalf("screen publish: %v", err)
	}
	before := f.session(t, stored.ID)
	if before.Status != "ONLINE" {
		t.Fatalf("status = %s, want ONLINE before the camera is touched", before.Status)
	}

	// --- the camera comes on, twice (at-least-once delivery) ---------------------------
	started := cameraWebhook(webhook.EventTrackPublished, room, identity, "TR_cam")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := processor.ProcessWebhook(ctx, started); err != nil {
			t.Fatalf("camera publish #%d: %v", attempt, err)
		}
	}
	after := f.session(t, stored.ID)
	if !sameSessionRow(after, before) {
		t.Fatalf("session row = %+v, want %+v: a camera must not move the session state machine (§24/§21)", after, before)
	}

	// --- it goes off, twice ------------------------------------------------------------
	stopped := cameraWebhook(webhook.EventTrackUnpublished, room, identity, "TR_cam")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := processor.ProcessWebhook(ctx, stopped); err != nil {
			t.Fatalf("camera unpublish #%d: %v", attempt, err)
		}
	}

	rows := cameraEventRows(t, f, stored.ID)
	if len(rows) != 2 {
		t.Fatalf("camera rows = %+v, want exactly two (one per real change)", rows)
	}
	want := []struct{ kind, sid string }{{"CAMERA_STARTED", "TR_cam"}, {"CAMERA_STOPPED", "TR_cam"}}
	for i, expected := range want {
		if rows[i].Type != expected.kind || rows[i].Sid != expected.sid {
			t.Fatalf("row %d = %+v, want %s for %s", i, rows[i], expected.kind, expected.sid)
		}
		if rows[i].Source != "CAMERA" {
			t.Fatalf("row %d source = %q, want CAMERA", i, rows[i].Source)
		}
	}
	if after := f.session(t, stored.ID); !sameSessionRow(after, before) {
		t.Fatalf("session row = %+v, want %+v: the camera cycle moved it", after, before)
	}

	// The payload is diagnostic metadata: identifiers only, and the check is on the RAW
	// jsonb — "no image, no audio, no name" has to be a property of the stored row (§13).
	var payload string
	if err := f.pool.QueryRow(ctx, `
		SELECT payload::text FROM session_events
		 WHERE session_id = $1 AND type = 'CAMERA_STARTED'`, stored.ID).Scan(&payload); err != nil {
		t.Fatalf("read camera payload: %v", err)
	}
	for _, forbidden := range []string{"image", "data:image", "data:video", "base64", "displayName", "token", "audio"} {
		if strings.Contains(strings.ToLower(payload), strings.ToLower(forbidden)) {
			t.Fatalf("camera payload contains %q: %s", forbidden, payload)
		}
	}
	for _, required := range []string{`"trackSid"`, `"trackSource"`, `"room"`, `"runId"`, `"studentId"`} {
		if !strings.Contains(payload, required) {
			t.Fatalf("camera payload is missing %s: %s", required, payload)
		}
	}
}

// TestCameraUnpublishedBeforePublishedInTheRealLog is the out-of-order rule against real
// SQL: the early stop must not create a row (the camera is off until it is switched on,
// §24), and the late start must be recorded.
func TestCameraUnpublishedBeforePublishedInTheRealLog(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	before := f.session(t, stored.ID)

	if err := processor.ProcessWebhook(ctx, cameraWebhook(webhook.EventTrackUnpublished, f.roomName, stored.LiveKitIdentity, "TR_cam")); err != nil {
		t.Fatalf("early unpublish: %v", err)
	}
	if rows := cameraEventRows(t, f, stored.ID); len(rows) != 0 {
		t.Fatalf("camera rows = %+v, want none before the camera was ever started", rows)
	}

	if err := processor.ProcessWebhook(ctx, cameraWebhook(webhook.EventTrackPublished, f.roomName, stored.LiveKitIdentity, "TR_cam")); err != nil {
		t.Fatalf("late publish: %v", err)
	}
	rows := cameraEventRows(t, f, stored.ID)
	if len(rows) != 1 || rows[0].Type != "CAMERA_STARTED" {
		t.Fatalf("camera rows = %+v, want [CAMERA_STARTED]", rows)
	}
	if after := f.session(t, stored.ID); !sameSessionRow(after, before) {
		t.Fatalf("session row = %+v, want %+v: the camera cycle must not touch it", after, before)
	}
}

// TestCameraRetryAfterAStopInTheRealLog is why the track sid is part of the guard: a
// webhook retried after its publication ended must not turn the camera back on, and the
// next REAL publication (a new sid) must be recorded.
func TestCameraRetryAfterAStopInTheRealLog(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	room, identity := f.roomName, stored.LiveKitIdentity

	for _, event := range []*livekit.WebhookEvent{
		cameraWebhook(webhook.EventTrackPublished, room, identity, "TR_cam_1"),
		cameraWebhook(webhook.EventTrackUnpublished, room, identity, "TR_cam_1"),
		// The retry of the FIRST publication, delivered after it was stopped.
		cameraWebhook(webhook.EventTrackPublished, room, identity, "TR_cam_1"),
	} {
		if err := processor.ProcessWebhook(ctx, event); err != nil {
			t.Fatalf("%s: %v", event.GetEvent(), err)
		}
	}
	rows := cameraEventRows(t, f, stored.ID)
	if len(rows) != 2 || rows[0].Type != "CAMERA_STARTED" || rows[1].Type != "CAMERA_STOPPED" {
		t.Fatalf("camera rows = %+v, want [CAMERA_STARTED CAMERA_STOPPED]: a retry is not a new state", rows)
	}

	// The student switches the camera on again: a new publication, a new row.
	if err := processor.ProcessWebhook(ctx, cameraWebhook(webhook.EventTrackPublished, room, identity, "TR_cam_2")); err != nil {
		t.Fatalf("second camera: %v", err)
	}
	rows = cameraEventRows(t, f, stored.ID)
	if len(rows) != 3 || rows[2].Type != "CAMERA_STARTED" || rows[2].Sid != "TR_cam_2" {
		t.Fatalf("camera rows = %+v, want a third row for the new publication", rows)
	}
}

// TestConcurrentCameraDeliveriesWriteOneRow exercises the transaction-scoped advisory lock
// of ApplyTrackState: webhooks are delivered concurrently, and two goroutines that both read
// "the camera is off" must not both write CAMERA_STARTED.
func TestConcurrentCameraDeliveriesWriteOneRow(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)
	event := cameraWebhook(webhook.EventTrackPublished, f.roomName, stored.LiveKitIdentity, "TR_cam")

	const deliveries = 6
	errs := make(chan error, deliveries)
	start := make(chan struct{})
	for i := 0; i < deliveries; i++ {
		go func() {
			<-start
			errs <- processor.ProcessWebhook(ctx, event)
		}()
	}
	close(start)
	for i := 0; i < deliveries; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent delivery: %v", err)
		}
	}

	if rows := cameraEventRows(t, f, stored.ID); len(rows) != 1 {
		t.Fatalf("camera rows = %+v, want exactly one: a concurrent duplicate is still a duplicate", rows)
	}
}

func TestWebhookCannotResurrectATerminalSession(t *testing.T) {
	f := newFixture(t)
	processor := session.NewProcessor(f.repo, nil)
	ctx := context.Background()

	stored := f.create(t, f.studentA.ID)

	// The teacher closes the lesson: the session is ROOM_CLOSED (terminal).
	closed, err := processor.CloseRunSessions(ctx, f.runID)
	if err != nil {
		t.Fatalf("CloseRunSessions(): %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	// The webhooks that were already in flight arrive afterwards. None of them may move
	// the row (§74).
	for _, event := range []*livekit.WebhookEvent{
		webhookFor(webhook.EventParticipantJoined, f.roomName, stored.LiveKitIdentity),
		screenWebhook(webhook.EventTrackPublished, f.roomName, stored.LiveKitIdentity, "TR_late"),
	} {
		if err := processor.ProcessWebhook(ctx, event); err != nil {
			t.Fatalf("%s: %v", event.GetEvent(), err)
		}
	}
	after := f.session(t, stored.ID)
	if after.Status != "ROOM_CLOSED" {
		t.Fatalf("status = %s, want ROOM_CLOSED: terminal states are never re-entered", after.Status)
	}
	if got := eventTypesOf(t, f, stored.ID); len(got) != 1 || got[0] != "ROOM_CLOSED" {
		t.Fatalf("events = %v, want [ROOM_CLOSED]", got)
	}
}

// sessionRow is the part of a `student_sessions` row these tests assert on: the state
// machine's own columns, and nothing about the camera.
//
// WHY it is a named type with a value comparison: "the camera changed nothing" is the rule
// of Phase 9, and `*time.Time` pointer identity would report a difference where there is
// none (two reads of the same instant are different pointers). Comparing the VALUES is what
// makes the assertion about the database rather than about Go's allocator.
type sessionRow struct {
	Status          string
	ConnectedAt     *time.Time
	ScreenStartedAt *time.Time
	ScreenLostAt    *time.Time
	LeftAt          *time.Time
}

func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	default:
		return a.Equal(*b)
	}
}

func sameSessionRow(a, b sessionRow) bool {
	return a.Status == b.Status &&
		sameTime(a.ConnectedAt, b.ConnectedAt) &&
		sameTime(a.ScreenStartedAt, b.ScreenStartedAt) &&
		sameTime(a.ScreenLostAt, b.ScreenLostAt) &&
		sameTime(a.LeftAt, b.LeftAt)
}

// f.session reads one row straight from the database, so the assertions are about what
// the control plane RECORDED.
func (f *fixture) session(t *testing.T, sessionID uuid.UUID) sessionRow {
	t.Helper()
	var row sessionRow
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status, connected_at, screen_started_at, screen_lost_at, left_at
		  FROM student_sessions WHERE id = $1`, sessionID).
		Scan(&row.Status, &row.ConnectedAt, &row.ScreenStartedAt, &row.ScreenLostAt, &row.LeftAt); err != nil {
		t.Fatalf("read session %s: %v", sessionID, err)
	}
	return row
}
