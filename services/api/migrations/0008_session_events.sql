-- 0008_session_events.sql
--
-- Phase 8: the append-only event log of a student session (task book §13, schema
-- §4.6). One row per thing that happened to one session: the screen went away, the
-- participant dropped, the teacher closed the lesson.
--
-- WHY events exist next to the status column (schema §1: "状态列表达现在，
-- session_events 表达过程"): `student_sessions.status` answers "is this student being
-- supervised RIGHT NOW?", which is what the teacher's wall renders. It cannot answer
-- "when did their screen go away, and did it come back?" — that is a sequence, and a
-- sequence needs rows. Overwriting the status column to keep history would make the
-- wall show the past.
--
-- WHY it hangs off student_sessions and not classroom_runs: an event is about ONE
-- student's media session. "The class had a bad evening" is a query over these rows,
-- not a row of its own.
--
-- WHY append-only, and what that means here: nothing in this codebase ever UPDATEs or
-- DELETEs a row of this table, because a log that can be rewritten cannot be used to
-- reconstruct what a teacher saw. The database does not enforce it with a trigger (no
-- other table in this schema carries one); the enforcement is that there is no code
-- path that writes anything but INSERT, and the tests assert the sequence of rows.

CREATE TABLE session_events (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Scope, not authorization: this column says which session an event belongs to.
    -- Whether a CALLER may read it is decided by internal/session (a student may only
    -- read their own session) — a foreign key never grants access.
    --
    -- ON DELETE CASCADE, unlike the RESTRICT used by every other foreign key in this
    -- schema: an event has no meaning without its session, and there is no API that
    -- deletes a session (leaving is a status, §50). CASCADE is therefore the honest
    -- statement of the dependency rather than a rule anybody can reach today.
    session_id uuid        NOT NULL REFERENCES student_sessions (id) ON DELETE CASCADE,
    -- The vocabulary of §13, enforced here as well as in Go (§80: the CHECK, the
    -- session.EventType constants and the frontend's SessionEventType are three views
    -- of one list and must be changed together). A new event type therefore needs a
    -- migration, which is exactly the review conversation it deserves: an event type
    -- is a fact this system will be asked to explain months later.
    type       text        NOT NULL,
    -- DIAGNOSTIC METADATA ONLY. What may go in here: the LiveKit room name (opaque
    -- lk_<run_uuid>), the track sid and source, the participant sid, the event id, the
    -- reason a transition was applied or skipped.
    --
    -- FORBIDDEN here, without exception (§13/§59): screen or camera images, audio
    -- content, transcripts, full LiveKit tokens, session cookies, passwords, or any
    -- human-meaningful identifier (a name, an account) — V1 does not record media (§53)
    -- and a catch-all jsonb column is the single easiest place to accidentally start.
    -- The same rule applies to the log lines that accompany these rows.
    payload    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- The fifteen types of §13, spelled out. A missing one makes a whole category of
    -- history unrecordable (the write fails and the transition is rolled back with it),
    -- which is why the list is asserted by a test rather than trusted to review.
    CONSTRAINT session_events_type_check CHECK (type IN (
        'SESSION_CREATED', 'PARTICIPANT_CONNECTED',
        'SCREEN_PUBLISHED', 'SCREEN_LOST', 'SCREEN_RESTORED',
        'CAMERA_STARTED', 'CAMERA_STOPPED',
        'MIC_STARTED', 'MIC_STOPPED',
        'CONNECTION_LOST', 'CONNECTION_RESTORED',
        'TEACHER_TALK_STARTED', 'TEACHER_TALK_ENDED',
        'STUDENT_LEFT', 'ROOM_CLOSED'))
);

-- The replay query ("what happened in this session, in order") — schema §5, and the
-- read a future session-detail screen performs. created_at is part of the key because
-- the read is ordered, and a second column in the index keeps that order cheap.
CREATE INDEX session_events_session_idx ON session_events (session_id, created_at);

-- The operational query ("every SCREEN_LOST in the last hour", "is any session in
-- this deployment flapping?"). DESC because those reads ask for the newest first and
-- a forward index would have to scan the whole history to answer.
CREATE INDEX session_events_type_time_idx ON session_events (type, created_at DESC);

COMMENT ON TABLE session_events IS
    'Append-only log of what happened to one student session (§13). Rows are written by the LiveKit webhook path and by the join/leave/close flows; nothing updates or deletes them.';
COMMENT ON COLUMN session_events.type IS
    'SESSION_CREATED | PARTICIPANT_CONNECTED | SCREEN_PUBLISHED | SCREEN_LOST | SCREEN_RESTORED | CAMERA_STARTED | CAMERA_STOPPED | MIC_STARTED | MIC_STOPPED | CONNECTION_LOST | CONNECTION_RESTORED | TEACHER_TALK_STARTED | TEACHER_TALK_ENDED | STUDENT_LEFT | ROOM_CLOSED. Mirrors the CHECK, internal/session.EventType and the frontend contract.';
COMMENT ON COLUMN session_events.payload IS
    'Diagnostic metadata only (opaque room name, track sid/source, participant sid, event id, reason). Never images, audio, full tokens, cookies, passwords or human-meaningful identifiers (§13/§53/§59).';

-- WHY there is no "current session status" column or trigger here: the status lives in
-- student_sessions, written by the same transaction that writes the event. This table
-- records; it never decides.
