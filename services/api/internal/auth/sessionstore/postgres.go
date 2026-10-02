// Package sessionstore is the PostgreSQL implementation of auth.SessionStore.
//
// It is a separate package from internal/auth so the service can be unit tested
// against a fake store while the SQL stays reviewable in one file. The dependency
// direction is one-way: sessionstore imports auth, never the other way round.
package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// sessionColumns is shared by every query so SELECT and Scan cannot drift apart.
const sessionColumns = `s.id, s.user_id, s.token_hash, s.csrf_token, s.issued_at, s.expires_at,
	s.revoked_at, s.last_seen_at, s.user_agent, s.ip`

// Store is the pgx-backed session store.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps a shared pool. The pool is owned by the caller.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Create inserts a session row.
//
// last_seen_at starts at issued_at rather than at the database default: the
// throttled touch logic compares against it, and a row whose "last seen" is the
// moment the clock happened to write the INSERT is a lie of a few milliseconds
// that serves nobody.
func (s *Store) Create(ctx context.Context, params auth.CreateSessionParams) (*auth.Session, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	const query = `
		INSERT INTO sessions
			(id, user_id, token_hash, csrf_token, issued_at, expires_at, last_seen_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $7, $8)
		RETURNING id, user_id, token_hash, csrf_token, issued_at, expires_at,
		          revoked_at, last_seen_at, user_agent, ip`

	row := s.pool.QueryRow(ctx, query,
		params.ID,
		params.UserID,
		params.TokenHash,
		params.CSRFToken,
		params.IssuedAt,
		params.ExpiresAt,
		nullableUserAgent(params.UserAgent),
		params.IP,
	)
	session, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// FindByTokenHash resolves a token hash to a live session plus the account's
// current role and status.
//
// Revoked and expired rows are filtered out in SQL: "a logged-out session cannot
// authenticate" is then a database-level guarantee rather than a rule a future
// call site has to remember (the schema doc's principle that business invariants
// belong in the database). A DISABLED account IS returned, with its status, so
// the service can distinguish ACCOUNT_DISABLED from AUTH_REQUIRED and revoke the
// row.
//
// The JOIN reads role/status on every request on purpose: a session created while
// the account was ACTIVE must stop working the moment an admin disables it, and a
// role change must take effect immediately (§37).
func (s *Store) FindByTokenHash(ctx context.Context, hash []byte) (*auth.SessionWithUser, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	const query = `
		SELECT ` + sessionColumns + `,
		       u.account, u.display_name, u.role, u.status, u.created_at, u.last_login_at
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1
		   AND s.revoked_at IS NULL
		   AND s.expires_at > now()`

	var (
		out       auth.SessionWithUser
		userAgent *string
		ip        *netip.Addr
		role      string
		status    string
	)
	err := s.pool.QueryRow(ctx, query, hash).Scan(
		&out.ID, &out.UserID, &out.TokenHash, &out.CSRFToken, &out.IssuedAt, &out.ExpiresAt,
		&out.RevokedAt, &out.LastSeenAt, &userAgent, &ip,
		&out.Account, &out.DisplayName, &role, &status, &out.UserCreatedAt, &out.UserLastLoginAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, auth.ErrSessionNotFound
		}
		return nil, err
	}
	if userAgent != nil {
		out.UserAgent = *userAgent
	}
	out.IP = ip
	out.Role = user.Role(role)
	out.UserStatus = user.Status(status)
	return &out, nil
}

// Revoke marks one session as revoked.
//
// Setting revoked_at on an already revoked row is a no-op, which keeps logout
// idempotent without a read-modify-write race.
func (s *Store) Revoke(ctx context.Context, id uuid.UUID) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// RevokeAllForUser revokes every live session of one account.
//
// Used when an account is disabled or its password is reset: both are "the old
// credential must stop working now" events.
func (s *Store) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// Touch records activity. It never extends expires_at — see migrations/0003.
func (s *Store) Touch(ctx context.Context, id uuid.UUID, now time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET last_seen_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now)
	return err
}

// DeleteExpired removes one account's expired rows.
//
// Scoped to a user rather than global so the statement is bounded and can run
// inside a login without holding a lock over the whole table. Phase 1 has no
// background job; this keeps the table from growing without one.
func (s *Store) DeleteExpired(ctx context.Context, userID uuid.UUID) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1 AND expires_at <= now()`, userID)
	return err
}

func (s *Store) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("sessionstore: not connected")
	}
	return nil
}

// scanSession reads one row of sessionColumns.
func scanSession(row pgx.Row) (*auth.Session, error) {
	var (
		out       auth.Session
		userAgent *string
		ip        *netip.Addr
	)
	if err := row.Scan(
		&out.ID, &out.UserID, &out.TokenHash, &out.CSRFToken, &out.IssuedAt, &out.ExpiresAt,
		&out.RevokedAt, &out.LastSeenAt, &userAgent, &ip,
	); err != nil {
		return nil, err
	}
	if userAgent != nil {
		out.UserAgent = *userAgent
	}
	out.IP = ip
	return &out, nil
}

// nullableUserAgent maps "" to SQL NULL: an empty string and "no header" are the
// same fact, and NULL is the honest way to store it.
func nullableUserAgent(ua string) *string {
	if ua == "" {
		return nil
	}
	return &ua
}
