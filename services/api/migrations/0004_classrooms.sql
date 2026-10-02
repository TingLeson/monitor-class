-- 0004_classrooms.sql
--
-- Phase 3: the classroom container (task book §7/§10, schema §4.2).
--
-- A Classroom is a long-lived course ("C++ 晚自习"), NOT one session of it. It
-- survives any number of openings; each CLOSED -> OPEN transition creates a new
-- classroom_runs row (0006) and the runs are what the history is read from (§6/§8).
-- Modelling the two as one table is the mistake this split exists to prevent: a
-- single row would have to overwrite opened_at/closed_at on every lesson and the
-- previous lesson would simply cease to exist.

CREATE TABLE classrooms (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text        NOT NULL,
    description      text        NULL,
    -- ON DELETE RESTRICT: a classroom is somebody's course with a roster and a run
    -- history. Deleting the account must not silently delete all of that (the
    -- system forbids physical deletion of history, schema §1); the operator has to
    -- deal with the classrooms first.
    owner_teacher_id uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- Two values only, on purpose (§7). WAITING/STARTING/PAUSED/ENDED/ARCHIVED are
    -- explicitly forbidden: every state beyond OPEN/CLOSED is a state the frontend,
    -- the student portal and the monitoring wall would each have to interpret, and
    -- "is this classroom enterable?" must stay a two-way question. Progress within
    -- a lesson belongs to ClassroomRun / StudentSession, not here.
    status           text        NOT NULL DEFAULT 'CLOSED',
    -- Set while OPEN, cleared when CLOSED. The foreign key is added at the END of
    -- 0006 and not here, because classroom_runs does not exist yet and cannot: its
    -- classroom_id column points back at this table. The dependency is genuinely
    -- circular, so one of the two edges has to be added after both tables exist —
    -- this one, because classrooms can be created (and stay CLOSED, with a NULL
    -- current_run_id) before the runs table exists.
    current_run_id   uuid        NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- A name of "" or "   " renders as a nameless row in every list and on every
    -- student's dashboard, which is indistinguishable from a frontend bug.
    CONSTRAINT classrooms_name_not_blank CHECK (length(btrim(name)) > 0),

    CONSTRAINT classrooms_status_valid CHECK (status IN ('OPEN', 'CLOSED')),

    -- THE invariant of this table (§10): "OPEN" and "current_run_id IS NOT NULL"
    -- are two spellings of the same fact, so they are stored as one. Without it,
    -- the transient state "OPEN but no run" would be representable, and a student
    -- entering during that window would land in a classroom whose run — the thing
    -- media sessions, events and the monitoring wall all hang off — does not exist.
    -- Stated as a biconditional so both halves are rejected: OPEN without a run and
    -- CLOSED with a stale run id.
    CONSTRAINT classrooms_run_consistency CHECK (
        (status = 'OPEN' AND current_run_id IS NOT NULL) OR
        (status = 'CLOSED' AND current_run_id IS NULL)
    )
);

-- Serves the only list this table has in Phase 3: "my classrooms", newest first
-- (§42). The second key (created_at DESC) is part of the index because the query
-- orders by it; the id tiebreaker stays in the query, where it is free.
CREATE INDEX classrooms_owner_idx ON classrooms (owner_teacher_id, created_at DESC);

-- §10 requires owner_teacher.role == TEACHER, and a plain foreign key cannot say
-- that: it can only prove the row exists in `users`. The check has to read a second
-- table, which is exactly what a trigger is for.
--
-- WHY the database re-checks something the service already validates: the service
-- check produces the readable error, this one is the guarantee. Owner ids also get
-- written by seed scripts, manual SQL and future features; without the trigger,
-- "classroom owned by a student" would be a legal row that no layer notices until
-- an authorization question is answered wrongly.
--
-- It fires on every INSERT and UPDATE — not only when owner_teacher_id is set —
-- per §10's wording, at the cost of one primary-key lookup per classroom write.
-- Classrooms are written a handful of times per lesson, so the simplicity of
-- "every row this table accepts has a TEACHER owner" is worth more than the lookup.
--
-- Known limit, deliberate: a role change on `users` (demoting a teacher to
-- STUDENT) does not re-fire this trigger, because that is a write to another
-- table. Phase 2's admin API has no role-change path at all (§4), so the only way
-- to reach that state is manual SQL — which the adminctl break-glass CLI and code
-- review cover.
CREATE FUNCTION classrooms_owner_is_teacher() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    owner_role text;
BEGIN
    SELECT role INTO owner_role FROM users WHERE id = NEW.owner_teacher_id;
    IF NOT FOUND THEN
        -- Reported as the foreign-key violation it is (SQLSTATE 23503), so callers
        -- translating constraint errors do not have to learn a second code path.
        RAISE EXCEPTION 'classrooms.owner_teacher_id % does not exist in users', NEW.owner_teacher_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF owner_role <> 'TEACHER' THEN
        RAISE EXCEPTION 'classrooms.owner_teacher_id must reference an account with role = TEACHER, got %', owner_role
            USING ERRCODE = 'check_violation', CONSTRAINT = 'classrooms_owner_is_teacher';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER classrooms_owner_is_teacher_trigger
    BEFORE INSERT OR UPDATE ON classrooms
    FOR EACH ROW EXECUTE FUNCTION classrooms_owner_is_teacher();

-- WHY there is no updated_at trigger, for the same reason 0002 gives for `users`:
-- every write goes through internal/classroom, which sets updated_at = now() in the
-- statement that changes the row. A trigger would be a second, invisible write path
-- that reviewers cannot see in the repository code and that would overwrite a
-- timestamp a backfill legitimately needs to control.

COMMENT ON TABLE classrooms IS
    'Long-lived classroom container (course). Opening one creates a row in classroom_runs; this table only ever holds OPEN or CLOSED.';
COMMENT ON COLUMN classrooms.owner_teacher_id IS
    'The single teacher allowed to edit, open and close this classroom (§4/§48). Must reference a TEACHER account — enforced by trigger.';
COMMENT ON COLUMN classrooms.status IS
    'OPEN/CLOSED only (§7). OPEN implies current_run_id IS NOT NULL and vice versa.';
COMMENT ON COLUMN classrooms.current_run_id IS
    'The currently OPEN classroom_runs row. FK added in 0006 (circular dependency); NULL exactly when status = CLOSED.';
