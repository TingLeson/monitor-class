package session

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

// sessionColumnNames is the single ordered projection every session read and write
// uses.
//
// WHY a shared list: Scan and SELECT drifting apart writes values into the wrong
// fields and fails silently whenever two columns share a type (a timestamp into the
// wrong timestamp). It also makes every write path return the same row shape, so the
// service can replace a stale in-memory session with the stored one without caring
// which statement produced it.
//
// WHY it is rendered with a table prefix rather than written out twice: the monitoring
// read JOINs `users`, which has its own id and status columns, so that query needs
// `s.id` while `INSERT ... RETURNING` must not be qualified. One list, two renderings —
// a column added here cannot be added to only one of them.
var sessionColumnNames = []string{
	"id", "classroom_run_id", "student_id", "livekit_identity", "status",
	"connected_at", "screen_started_at", "screen_lost_at", "left_at", "created_at", "updated_at",
}

// sessionColumns renders the projection, optionally qualified with a table alias
// ("s." for joined reads, "" everywhere else).
func sessionColumns(prefix string) string {
	qualified := make([]string, 0, len(sessionColumnNames))
	for _, name := range sessionColumnNames {
		qualified = append(qualified, prefix+name)
	}
	return "\n\t" + strings.Join(qualified, ", ")
}

// activeRunStudentIndex is the partial unique index that enforces §50: one active
// session per (run, student). It is named here because the join path uses it as an
// ON CONFLICT arbiter, and an arbiter that silently stops matching is a uniqueness
// rule that silently stops applying.
const activeRunStudentIndex = `
	(classroom_run_id, student_id)
	WHERE status IN ('CONNECTING', 'ONLINE', 'SCREEN_LOST', 'DISCONNECTED')`

// Postgres is the pgx implementation of Repository.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres wraps a pool. The pool is shared with the rest of the process; this
// type never closes it.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// CreateOrReuse returns the student's active session for a run, creating one when
// there is none (§43/§50).
//
// # Why one statement and not "SELECT, then INSERT or UPDATE"
//
// Two concurrent joins from the same browser (a double click, a page that retried
// after a timeout) would both read "no active session" and both insert; the partial
// unique index would then reject one of them with a driver error. `INSERT ... ON
// CONFLICT DO UPDATE` makes the second one reuse the first one's row instead, so the
// wall cannot grow a second tile of the same student and the endpoint cannot fail for
// a reason the student cannot act on.
//
// # What "reuse" means, and what it must not touch
//
// The reused row keeps its id — and therefore its livekit_identity, which is what
// makes LiveKit reject the stale connection and let the new one in ("same identity
// rejoins" kicks the old session, §50) without any kick logic of our own.
//
// Only status, left_at and updated_at move: the timestamps describe firsts
// (connected_at, screen_started_at) that stay true across a reconnect, and erasing
// them would lose "when did this student first get their screen up?".
//
// A session that is already LEFT is NOT reused: leaving is a decision, and the record
// of it must survive. The partial index excludes terminal states, so a re-entry in the
// same lesson creates a new row — the wall then shows the earlier LEFT tile and the
// new live one, which is the true sequence of the lesson.
func (p *Postgres) CreateOrReuse(ctx context.Context, params CreateOrReuseParams) (*StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	// $1 is passed as text and cast where a uuid is needed, so the identity and the id
	// are provably the same value in one statement (student_sessions_identity_is_id
	// checks it again in the database).
	query := `
		INSERT INTO student_sessions (id, classroom_run_id, student_id, livekit_identity, status)
		VALUES ($1::uuid, $2, $3, $1, 'CONNECTING')
		ON CONFLICT ` + activeRunStudentIndex + `
		DO UPDATE SET status = 'CONNECTING', left_at = NULL, updated_at = now()
		RETURNING` + sessionColumns("")

	stored, err := scanSession(p.pool.QueryRow(ctx, query,
		params.SessionID.String(), params.ClassroomRunID, params.StudentID))
	if err != nil {
		return nil, translateWriteError(err)
	}
	return stored, nil
}

// Leave marks one student's own session LEFT.
//
// The WHERE clause carries the ownership rule: a session id that belongs to somebody
// else matches no row, and the caller cannot tell that apart from an id that does not
// exist (§58). left_at is COALESCEd so a repeated leave keeps the FIRST timestamp —
// the row records when the student actually left, not when the request was last sent.
//
// The update is not guarded by the current status: a leave is valid from CONNECTING,
// ONLINE, SCREEN_LOST and DISCONNECTED alike, and a session that is already LEFT is
// returned unchanged (204 on a retried request, not an error).
func (p *Postgres) Leave(ctx context.Context, sessionID, studentID uuid.UUID) (*StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	query := `
		UPDATE student_sessions
		   SET status = 'LEFT', left_at = COALESCE(left_at, now()), updated_at = now()
		 WHERE id = $1 AND student_id = $2
		RETURNING` + sessionColumns("")

	stored, err := scanSession(p.pool.QueryRow(ctx, query, sessionID, studentID))
	if errors.Is(err, pgx.ErrNoRows) {
		// Not mine, or not there: one answer for both, deliberately (see ErrSessionNotFound).
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, translateWriteError(err)
	}
	return stored, nil
}

