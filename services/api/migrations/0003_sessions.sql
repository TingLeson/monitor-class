-- 0003_sessions.sql
--
-- Phase 1: server-side sessions for students, teachers and admins (§41).
--
-- WHY opaque server-side sessions instead of a JWT:
--
--   - "Disabled account" and "logout" must take effect on the very next request.
--     A self-contained token stays valid until it expires no matter what the
--     database says, so an account disabled mid-class would keep watching until
--     the token ran out. This system is a supervision tool; that is unacceptable.
--   - A session row is revocable, auditable and enumerable ("what is logged in
--     right now?"); a JWT is none of those without extra infrastructure.
--   - The token carries no claims at all, so there is nothing in the client's
--     possession to tamper with: role and status are read from `users` on every
--     request (§37).

CREATE TABLE sessions (
    -- No gen_random_uuid() default here (unlike `users`): the API mints the id
    -- before the INSERT so the session id can be put in the log line and in the
    -- request context even if the INSERT itself fails.
    id           uuid        PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the opaque token. THE RAW TOKEN IS NEVER STORED ANYWHERE ON THE
    -- SERVER: it exists only in the client's HttpOnly cookie, so a database dump,
    -- a backup, a replica or a log that leaks this table cannot be replayed as a
    -- login. SHA-256 (not Argon2) is correct here because the input is 256 bits
    -- of CSPRNG output, not a guessable password (§41).
    token_hash   bytea       NOT NULL UNIQUE,
    -- Per-session CSRF token, readable by the frontend (non-HttpOnly companion
    -- cookie) and compared, in constant time, against the X-CSRF-Token header on
    -- every unsafe method (§63).
    csrf_token   text        NOT NULL,
    issued_at    timestamptz NOT NULL DEFAULT now(),
    -- Absolute expiry, fixed at login. There is no sliding renewal: in a
    -- supervision system "never get logged out" is not a goal, and a predictable
    -- fixed lifetime is what makes an incident timeline answerable ("who could
    -- still have been connected at 14:05?").
    expires_at   timestamptz NOT NULL,
    -- Soft revocation. The row is kept because it is the audit trail of the
    -- login; every query that authenticates a request filters revoked rows out.
    revoked_at   timestamptz NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent   text        NULL,
    ip           inet        NULL,

    CONSTRAINT sessions_expires_after_issued CHECK (expires_at > issued_at)
);

COMMENT ON TABLE sessions IS
    'Opaque server-side sessions. Only the SHA-256 of the token is stored; the raw token lives exclusively in the client cookie.';
COMMENT ON COLUMN sessions.token_hash IS
    'SHA-256(raw token). Never log, never return, never compare with ==; lookups go through this unique index.';
COMMENT ON COLUMN sessions.revoked_at IS
    'Set by logout, by "disable account" or by a password reset. Non-NULL rows are kept for audit but never authenticate a request.';
COMMENT ON COLUMN sessions.last_seen_at IS
    'Throttled activity timestamp (SESSION_IDLE_TOUCH_INTERVAL), used for operational visibility — not for extending expires_at.';

-- Partial index: every authentication lookup is "this user's live sessions"
-- (logout-everywhere, revoke-on-disable, cleanup on login). Revoked rows are the
-- growing part of the table and are of no interest to those queries.
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;

-- Supports deleting expired rows without a sequential scan. Phase 1 has no
-- background job, so the cleanup runs opportunistically at login; the index is
-- what keeps that cheap once the table has grown.
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
