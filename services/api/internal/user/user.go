// Package user holds the account domain and its persistence.
//
// WHY this is its own package instead of living inside internal/httpapi: the
// same accounts are read by the HTTP handlers, by the session middleware, by the
// adminctl break-glass CLI and — from Phase 2 — by the admin user-management API.
// Keeping the type, the validation rules and the SQL in one place is what makes
// "students never have a password" and "account lookups are case-insensitive"
// properties of the system rather than of one call site.
//
// Two rules matter more than the code below:
//
//   - The database is the last line of defence (§9). Every invariant stated here
//     is also a CHECK constraint in migrations/0002_users.sql, because
//     application code can be bypassed by a script or a manual UPDATE and a
//     constraint cannot.
//   - A User carries password_hash because the login path needs it. Nothing in
//     this package ever logs it, and the HTTP layer never serialises it.
package user

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is one of the three kinds of account in the system (§2.1).
//
// The three roles are not a hierarchy: a teacher is not a "senior student" and an
// admin does not inherit teacher rights. Each entry point and each route group is
// bound to exactly one role, so authorization is an equality check, never a
// ranking (§37).
type Role string

const (
	RoleAdmin   Role = "ADMIN"
	RoleTeacher Role = "TEACHER"
	RoleStudent Role = "STUDENT"
)

// Roles lists every valid role, in the order the system documents them.
var Roles = []Role{RoleAdmin, RoleTeacher, RoleStudent}

// Valid reports whether r is one of the three known roles.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleTeacher, RoleStudent:
		return true
	default:
		return false
	}
}

// Lower returns the role as a lowercase token, used for cookie names and
// rate-limit keys.
func (r Role) Lower() string { return strings.ToLower(string(r)) }

// ParseRole converts an operator- or request-supplied string into a Role.
//
// The input is normalised to upper case so `--role teacher` on the command line
// works, but an unknown value is an error: silently defaulting to STUDENT would
// create an account with fewer privileges than intended, and defaulting to ADMIN
// would be a privilege escalation.
func ParseRole(raw string) (Role, error) {
	role := Role(strings.ToUpper(strings.TrimSpace(raw)))
	if !role.Valid() {
		return "", fmt.Errorf("unknown role %q: must be one of ADMIN, TEACHER, STUDENT", raw)
	}
	return role, nil
}

// Status is the account lifecycle state (§9).
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusDisabled:
		return true
	default:
		return false
	}
}

