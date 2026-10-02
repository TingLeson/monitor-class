-- 0002_users.sql
--
-- Phase 1: accounts, roles and password hashes (task book §2.1/§3/§4/§9).
--
-- This table is the root of every authorization decision in the system. The
-- frontend only hides what a user should not see; whether a request is allowed
-- is answered by a row here, joined at request time — never by a cookie, a JWT
-- claim or a client-side route guard (§37/§63).

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- citext (installed in 0001) makes the account case-insensitive AND unique:
    -- "S10086" and "s10086" cannot become two accounts. Enforcing it here rather
    -- than in application code means no future import script, admin fix-up or ad
    -- hoc SQL can create the ambiguity an impersonation attempt relies on.
    account       citext      NOT NULL UNIQUE,
    display_name  text        NOT NULL,
    role          text        NOT NULL,
    -- Argon2id PHC string only (see internal/auth/password.go). NULL for students
    -- by construction, not by convention — see users_password_by_role below.
    password_hash text        NULL,
    status        text        NOT NULL DEFAULT 'ACTIVE',
    -- Who created this account (an admin, §68). ON DELETE SET NULL: deleting an
    -- admin must never cascade into deleting the students they created.
    created_by    uuid        NULL REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz NULL,

    -- Roles and statuses are constrained strings rather than PostgreSQL ENUMs:
    -- a CHECK constraint and the Go constants in internal/user can be kept in
    -- lockstep, whereas ENUM values cannot be removed and are painful to migrate.
    CONSTRAINT users_role_valid CHECK (role IN ('ADMIN', 'TEACHER', 'STUDENT')),
    CONSTRAINT users_status_valid CHECK (status IN ('ACTIVE', 'DISABLED')),

    -- A display name of " " or "" would render as a nameless row in the teacher
    -- console, which is indistinguishable from a UI bug when it happens.
    CONSTRAINT users_display_name_not_blank CHECK (length(btrim(display_name)) > 0),

    -- The account is the only credential a student has (§2.2), so the format is
    -- a security boundary, not cosmetics: no whitespace, no control characters
    -- and no Unicode lookalikes that could be mistaken for another student's
    -- account on screen while comparing unequal in the database.
    CONSTRAINT users_account_format CHECK (account ~ '^[A-Za-z0-9._-]{3,64}$'),

    -- §9: STUDENT must have no password, TEACHER/ADMIN must have one. Stated as
    -- one biconditional so the database rejects both a student created with a
    -- leftover hash and a teacher created without one — the two ways this rule
    -- would otherwise rot.
    CONSTRAINT users_password_by_role CHECK (
        (role = 'STUDENT' AND password_hash IS NULL) OR
        (role <> 'STUDENT' AND password_hash IS NOT NULL)
    )
);

-- Serves "list the active teachers / active students" (admin user management in
-- Phase 2) and the disabled-account audit an operator runs after an incident.
CREATE INDEX users_role_status_idx ON users (role, status);

-- WHY there is no set_updated_at() trigger here:
--
-- Every write to this table goes through internal/user, which sets
-- updated_at = now() explicitly in the same statement that changes the row. A
-- trigger would add a second, invisible write path that reviewers cannot see in
-- the repository code, silently overwrite an application-supplied timestamp
-- (which would break an import or a backfill that must preserve real times) and
-- make `updated_at` change even when a statement updates nothing. The rule this
-- codebase follows is: the database enforces invariants that must never be
-- violated (the CHECKs above), the application owns bookkeeping it may
-- legitimately need to control.

COMMENT ON TABLE users IS
    'Control Plane accounts (ADMIN/TEACHER/STUDENT). The only authority for authentication and RBAC.';
COMMENT ON COLUMN users.account IS
    'Case-insensitive login identifier (citext). Restricted to [A-Za-z0-9._-]{3,64} to prevent homoglyph/blank accounts.';
COMMENT ON COLUMN users.password_hash IS
    'Argon2id PHC string for TEACHER/ADMIN, NULL for STUDENT. Never returned by any API and never logged.';
COMMENT ON COLUMN users.status IS
    'DISABLED accounts cannot log in and cannot keep using an existing session (enforced at every request).';
