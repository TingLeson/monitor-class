package auth

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/user"
)

// ErrSessionNotFound means no usable session matches the presented token.
//
// "Usable" is deliberately narrow: a revoked or expired row does not match either,
// so a token that has been logged out can never authenticate again even if the
// caller still holds the cookie.
var ErrSessionNotFound = errors.New("auth: session not found")

// Session is one row of `sessions`, minus the raw token (which is never stored).
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	CSRFToken  string
	IssuedAt   time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastSeenAt time.Time
	UserAgent  string
	IP         *netip.Addr
}

// SessionWithUser is a session joined with the current state of its account.
//
// WHY the join is part of the store contract and not a second lookup: role and
// status must come from the database on EVERY request, not be cached in the
// session row or the cookie. A teacher demoted to student, or an account disabled
// in the middle of a class, must take effect on the next request — that is the
// whole reason sessions are server-side rows (§37/§41). Reading both in one
// statement also removes a race where the account changes between two queries.
type SessionWithUser struct {
	Session
	Account         string
	DisplayName     string
	Role            user.Role
	UserStatus      user.Status
	UserCreatedAt   time.Time
	UserLastLoginAt *time.Time
}

// CreateSessionParams is the input of SessionStore.Create.
//
// The caller supplies ID, IssuedAt and ExpiresAt rather than letting the store
// invent them, so the service can log the session id and the exact expiry it
// promised the client even if the INSERT fails.
type CreateSessionParams struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	CSRFToken string
	IssuedAt  time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        *netip.Addr
}

// SessionStore is the persistence contract of the session lifecycle.
//
// Task book §34 requires this abstraction explicitly, and it earns its keep three
// times over: the auth service is unit-testable without PostgreSQL, a future
// Redis-backed store is a drop-in for read-heavy deployments, and "session data
// lives in PostgreSQL" stays a property of the implementation rather than an
// assumption baked into the service.
type SessionStore interface {
	// Create inserts a new session and returns the stored row.
	Create(ctx context.Context, params CreateSessionParams) (*Session, error)
	// FindByTokenHash resolves a presented token to its session and the CURRENT
	// account state. Revoked and expired sessions are not returned; a session
	// whose account is DISABLED is returned (with UserStatus=DISABLED) so the
	// caller can revoke it and answer ACCOUNT_DISABLED rather than a generic 401.
	FindByTokenHash(ctx context.Context, hash []byte) (*SessionWithUser, error)
	// Revoke marks one session as revoked. It is idempotent.
	Revoke(ctx context.Context, id uuid.UUID) error
	// RevokeAllForUser revokes every live session of one account: password reset,
	// account disabled, "log out everywhere".
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
	// Touch records activity for the idle-visibility column.
	Touch(ctx context.Context, id uuid.UUID, now time.Time) error
	// DeleteExpired removes this user's expired rows. Phase 1 has no background
	// job, so it runs opportunistically at login.
	DeleteExpired(ctx context.Context, userID uuid.UUID) error
}
