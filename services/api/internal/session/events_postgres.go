package session

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file is the persistence half of the Phase 8 event path (§13/§45/§74): the
// guarded state transitions the LiveKit webhook drives, and the append-only event
// rows that record them.
//
// # The one idea that shapes every statement here
//
// A webhook is an OBSERVATION of the media plane, delivered at least once, in no
// guaranteed order, and possibly long after the fact (§45). It is therefore never
// allowed to say "set this session to ONLINE"; it may only say "if this session is
// still in one of these states, move it there". That is the compare-and-set below,
// and it is what makes duplicate delivery a no-op and an out-of-order delivery
// harmless: the second `track_unpublished` finds the session already in SCREEN_LOST,
// and a `track_published` that arrives after the session was closed finds a terminal
// state it is not allowed to leave.
//
// # Why the event row is written in the same transaction
//
// The event log is the record of what the state column did. A crash between the
// UPDATE and the INSERT would leave a state change nobody can explain, and "why does
// the wall say SCREEN_LOST?" is exactly the question this table exists to answer.

// SessionInRoomByIdentity resolves a media identity to its session inside one room.
//
// The JOIN on classroom_runs is the room check: the identity is an opaque UUID that
// happens to be unique across the whole table, but a webhook naming a room is only
// allowed to move sessions of THAT room. Without the join, a replayed event from last
// week's room could move this week's session.
func (p *Postgres) SessionInRoomByIdentity(ctx context.Context, roomName, identity string) (*StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	query := `
		SELECT` + sessionColumns("ss.") + `
		FROM student_sessions ss
		JOIN classroom_runs r ON r.id = ss.classroom_run_id
		WHERE r.livekit_room_name = $1 AND ss.livekit_identity = $2`

	stored, err := scanSession(p.pool.QueryRow(ctx, query, roomName, identity))
	if errors.Is(err, pgx.ErrNoRows) {
		// "Not a student session of this room" — a teacher's login-session identity, an
		// identity from another deployment, or a session that was deleted. One answer
		// for all three: the caller logs it and answers 200 (§45).
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// RunByRoomName resolves a media room to the run and classroom it belongs to.
func (p *Postgres) RunByRoomName(ctx context.Context, roomName string) (ClassroomRef, bool, error) {
	if p == nil || p.pool == nil {
		return ClassroomRef{}, false, errors.New("session: repository is not connected")
	}
	const query = `
		SELECT r.id, r.classroom_id
		FROM classroom_runs r
		WHERE r.livekit_room_name = $1`

	var ref ClassroomRef
	err := p.pool.QueryRow(ctx, query, roomName).Scan(&ref.RunID, &ref.ClassroomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClassroomRef{}, false, nil
	}
	if err != nil {
		return ClassroomRef{}, false, err
	}
	return ref, true, nil
}

// ClassroomRefByRunID is RunByRoomName keyed by the run instead of the room.
func (p *Postgres) ClassroomRefByRunID(ctx context.Context, runID uuid.UUID) (ClassroomRef, bool, error) {
	if p == nil || p.pool == nil {
		return ClassroomRef{}, false, errors.New("session: repository is not connected")
	}
	const query = `SELECT r.id, r.classroom_id FROM classroom_runs r WHERE r.id = $1`

	var ref ClassroomRef
	err := p.pool.QueryRow(ctx, query, runID).Scan(&ref.RunID, &ref.ClassroomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClassroomRef{}, false, nil
	}
	if err != nil {
		return ClassroomRef{}, false, err
	}
	return ref, true, nil
}

// ApplyTransition persists one guarded state change plus its event row.
//
// The status guard is the whole concurrency story: `WHERE id = $1 AND status = ANY($2)`
// either matches the state the caller reasoned about or matches nothing at all. A
// stale observation therefore cannot resurrect a session, and a duplicate webhook
// cannot append a second event — the second call finds the row already in the target
// state, which is not in the `From` set.
//
// `OnlyIfNeverConnected` narrows that further for the one transition whose state does
// not change (participant_joined on a session that is still CONNECTING): there the
// guard has to be the timestamp, or a duplicate would be indistinguishable from a
// first delivery.
func (p *Postgres) ApplyTransition(ctx context.Context, t Transition) (*TransitionResult, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	if !t.Applied() {
		// A programming error rather than a data condition: refusing here keeps an
		// accidentally empty guard from writing an unguarded UPDATE.
		return nil, errors.New("session: transition needs at least one source state and a target state")
	}
	if len(t.From) == 0 {
		return nil, errors.New("session: transition needs at least one source state")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Rollback after a successful commit is a no-op, so this is the simple and safe
	// form: every early return leaves nothing behind.
	defer func() { _ = tx.Rollback(ctx) }()

	from := make([]string, 0, len(t.From))
	for _, status := range t.From {
		from = append(from, string(status))
	}
	// $7 is the "never connected" narrowing. `NOT $7 OR connected_at IS NULL` keeps the
	// statement usable for the ordinary transitions without a second query shape.
	query := `
		UPDATE student_sessions
		   SET status            = $3,
		       connected_at      = CASE WHEN $4 THEN COALESCE(connected_at, now()) ELSE connected_at END,
		       screen_started_at = CASE WHEN $5 THEN COALESCE(screen_started_at, now()) ELSE screen_started_at END,
		       screen_lost_at    = CASE WHEN $6 THEN now() ELSE screen_lost_at END,
		       updated_at        = now()
		 WHERE id = $1
		   AND status = ANY($2::text[])
		   AND (NOT $7 OR connected_at IS NULL)
		RETURNING` + sessionColumns("")

	stored, err := scanSession(tx.QueryRow(ctx, query,
		t.SessionID, from, string(t.To),
		t.MarkConnected, t.MarkScreenStarted, t.MarkScreenLost, t.OnlyIfNeverConnected,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard did not match: a stale observation or a duplicate delivery. Not an
		// error, and deliberately not an event row (§74).
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &TransitionResult{}, nil
	}
	if err != nil {
		return nil, translateWriteError(err)
	}

	result := &TransitionResult{Session: stored}
	if t.Event != "" {
		event, err := insertEventRow(ctx, tx, stored.ID, t.Event, withSessionContext(t.Payload, stored))
		if err != nil {
			return nil, err
		}
		result.Event = event
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// CloseRunSessions marks every active session of a run ROOM_CLOSED (§49/§45).
//
// One statement, two data-modifying CTEs: the UPDATE and the event INSERT see the same
// set of rows, so "the session is ROOM_CLOSED" and "there is a ROOM_CLOSED event" can
// never disagree — which is the whole point of the event log.
//
// The guard is the ACTIVE status list, so the statement is idempotent by construction:
// a teacher close followed by the room_finished webhook closes the same run twice and
// the second call matches nothing (the sessions are already terminal), for both the
// UPDATE and the events.
func (p *Postgres) CloseRunSessions(ctx context.Context, runID uuid.UUID, event EventType, payload map[string]any) ([]StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	if runID == uuid.Nil {
		return nil, errors.New("session: a run id is required to close its sessions")
	}

	query := `
		WITH updated AS (
			UPDATE student_sessions
			   SET status = 'ROOM_CLOSED', updated_at = now()
			 WHERE classroom_run_id = $1
			   AND status IN ('CONNECTING', 'ONLINE', 'SCREEN_LOST', 'DISCONNECTED')
			RETURNING` + sessionColumns("") + `
		), logged AS (
			INSERT INTO session_events (session_id, type, payload)
			SELECT id, $2, $3::jsonb FROM updated
			RETURNING id
		)
		SELECT` + sessionColumns("") + ` FROM updated`

	rows, err := p.pool.Query(ctx, query, runID, string(event), jsonPayload(payload))
	if err != nil {
		return nil, translateWriteError(err)
	}
	defer rows.Close()

	closed := make([]StudentSession, 0, 8)
	for rows.Next() {
		stored, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		closed = append(closed, *stored)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return closed, nil
}

// RecordEventOnce appends one event of a type for a session unless it already exists.
//
// The NOT EXISTS guard is on (session_id, type) and uses `session_events_session_idx`.
// It is the idempotency of the two lifecycle events that have no state transition to
// guard on: a session row is created once, and STUDENT_LEFT can only be recorded once
// because LEFT is terminal (§50) and a re-entry is a NEW row.
func (p *Postgres) RecordEventOnce(ctx context.Context, sessionID uuid.UUID, event EventType, payload map[string]any) (bool, error) {
	if p == nil || p.pool == nil {
		return false, errors.New("session: repository is not connected")
	}
	const query = `
		INSERT INTO session_events (session_id, type, payload)
		SELECT $1, $2, $3::jsonb
		WHERE NOT EXISTS (
			SELECT 1 FROM session_events WHERE session_id = $1 AND type = $2
		)
		RETURNING id`

	var id uuid.UUID
	err := p.pool.QueryRow(ctx, query, sessionID, string(event), jsonPayload(payload)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already recorded by an earlier call (a double-clicked join, a retried leave).
		return false, nil
	}
	if err != nil {
		return false, translateWriteError(err)
	}
	return true, nil
}

// insertEventRow appends one row inside the caller's transaction.
func insertEventRow(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, event EventType, payload map[string]any) (*SessionEvent, error) {
	const query = `
		INSERT INTO session_events (session_id, type, payload)
		VALUES ($1, $2, $3::jsonb)
		RETURNING id, session_id, type, payload, created_at`

	var (
		stored     SessionEvent
		kind       string
		payloadRaw []byte
	)
	if err := tx.QueryRow(ctx, query, sessionID, string(event), jsonPayload(payload)).
		Scan(&stored.ID, &stored.SessionID, &kind, &payloadRaw, &stored.CreatedAt); err != nil {
		return nil, translateWriteError(err)
	}
	stored.Type = EventType(kind)
	stored.Payload = decodePayload(payloadRaw)
	return &stored, nil
}

// withSessionContext adds the identifiers every event row benefits from when it is
// read back months later. They are opaque UUIDs, never names (§8/§44).
//
// WHY the identifiers are copied into the payload at all, when session_id already
// links the row: a session row can be joined away, and an operator grepping one event
// out of a log dump should still see which run it belonged to without a second query.
func withSessionContext(payload map[string]any, session *StudentSession) map[string]any {
	out := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		out[key] = value
	}
	if session != nil {
		out["runId"] = session.ClassroomRunID.String()
		out["studentId"] = session.StudentID.String()
	}
	return out
}

// jsonPayload renders a payload for a jsonb parameter.
//
// An empty map is stored as `{}` and never as SQL NULL: every read of a payload can
// then assume an object, and `payload->>'reason'` on a NULL would silently be NULL for
// a row that was merely written by an older code path.
//
// A value that cannot be marshalled is replaced by a marker string rather than failing
// the write: the state transition is the load-bearing half of this call, and losing the
// audit row over a diagnostic field would be the wrong trade. The marker is greppable,
// so a bad payload is visible instead of silent.
func jsonPayload(payload map[string]any) string {
	if len(payload) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"payloadError":"unencodable"}`
	}
	return string(encoded)
}

// decodePayload keeps a payload readable in Go without exposing a jsonb-specific type
// to the rest of the package. A payload that cannot be decoded becomes an empty map:
// the event's identity and time are the load-bearing columns, and a malformed payload
// must not be able to fail a read of the session itself.
func decodePayload(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

// compile-time assertion that *Postgres serves the runtime event path too.
var _ EventStore = (*Postgres)(nil)
