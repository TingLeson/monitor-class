-- 0007_student_sessions.sql
--
-- Phase 6: one student's media session inside one ClassroomRun (task book §12,
-- schema §4.5). This is the table the control plane writes when a student presses
-- "进入课堂" and the row every observation of the media plane is folded into.
--
-- WHY a session row exists at all, when LiveKit already knows who is in the room:
-- LiveKit is the media plane and it is not a Source of Truth (§33). "A participant
-- exists" cannot answer "is this student supposed to be in this lesson?", "when did
-- their screen first come up?", or "did they leave at 20:31 or lose their network?".
-- Those are business facts, they must survive a LiveKit restart, and they are what
-- the teacher's wall (§51) and Phase 8's events are built from.
--
-- WHY it hangs off classroom_run_id and not classroom_id (§8/§12): attendance,
-- screen history and monitoring belong to ONE execution of a course. Attaching them
-- to the long-lived classroom would merge every evening into one row and make
-- "what happened on Tuesday?" unanswerable.

CREATE TABLE student_sessions (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE RESTRICT for the same reason classroom_runs uses it: a lesson that
    -- somebody attended is history, and deleting the run must not silently erase who
    -- was in it.
    classroom_run_id  uuid        NOT NULL REFERENCES classroom_runs (id) ON DELETE RESTRICT,
    -- ON DELETE RESTRICT: accounts are not deleted while they are part of a lesson's
    -- record. Removing a student is an administrative action that has to face this.
    student_id        uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- The identity this participant uses in the media room §44. It equals the id
    -- below, and the CHECK at the bottom is what makes that true for every writer,
    -- including a future one that has not read this comment.
    --
    -- UNIQUE is not decoration: identity is what every media-plane observation
    -- (ListParticipants, and the Phase 8 webhooks) is keyed by. Two sessions sharing
    -- an identity would mean two students' tracks folded into one monitoring tile,
    -- and LiveKit would kick one of them out on connect ("same identity rejoins"),
    -- so the collision would look like a random disconnect instead of a data bug.
    livekit_identity  text        NOT NULL UNIQUE,
    -- No DEFAULT: a session must be created in a state somebody chose. A default
    -- would let a future INSERT land in CONNECTING without that being a decision,
    -- and the row would then claim a media connection nobody ever attempted.
    status            text        NOT NULL,
    -- The timestamps below are written by the CONTROL PLANE from what it observed,
    -- never by the browser (§45: "我已经成功了" from a client is not evidence).
    connected_at      timestamptz NULL,
    screen_started_at timestamptz NULL,
    screen_lost_at    timestamptz NULL,
    left_at           timestamptz NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    -- The six V1 states of §12. Terminal states are LEFT and ROOM_CLOSED: a later
    -- phase that wants to resume a left session must say so by changing this CHECK,
    -- which is the review conversation that change deserves.
    CONSTRAINT student_sessions_status_valid CHECK (status IN (
        'CONNECTING', 'ONLINE', 'SCREEN_LOST', 'DISCONNECTED', 'LEFT', 'ROOM_CLOSED')),

    -- The media identity IS the session id (§44). WHY this is a database constraint
    -- and not a convention: identity is the only string of ours that reaches other
    -- participants and the LiveKit dashboard, and it must never be a name, an
    -- account, or a phone number (§8/§26). Any scheme other than "equals a UUID the
    -- control plane generated" has to be argued for in a migration.
    CONSTRAINT student_sessions_identity_is_id CHECK (livekit_identity = id::text)
);

-- At most ONE active session per (run, student) — §50, enforced by the database and
-- not only by the service.
--
-- WHY partial: DISCONNECTED counts as active (a dropped connection is the same
-- student, the same lesson, and LiveKit's rejoin kicks their old connection, so the
-- session row must be reused rather than duplicated — otherwise the wall grows a
-- second "张三" every time somebody's wifi blinks). LEFT and ROOM_CLOSED are the
-- terminal states and are excluded: leaving is a decision, and the record of it must
-- be able to coexist with a later re-entry in the same lesson.
--
-- This index is also the arbiter the join path's `INSERT ... ON CONFLICT DO UPDATE`
-- targets, which is what makes "two concurrent joins from one browser" collapse into
-- one row without a read-then-write race the two requests could both win.
CREATE UNIQUE INDEX student_sessions_active_idx
    ON student_sessions (classroom_run_id, student_id)
    WHERE status IN ('CONNECTING', 'ONLINE', 'SCREEN_LOST', 'DISCONNECTED');

-- The monitoring wall's query (§51): every session of the run currently OPEN, in one
-- indexed scan. `status` is the second column because the read is "who is in this
-- lesson?" and the filter on the six states is part of the same access path.
CREATE INDEX student_sessions_run_idx ON student_sessions (classroom_run_id, status);

-- WHY there is NO session_events table here: events are Phase 8 (§13/§74), together
-- with the LiveKit webhook that produces them. Creating an empty event table now
-- would imply that "who stopped sharing when" is already recorded, when in Phase 6
-- the only observation channel is the poll in the monitor endpoint. `lastEventAt` in
-- the monitor DTO therefore reports updated_at (see internal/session).

COMMENT ON TABLE student_sessions IS
    'One student''s media session in one ClassroomRun (§12). Rows are created by the join API and advanced by server-side observation, never by client claims.';
COMMENT ON COLUMN student_sessions.livekit_identity IS
    'LiveKit participant identity, always equal to id (student_sessions_identity_is_id). Opaque UUID by contract; a name here would leak the roster to every participant (§8/§44).';
COMMENT ON COLUMN student_sessions.status IS
    'CONNECTING | ONLINE | SCREEN_LOST | DISCONNECTED | LEFT | ROOM_CLOSED. Advanced by the control plane from media-plane observation; LEFT and ROOM_CLOSED are terminal.';
COMMENT ON COLUMN student_sessions.connected_at IS
    'First time the control plane observed this identity present in the room. Set once; NULL means "never actually connected", which is how a session stuck in CONNECTING is recognisable.';
COMMENT ON COLUMN student_sessions.screen_started_at IS
    'First time a SCREEN_SHARE track was observed for this session. The §21 invariant ONLINE => screen track exists is what keeps this column meaningful.';
