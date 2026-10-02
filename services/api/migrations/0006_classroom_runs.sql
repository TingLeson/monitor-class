-- 0006_classroom_runs.sql
--
-- Phase 3: one row per execution of a classroom (task book §8, schema §4.4).
--
-- WHY a separate table instead of opened_at/closed_at on `classrooms`:
-- "C++ 晚自习" runs every evening. Keeping one pair of timestamps would erase
-- yesterday's lesson the moment tonight's starts — the supervision history ("who was
-- connected at 19:40 on Tuesday?") would be unrecoverable. Every CLOSED -> OPEN
-- creates a NEW run; reusing an old one is forbidden (§8), and the media sessions,
-- events and monitoring data of a lesson all attach to this id.

CREATE TABLE classroom_runs (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE RESTRICT: runs are history. A classroom that has ever been opened
    -- cannot be deleted, which is what keeps "what happened in this room?" answerable.
    classroom_id      uuid        NOT NULL REFERENCES classrooms (id) ON DELETE RESTRICT,
    status            text        NOT NULL DEFAULT 'OPEN',
    -- The LiveKit room name, always `lk_<run_uuid>` (§8). Opaque on purpose: this
    -- string is visible to every participant through the media SDK, so a name built
    -- from a student account, a teacher name or a real class name would publish the
    -- school's roster to anyone in the room. A UUID reveals nothing.
    --
    -- UNIQUE because two runs sharing a room name would merge two lessons into one
    -- LiveKit room: students of the second lesson would appear on the first lesson's
    -- monitoring wall. The application generates the value from the run id, but the
    -- database is what makes the collision impossible.
    --
    -- WHY there is no CHECK on the `lk_...` shape: uniqueness and opacity are the
    -- invariants that matter, and neither is expressible as a regexp. A format
    -- constraint would only freeze today's naming scheme into the schema, so the
    -- next phase that needs a different opaque prefix would require a migration to
    -- relax a constraint that never protected anything.
    livekit_room_name text        NOT NULL UNIQUE,
    opened_at         timestamptz NOT NULL DEFAULT now(),
    closed_at         timestamptz NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT classroom_runs_status_valid CHECK (status IN ('OPEN', 'CLOSED')),

    -- Same biconditional style as classrooms_run_consistency: "still running" and
    -- "has no end time" are one fact. Without it a run could be CLOSED with a NULL
    -- closed_at, and every duration report and every "was the student still
    -- connected when the room closed?" question would silently compute from NULL.
    CONSTRAINT classroom_runs_closed_at CHECK (
        (status = 'OPEN' AND closed_at IS NULL) OR
        (status = 'CLOSED' AND closed_at IS NOT NULL)
    )
);

-- At most ONE open run per classroom, enforced by the database and not only by the
-- application. WHY a partial unique index: the open endpoint's transaction already
-- takes `SELECT ... FOR UPDATE` on the classroom row, which serialises two clicks
-- from the same teacher. This index is the second, independent guarantee that
-- covers everything the row lock does not — a retried request that lost its
-- transaction, a future second call site (a scheduler, an import script), or two
-- API processes where one forgot the lock. With it, "two concurrent opens" is a
-- state the database cannot represent, so the loser always fails cleanly instead of
-- producing two live runs for one lesson.
CREATE UNIQUE INDEX classroom_runs_one_open_idx
    ON classroom_runs (classroom_id) WHERE status = 'OPEN';

-- The remaining half of the circular dependency that 0004 could not express:
-- classrooms.current_run_id -> classroom_runs.id. Both tables now exist, so the
-- constraint can be added.
--
-- WHY a plain (NOT DEFERRABLE) foreign key is enough here, and no DEFERRABLE
-- INITIALLY DEFERRED is needed: opening a classroom inserts the run first and then
-- points current_run_id at it, in one transaction. A non-deferred FK is checked at
-- the end of the statement that writes the referencing row, by which time the
-- referenced row exists. There is no window in which the reference is dangling, so
-- paying for deferred checking (and losing the immediate error at the exact
-- statement that caused it) would buy nothing.
--
-- ON DELETE RESTRICT: a run that a classroom currently points at must not vanish.
ALTER TABLE classrooms
    ADD CONSTRAINT classrooms_current_run_fk
    FOREIGN KEY (current_run_id) REFERENCES classroom_runs (id) ON DELETE RESTRICT;

COMMENT ON TABLE classroom_runs IS
    'One row per CLOSED->OPEN transition of a classroom (§8). Never reused, never updated except to close.';
COMMENT ON COLUMN classroom_runs.livekit_room_name IS
    'Opaque room name lk_<run_uuid>. MUST NOT contain a student account, a real name or a class name (§8). Never returned by the API in Phase 3.';
COMMENT ON COLUMN classroom_runs.closed_at IS
    'Set exactly when status becomes CLOSED; the pair is enforced by classroom_runs_closed_at.';
