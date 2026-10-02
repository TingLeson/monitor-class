-- 0005_classroom_students.sql
--
-- Phase 3: the authorization list — which students may see and enter which
-- classroom (task book §11, schema §4.3).

CREATE TABLE classroom_students (
    -- ON DELETE CASCADE: this row is not history, it is an authorization. When the
    -- classroom itself is deleted there is nothing left to authorize, and keeping
    -- orphaned grants would only leave rows that can never match a query.
    classroom_id uuid        NOT NULL REFERENCES classrooms (id) ON DELETE CASCADE,
    -- ON DELETE RESTRICT: the opposite choice, and deliberately so. The roster is an
    -- audit trail of who was allowed into a lesson ("was this student supposed to be
    -- there?"), and deleting an account must not quietly rewrite that answer. An
    -- operator deleting a student has to remove them from their classrooms first,
    -- which is the moment they are forced to notice what the deletion means.
    student_id   uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    added_at     timestamptz NOT NULL DEFAULT now(),
    -- Who granted access. ON DELETE SET NULL: if the teacher account is ever removed
    -- the grant itself must survive, minus the attribution.
    added_by     uuid        NULL REFERENCES users (id) ON DELETE SET NULL,

    -- The composite primary key IS the "no duplicates" rule: a student is on a
    -- roster at most once, so the add operation is idempotent by construction
    -- (INSERT ... ON CONFLICT DO NOTHING) instead of by a read-then-write check in
    -- the service that two concurrent requests could both pass.
    PRIMARY KEY (classroom_id, student_id)
);

-- The primary key covers "who is in this classroom?" (leading column). This index
-- covers the mirror question, which is the student portal's only listing query:
-- "which classrooms am I authorized for?" (§14). Without it that query is a
-- sequential scan of every grant in the school — the exact shape that gets slower
-- with every teacher who uses the product.
CREATE INDEX classroom_students_student_idx ON classroom_students (student_id);

-- WHY there is no trigger forcing student_id to reference role = STUDENT here,
-- while classrooms.owner_teacher_id does have one:
--
-- The two columns are read differently. owner_teacher_id is a PERMISSION — the
-- answer to "may this account open this classroom?" — so a wrong value is an
-- authorization hole that must be impossible in the table. student_id is only an
-- ENTITLEMENT: it grants visibility into a classroom the teacher already owns, and
-- the route that consumes it validates the account (STUDENT + ACTIVE) at the moment
-- it is granted (§11). A hard trigger would also make a legitimate later change
-- (an account's role changed by an administrator, a student disabled) fail on a
-- table nobody is writing to, turning a policy decision into a database outage.
-- The service is the enforcement point; this table records who was granted what,
-- and when.

COMMENT ON TABLE classroom_students IS
    'Per-classroom student authorization (§11). Being on this list is what makes a classroom visible to a student.';
COMMENT ON COLUMN classroom_students.added_by IS
    'Teacher who granted access. Kept for audit; NULL if that account is later deleted.';