// User is one account row.
//
// It intentionally contains PasswordHash: the login path must be able to verify
// it. The rule that keeps it out of responses is enforced by the HTTP layer
// building a DTO with no such field, not by hiding the column from the domain —
// a domain type that lies about the database is harder to reason about than one
// that carries the value and is never asked to serialise it.
type User struct {
	ID          uuid.UUID
	Account     string
	DisplayName string
	Role        Role
	Status      Status
	// PasswordHash is the Argon2id PHC string, nil for students.
	PasswordHash *string
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// HasPassword reports whether this account can authenticate with a password.
func (u *User) HasPassword() bool {
	return u != nil && u.PasswordHash != nil && *u.PasswordHash != ""
}

// Business errors of this package. They are sentinels so callers can use
// errors.Is, and their text never contains SQL, a table name or a driver
// message: those belong in the wrapped cause.
var (
	// ErrNotFound means no row matched. Callers decide what that implies:
	// the login flow turns it into invalid credentials, adminctl prints it.
	ErrNotFound = errors.New("user: account not found")

	// ErrAccountTaken means the unique constraint on account rejected an insert.
	// The original driver error is wrapped for logs but never surfaced to a
	// client — a raw "duplicate key value violates unique constraint
	// users_account_key" is both unhelpful and an information leak.
	ErrAccountTaken = errors.New("user: account already exists")

	// ErrLastAdmin means the requested transition would leave the deployment with
	// no active ADMIN, i.e. nobody able to manage accounts any more. It is
	// returned by SetStatus, which enforces the rule inside the statement: a
	// read-then-write check in the service alone would let two concurrent
	// "disable the other admin" requests both pass.
	ErrLastAdmin = errors.New("user: refusing to disable the last active admin")
)

// accountPattern mirrors the users_account_format CHECK constraint exactly.
//
// WHY it is duplicated in code rather than only in the database: the constraint
// produces a driver error, and a CLI or an API should answer "the account may
// only contain A-Z a-z 0-9 . _ - and must be 3-64 characters long" instead of
// forwarding a SQLSTATE. The database stays the authority; this is the readable
// front door to it.
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,64}$`)

// MaxDisplayNameLength bounds a display name. It is a sanity limit, not a
// security boundary: the value is rendered in the teacher console, and an
// unbounded string would let one account blow up every monitoring page.
const MaxDisplayNameLength = 128

// ValidateAccount checks an account against the database's format constraint.
func ValidateAccount(account string) error {
	if !accountPattern.MatchString(account) {
		return fmt.Errorf("account %q is invalid: use 3-64 characters from A-Z a-z 0-9 . _ -", account)
	}
	return nil
}

// ValidateDisplayName rejects names that would render as an empty row.
//
// The value is stored trimmed: a trailing space is invisible in the UI and would
// make two "different" names collide in reports.
func ValidateDisplayName(displayName string) (string, error) {
	trimmed := strings.TrimSpace(displayName)
	if trimmed == "" {
		return "", errors.New("display name must not be empty")
	}
	if len([]rune(trimmed)) > MaxDisplayNameLength {
		return "", fmt.Errorf("display name is too long: at most %d characters", MaxDisplayNameLength)
	}
	return trimmed, nil
}

// CreateParams is the input of Repository.Create.
type CreateParams struct {
	Account     string
	DisplayName string
	Role        Role
	// PasswordHash must be non-nil for ADMIN/TEACHER and nil for STUDENT; the
	// database enforces it, and Repository.Create passes it through unchanged.
	PasswordHash *string
	CreatedBy    *uuid.UUID
}

// ListFilter is the query of the admin account list (§68).
//
// Nil pointers mean "no filter on this field"; there is deliberately no "empty
// string means no filter" convention, because `q=""` from a frontend search box
// and "the client sent no q at all" must not be able to diverge in behaviour.
type ListFilter struct {
	// Role, when set, keeps only that role. ADMIN is included: an admin must be
	// able to see the other admins in order to answer "who else can do this?".
	Role *Role
	// Status, when set, keeps only ACTIVE or DISABLED accounts.
	Status *Status
	// Query, when non-empty, is matched case-insensitively against the account
	// AND the display name. Both are included because an admin looking for a
	// person may know either one — and the account is the only identifier that is
	// guaranteed unique.
	Query string
	// Limit is the page size. Must be > 0: a repository that silently defaults to
	// "everything" turns a paging bug into a full-table load.
	Limit int
	// Offset skips that many rows of the filtered, stably ordered result.
	Offset int
}

// ListResult is one page of accounts plus the total number of rows the filter
// matched.
//
// WHY Total is part of the same call: the admin list renders "1-50 of 123". A
// separate COUNT endpoint would let the two numbers come from different moments,
// which shows up as a page counter that never settles.
type ListResult struct {
	Users []User
	Total int
}

// Repository is the persistence contract for accounts.
//
// It is an interface so the auth service and the HTTP middleware can be unit
// tested against fakes: an authorization test that needs PostgreSQL to run is a
// test nobody runs.
type Repository interface {
	// FindByAccount looks an account up case-insensitively (citext).
	FindByAccount(ctx context.Context, account string) (*User, error)
	// FindByID looks an account up by primary key.
	FindByID(ctx context.Context, id uuid.UUID) (*User, error)
	// Create inserts an account and returns the stored row. A duplicate account
	// (in any letter case — the column is citext) surfaces as ErrAccountTaken.
	Create(ctx context.Context, params CreateParams) (*User, error)
	// TouchLastLogin records a successful login. Best effort by contract: a
	// failure here must never fail the login itself.
	TouchLastLogin(ctx context.Context, id uuid.UUID) error
	// SetPasswordHash replaces the password hash (password reset, or the
	// transparent rehash after a successful login with stale parameters).
	SetPasswordHash(ctx context.Context, id uuid.UUID, hash string) error
	// List returns accounts, optionally filtered by role, newest first.
	List(ctx context.Context, role *Role) ([]User, error)
	// ListPage returns one page of accounts plus the filtered total, ordered by
	// created_at DESC, id DESC. The order is stable, which is what makes paging
	// through the admin list unable to repeat or skip a row.
	ListPage(ctx context.Context, filter ListFilter) (*ListResult, error)
	// UpdateDisplayName rewrites the display name and bumps updated_at.
	UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error
	// SetStatus switches an account between ACTIVE and DISABLED.
	//
	// The ACTIVE→DISABLED transition refuses to remove the last active ADMIN and
	// returns ErrLastAdmin: that check is repeated inside the statement so two
	// concurrent "disable the other admin" requests cannot both pass a
	// read-then-write check and leave the deployment with no administrator.
	SetStatus(ctx context.Context, id uuid.UUID, status Status) error
	// CountActiveAdmins reports how many ADMIN accounts can still log in.
	CountActiveAdmins(ctx context.Context) (int, error)
}
