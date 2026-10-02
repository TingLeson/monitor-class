package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// statusFilterValue is roleFilterValue for the status filter.
func statusFilterValue(status *Status) any {
	if status == nil {
		return nil
	}
	return string(*status)
}

// listFilterClause builds the WHERE clause shared by the COUNT and the page query.
//
// WHY one builder for both: the total and the page must be computed from exactly
// the same predicate. Two hand-maintained copies of a five-condition filter is
// how "the list shows 12 rows but the counter says 11" bugs are born.
//
// The search term is bound as a parameter — never concatenated into the statement
// (§63) — and matched with ILIKE because it is a substring filter, not an
// equality test; the wildcards are added to the parameter, so a `%` typed by the
// admin is matched literally as part of their input rather than being interpreted
// as "match everything".
func listFilterClause(filter ListFilter) (string, []any) {
	clause := `
		 WHERE ($1::text IS NULL OR role = $1)
		   AND ($2::text IS NULL OR status = $2)
		   AND ($3::text = '' OR account ILIKE $4 OR display_name ILIKE $4)`
	pattern := "%" + strings.TrimSpace(filter.Query) + "%"
	return clause, []any{
		roleFilterValue(filter.Role),
		statusFilterValue(filter.Status),
		strings.TrimSpace(filter.Query),
		pattern,
	}
}

