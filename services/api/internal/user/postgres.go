package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL SQLSTATE codes this package translates into business errors. They
// are spelled out rather than imported from a helper module because they are part
// of the SQL standard and will not change; a new dependency for five strings
// would not pay for itself.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)

// userColumns is the single column list every query in this file uses.
//
// WHY a shared constant: Scan and SELECT drifting apart is the classic way a
// repository starts writing values into the wrong fields, and it fails silently
// when two columns happen to share a type.
const userColumns = `id, account, display_name, role, password_hash, status,
	created_by, created_at, updated_at, last_login_at`

// Postgres is the pgx implementation of Repository.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres wraps a pool. The pool is shared with the rest of the process; this
// type never closes it.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// FindByAccount looks up one account, case-insensitively.
//
// The `::citext` cast is not decoration. pgx declares a Go string parameter as
// `text`, and PostgreSQL resolves `citext = text` to the *text* equality operator
// (the only one reachable through implicit casts), which is case-SENSITIVE. The
// explicit cast forces citext semantics, so `s10086` finds `S10086` — the whole
// point of the column type. Without it, a student typing their account in the
// wrong case is told "no such account", which is the most common field failure
// this design exists to prevent (§9).
func (p *Postgres) FindByAccount(ctx context.Context, account string) (*User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE account = $1::citext`
	return p.queryOne(ctx, query, account)
}

// FindByID looks up one account by primary key.
func (p *Postgres) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return p.queryOne(ctx, query, id)
}

func (p *Postgres) queryOne(ctx context.Context, query string, args ...any) (*User, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("user: repository is not connected")
	}
	row := p.pool.QueryRow(ctx, query, args...)
	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The driver error is dropped on purpose: "no rows in result set" is
			// not a business fact, and pgx.ErrNoRows escaping into HTTP would
			// bypass the error envelope.
			return nil, ErrNotFound
		}
		return nil, err
	}
	return u, nil
}

// Create inserts an account.
//
// Validation of the account format and the display name is the caller's job (see
// ValidateAccount / ValidateDisplayName) so it can answer with a readable
// message; the database re-checks both, and its verdict wins.
func (p *Postgres) Create(ctx context.Context, params CreateParams) (*User, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("user: repository is not connected")
	}
	const query = `
		INSERT INTO users (account, display_name, role, password_hash, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + userColumns

	row := p.pool.QueryRow(ctx, query,
		params.Account, params.DisplayName, string(params.Role), params.PasswordHash, params.CreatedBy,
	)
	u, err := scanUser(row)
	if err != nil {
		return nil, translateWriteError(err)
	}
	return u, nil
}

// TouchLastLogin records a successful login.
//
// updated_at is bumped as well: it answers "when was this row last written?" and
// a login is a write.
func (p *Postgres) TouchLastLogin(ctx context.Context, id uuid.UUID) error {
	if p == nil || p.pool == nil {
		return errors.New("user: repository is not connected")
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE users SET last_login_at = now(), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPasswordHash replaces the stored Argon2id string.
func (p *Postgres) SetPasswordHash(ctx context.Context, id uuid.UUID, hash string) error {
	if p == nil || p.pool == nil {
		return errors.New("user: repository is not connected")
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
	if err != nil {
		return translateWriteError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns accounts, newest first, optionally filtered by role.
//
// There is no pagination in Phase 1: the caller is the adminctl CLI, and a school
// has hundreds of accounts, not millions. Phase 2 (admin HTTP list) must add
// pagination before this becomes a public endpoint.
func (p *Postgres) List(ctx context.Context, role *Role) ([]User, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("user: repository is not connected")
	}
	const query = `
		SELECT ` + userColumns + `
		  FROM users
		 WHERE ($1::text IS NULL OR role = $1)
		 ORDER BY created_at DESC, account ASC`

	rows, err := p.pool.Query(ctx, query, roleFilterValue(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// roleFilterValue converts the optional filter into something pgx can send as
// text; a typed nil pointer would be ambiguous for the server.
func roleFilterValue(role *Role) any {
	if role == nil {
		return nil
	}
	return string(*role)
}

// scanUser reads one row of userColumns.
func scanUser(row pgx.Row) (*User, error) {
	var (
		u        User
		role     string
		status   string
		hash     *string
		created  time.Time
		updated  time.Time
		lastSeen *time.Time
	)
	if err := row.Scan(
		&u.ID, &u.Account, &u.DisplayName, &role, &hash, &status,
		&u.CreatedBy, &created, &updated, &lastSeen,
	); err != nil {
		return nil, err
	}
	u.Role = Role(role)
	u.Status = Status(status)
	u.PasswordHash = hash
	u.CreatedAt = created
	u.UpdatedAt = updated
	u.LastLoginAt = lastSeen
	return &u, nil
}

// translateWriteError turns constraint violations into business errors.
//
// WHY translate at all: a `*pgconn.PgError` stringifies to SQLSTATE, relation and
// constraint names. That is exactly the kind of internal detail that must not
// reach a client (§58) and must not appear in a message a human pastes into a
// ticket. The translated error keeps a readable sentence and still carries the
// driver error for `errors.Is`/`errors.As` and for the server-side log.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case sqlstateUniqueViolation:
		// The only unique constraint on users is the account, so this is an
		// unambiguous "account taken" — including when the difference is only
		// letter case, which is the point of citext.
		return &constraintError{
			message: fmt.Sprintf("user: account already exists (constraint %s)", pgErr.ConstraintName),
			causes:  []error{ErrAccountTaken, pgErr},
		}
	case sqlstateCheckViolation:
		return &constraintError{
			message: fmt.Sprintf("user: %s: %s", pgErr.ConstraintName, checkConstraintHint(pgErr.ConstraintName)),
			causes:  []error{pgErr},
		}
	case sqlstateForeignKeyViolation:
		return &constraintError{
			message: fmt.Sprintf("user: referenced row does not exist (constraint %s)", pgErr.ConstraintName),
			causes:  []error{pgErr},
		}
	default:
		return err
	}
}

// constraintError is a business error that explains which rule was violated in
// prose while keeping the driver error reachable for logs and for errors.Is.
//
// WHY the driver error is not simply appended to the message: the message is what
// a CLI prints and what a ticket ends up containing, and "SQLSTATE 23514" in it
// tells a human nothing they can act on. The cause travels through Unwrap, where
// only the structured log sees it.
type constraintError struct {
	message string
	causes  []error
}

func (e *constraintError) Error() string { return e.message }

// Unwrap returns every cause, so both errors.Is(err, ErrAccountTaken) and
// errors.As(err, &pgErr) work on the same value.
func (e *constraintError) Unwrap() []error { return e.causes }

// checkConstraintHint explains the constraint that rejected the write, in the
// terms of the business rule rather than of the schema.
func checkConstraintHint(constraint string) string {
	switch constraint {
	case "users_password_by_role":
		return "students must not have a password, teachers and admins must have one"
	case "users_account_format":
		return "account must be 3-64 characters from A-Z a-z 0-9 . _ -"
	case "users_display_name_not_blank":
		return "display name must not be blank"
	case "users_role_valid":
		return "role must be ADMIN, TEACHER or STUDENT"
	case "users_status_valid":
		return "status must be ACTIVE or DISABLED"
	default:
		return "a database constraint rejected the value"
	}
}