// ListByRun returns every session of one run, oldest first.
//
// JOIN users, not LEFT JOIN: student_id is NOT NULL and FK-enforced, so the account
// always exists and an INNER JOIN states that. A broken row then shows up as a missing
// tile (loud) rather than as a tile with a blank name (quiet, and mistaken for a
// frontend bug).
//
// The order is `created_at, id`. created_at is the transaction timestamp, so a class
// that joins in the same second shares it — the id tiebreaker is what keeps the wall
// from reshuffling between two refreshes of the same page, which teachers read as
// "the tiles are jumping around".
//
// Terminal sessions are included: §51 shows that a student left. Hiding them would
// make a student who left look like a student who never existed.
func (p *Postgres) ListByRun(ctx context.Context, runID uuid.UUID) ([]StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	query := `SELECT` + sessionColumns("s.") + `, u.display_name
		FROM student_sessions s
		JOIN users u ON u.id = s.student_id
		WHERE s.classroom_run_id = $1
		ORDER BY s.created_at ASC, s.id ASC`

	rows, err := p.pool.Query(ctx, query, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil empty slice: a run nobody joined answers `"students": []`, not null, so
	// the wall renders its empty state without a null check.
	out := make([]StudentSession, 0, 32)
	for rows.Next() {
		var (
			session     StudentSession
			status      string
			connectedAt *time.Time
			screenStart *time.Time
			screenLost  *time.Time
			leftAt      *time.Time
		)
		if err := rows.Scan(
			&session.ID, &session.ClassroomRunID, &session.StudentID, &session.LiveKitIdentity, &status,
			&connectedAt, &screenStart, &screenLost, &leftAt, &session.CreatedAt, &session.UpdatedAt,
			&session.StudentDisplayName,
		); err != nil {
			return nil, err
		}
		session.Status = Status(status)
		session.ConnectedAt = connectedAt
		session.ScreenStartedAt = screenStart
		session.ScreenLostAt = screenLost
		session.LeftAt = leftAt
		out = append(out, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyObservation persists one decided transition.
//
// `AND status = $6` is the compare-and-set: the transition was computed from the row
// this caller read a moment ago, and if that row has moved since — a student pressed
// leave, another teacher's poll advanced it — this update must do nothing. Returning
// nil (no rows) rather than an error says "your observation is stale", which is a
// normal outcome of polling and not a failure.
//
// The timestamps come from the DATABASE clock, not from the API process: a lesson's
// record must not be ordered by a host whose clock drifted, and the same reason is why
// every other write in this schema timestamps with now().
func (p *Postgres) ApplyObservation(ctx context.Context, change ObservationChange) (*StudentSession, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("session: repository is not connected")
	}
	query := `
		UPDATE student_sessions
		   SET status = $3,
		       connected_at      = CASE WHEN $4 THEN COALESCE(connected_at, now()) ELSE connected_at END,
		       screen_started_at = CASE WHEN $5 THEN COALESCE(screen_started_at, now()) ELSE screen_started_at END,
		       screen_lost_at    = CASE WHEN $6 THEN now() ELSE screen_lost_at END,
		       updated_at        = now()
		 WHERE id = $1 AND status = $2
		RETURNING` + sessionColumns("")

	stored, err := scanSession(p.pool.QueryRow(ctx, query,
		change.SessionID, string(change.From), string(change.To),
		change.MarkConnected, change.MarkScreenStarted, change.MarkScreenLost,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, translateWriteError(err)
	}
	return stored, nil
}

// scanSession reads one row of sessionColumns.
func scanSession(row pgx.Row) (*StudentSession, error) {
	var (
		session     StudentSession
		status      string
		connectedAt *time.Time
		screenStart *time.Time
		screenLost  *time.Time
		leftAt      *time.Time
	)
	if err := row.Scan(
		&session.ID, &session.ClassroomRunID, &session.StudentID, &session.LiveKitIdentity, &status,
		&connectedAt, &screenStart, &screenLost, &leftAt, &session.CreatedAt, &session.UpdatedAt,
	); err != nil {
		return nil, err
	}
	session.Status = Status(status)
	session.ConnectedAt = connectedAt
	session.ScreenStartedAt = screenStart
	session.ScreenLostAt = screenLost
	session.LeftAt = leftAt
	return &session, nil
}

// translateWriteError turns constraint violations into business errors or into
// readable internal errors, mirroring internal/classroom's translation.
//
// The driver's message (SQLSTATE, relation, constraint) must never reach a client
// (§58), but it must remain reachable for the log — hence the wrapping.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case sqlstateUniqueViolation:
		return fmt.Errorf("session: a uniqueness constraint rejected the write (constraint %s): %w", pgErr.ConstraintName, pgErr)
	case sqlstateCheckViolation:
		return fmt.Errorf("session: a check constraint rejected the write (constraint %s): %w", pgErr.ConstraintName, pgErr)
	case sqlstateForeignKeyViolation:
		return fmt.Errorf("session: referenced row does not exist (constraint %s): %w", pgErr.ConstraintName, pgErr)
	default:
		return err
	}
}

// PostgreSQL SQLSTATE codes, spelled out for the same reason internal/classroom
// spells them out: they are part of the SQL standard and will not change.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)