// ListPage returns one page of accounts plus the total the filter matched.
//
// The ORDER BY is `created_at DESC, id DESC` and the second key is not
// decoration. created_at defaults to now(), which in PostgreSQL is the
// TRANSACTION timestamp: every account created by one seeding transaction (or by
// two requests that commit in the same microsecond) shares a timestamp exactly.
// Ordering by created_at alone leaves those ties in an arbitrary, plan-dependent
// order that can differ between the COUNT query, page 1 and page 2 — so an admin
// paging through the list sees a row twice and never sees another. The primary
// key breaks every tie deterministically.
func (p *Postgres) ListPage(ctx context.Context, filter ListFilter) (*ListResult, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("user: repository is not connected")
	}
	if filter.Limit <= 0 {
		// Refusing beats defaulting: a caller that forgot the page size would
		// otherwise get the whole table, and the mistake would only show up as
		// latency on a database nobody is watching.
		return nil, errors.New("user: ListPage requires a positive limit")
	}
	if filter.Offset < 0 {
		return nil, errors.New("user: ListPage requires a non-negative offset")
	}

	clause, args := listFilterClause(filter)

	// Two statements, one predicate: the count answers "how many pages are
	// there?", the page answers "what is on this one?". They are deliberately not
	// wrapped in a transaction: a row created between them changes the total by
	// one, which is a cosmetic rounding difference, while holding a transaction
	// open across two full scans would be a real cost on every admin page load.
	total := 0
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM users`+clause, args...).Scan(&total); err != nil {
		return nil, err
	}

	pageArgs := append(append([]any{}, args...), filter.Limit, filter.Offset)
	query := `SELECT ` + userColumns + ` FROM users` + clause +
		` ORDER BY created_at DESC, id DESC LIMIT $5 OFFSET $6`

	rows, err := p.pool.Query(ctx, query, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil empty slice: an admin looking at an empty filter result must get
	// `"users": []`, not `"users": null`, or every frontend needs a null check
	// before it can call .map().
	out := make([]User, 0, min(filter.Limit, 64))
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
	return &ListResult{Users: out, Total: total}, nil
}

// UpdateDisplayName rewrites the display name.
//
// The name is validated by the caller (ValidateDisplayName) so the operator gets
// a readable message; the users_display_name_not_blank constraint remains the
// last line of defence. updated_at is bumped explicitly because the table has no
// trigger by design (see migrations/0002), and a display-name change that leaves
// updated_at untouched would make the admin list's "last modified" column lie.
func (p *Postgres) UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error {
	if p == nil || p.pool == nil {
		return errors.New("user: repository is not connected")
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE users SET display_name = $2, updated_at = now() WHERE id = $1`, id, displayName)
	if err != nil {
		return translateWriteError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus switches an account between ACTIVE and DISABLED.
//
// Setting the status the row already has is a successful write (idempotent): the
// caller's intent — "this account must be ACTIVE/DISABLED" — is already true, so
// answering ErrNotFound or a conflict would only make the admin UI special-case a
// no-op.
//
// The third condition is the important one. It refuses the ACTIVE→DISABLED
// transition of the LAST active admin *inside the statement*, where the check and
// the write are one atomic operation:
//
//	WHERE id = $1
//	  AND (status <> 'DISABLED' OR ...)          -- only guard the real transition
//	  AND (role <> 'ADMIN' OR EXISTS (           -- an active admin must remain
//	        SELECT 1 FROM users
//	         WHERE role = 'ADMIN' AND status = 'ACTIVE' AND id <> $1))
//
// WHY in SQL and not only in the service: two admins clicking "disable" on each
// other at the same moment both pass a read-then-write check in Go (each sees the
// other as active), and both updates then succeed — leaving a deployment with no
// administrator at all and no way back in except the adminctl break-glass CLI.
// PostgreSQL evaluates this condition under the row locks it takes for the
// UPDATE, so the second statement sees the first one's effect and matches no row.
// The service performs the same check first purely to produce a readable message
// on the common, non-racing path.
func (p *Postgres) SetStatus(ctx context.Context, id uuid.UUID, status Status) error {
	if p == nil || p.pool == nil {
		return errors.New("user: repository is not connected")
	}
	// The decision is computed ONCE, in a CTE, and the write then executes it.
	//
	// WHY a CTE instead of conditions inline in SET: the "may this account be
	// disabled?" answer is needed twice — once to decide the new status, once to
	// decide whether updated_at is touched — and duplicating that predicate is how
	// the two halves of one rule drift apart. Reading it from the row's OLD values
	// (which is what a CTE attached to the same statement sees) is also what makes
	// a DISABLED→DISABLED write distinguishable from an ACTIVE→DISABLED one, which
	// the WHERE clause cannot do: WHERE sees the new row version, where
	// `status <> 'DISABLED'` is true for both.
	//
	// The guard is evaluated by the statement itself, under the row locks the UPDATE
	// takes, so two administrators disabling each other concurrently cannot both
	// pass: the second statement re-reads the row, finds no other ACTIVE ADMIN and
	// refuses. That is the whole reason the rule is here and not only in the
	// service, where the classic read-then-write race would leave the deployment
	// with no administrator at all.
	const query = `
		WITH target AS (
		    SELECT id, status, role,
		           (role = 'ADMIN'
		            AND status = 'ACTIVE'
		            AND NOT EXISTS (SELECT 1 FROM users other
		                             WHERE other.role = 'ADMIN'
		                               AND other.status = 'ACTIVE'
		                               AND other.id <> users.id)) AS is_last_admin
		      FROM users
		     WHERE id = $1
		)
		UPDATE users u
		   SET status = CASE
		                  WHEN $2 = 'DISABLED' AND t.is_last_admin THEN t.status
		                  ELSE $2
		                END,
		       updated_at = CASE
		                      WHEN t.status IS DISTINCT FROM (
		                             CASE
		                               WHEN $2 = 'DISABLED' AND t.is_last_admin THEN t.status
		                               ELSE $2
		                             END)
		                      THEN now()
		                      ELSE u.updated_at
		                    END
		  FROM target t
		 WHERE u.id = t.id
		RETURNING u.status`

	var resulting string
	err := p.pool.QueryRow(ctx, query, id, string(status)).Scan(&resulting)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// With the guard expressed as a value and not as a filter, the only way
			// to match no row is that the account does not exist.
			return ErrNotFound
		}
		return translateWriteError(err)
	}
	// The row exists but still carries its previous status: the guard refused the
	// ACTIVE→DISABLED transition of the last administrator. Reporting the rule
	// rather than the row is what keeps a concurrent double-disable from looking
	// like a success.
	if Status(resulting) != status && status == StatusDisabled {
		return ErrLastAdmin
	}
	return nil
}

// CountActiveAdmins counts the ADMIN accounts that can still log in.
//
// It is the read side of the "never disable the last administrator" rule: the
// service asks before writing so the admin gets "this is the last active
// administrator" instead of a rejection it cannot explain.
func (p *Postgres) CountActiveAdmins(ctx context.Context) (int, error) {
	if p == nil || p.pool == nil {
		return 0, errors.New("user: repository is not connected")
	}
	var count int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = 'ADMIN' AND status = 'ACTIVE'`).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
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
