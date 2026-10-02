package classroom

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/user"
)

// PostgreSQL SQLSTATE codes this package translates into business errors. They
// are spelled out (not imported from a helper) for the same reason internal/user
// spells them out: they are part of the SQL standard and will not change.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
	// PostgreSQL reports ON DELETE RESTRICT as 23001, a different code from the
	// 23503 it uses for NO ACTION and for a missing referenced row on INSERT. It is
	// listed because this package is not the only writer of this schema: a future
	// delete endpoint would surface here, and a raw driver error must never become
	// the client's answer (§58).
	sqlstateRestrictViolation = "23001"
)

// Constraint names that carry a business meaning. Matching on the name (and not
// only on the SQLSTATE) is what keeps a genuine bug — a UUID collision on
// livekit_room_name, say — from being reported as a normal "already open".
const (
	constraintOneOpenRun     = "classroom_runs_one_open_idx"
	constraintOwnerIsTeacher = "classrooms_owner_is_teacher"
	constraintRunConsistency = "classrooms_run_consistency"
	constraintRunsClosedAt   = "classroom_runs_closed_at"
)

// classroomColumns is the single projection every classroom read uses.
//
// WHY a shared constant: Scan and SELECT drifting apart writes values into the
// wrong fields and fails silently whenever two columns share a type. The roster
// size and the current run are part of the same read because both are needed by
// every response that contains a classroom.
const classroomColumns = `
	c.id, c.name, c.description, c.owner_teacher_id, c.status, c.current_run_id,
	c.created_at, c.updated_at,
	(SELECT count(*) FROM classroom_students cs WHERE cs.classroom_id = c.id),
	r.id, r.opened_at`

// classroomSource joins the current run.
//
// A LEFT JOIN is correct because a CLOSED classroom has no run; the database
// guarantees the inverse too (classrooms_run_consistency), so an OPEN classroom
// always finds its run here. The run is joined and not looked up per row, which is
// what keeps the list a single query.
const classroomSource = `
	FROM classrooms c
	LEFT JOIN classroom_runs r ON r.id = c.current_run_id`

// runColumns is the projection of classroom_runs.
const runColumns = `id, classroom_id, status, livekit_room_name, opened_at, closed_at`

// Postgres is the pgx implementation of Repository.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres wraps a pool. The pool is shared with the rest of the process; this
// type never closes it.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// querier is what the read helpers need: both *pgxpool.Pool and pgx.Tx satisfy it,
// so a read can run inside a transaction (where the row lock is held) or on its
// own without duplicating the SQL.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Create inserts a CLOSED classroom.
//
// The INSERT and the read-back share one transaction so the returned row is
// exactly what was stored (including the database-generated id and timestamps).
// A data-modifying CTE would NOT work here: within a single statement the outer
// SELECT sees the pre-statement snapshot, so the freshly inserted row would be
// invisible and the query would return nothing.
func (p *Postgres) Create(ctx context.Context, params CreateParams) (*Classroom, error) {
	return inTx(ctx, p.pool, func(tx pgx.Tx) (*Classroom, error) {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO classrooms (name, description, owner_teacher_id, status)
			VALUES ($1, $2, $3, 'CLOSED')
			RETURNING id`,
			params.Name, params.Description, params.OwnerTeacherID,
		).Scan(&id)
		if err != nil {
			return nil, translateWriteError(err)
		}
		return classroomByID(ctx, tx, id)
	})
}

// GetByID returns one classroom.
//
// It deliberately does NOT filter on the owner: the service has to tell "no such
// classroom" (404) from "not yours" (403), and a WHERE owner_teacher_id = $2 here
// would collapse the two into one indistinguishable empty result.
func (p *Postgres) GetByID(ctx context.Context, id uuid.UUID) (*Classroom, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("classroom: repository is not connected")
	}
	return classroomByID(ctx, p.pool, id)
}

// ListByOwner returns a teacher's classrooms, newest first.
//
// There is NO pagination, and that is a deliberate trade-off. A teacher owns a
// handful of courses (one per class they teach), not thousands; a page parameter
// would add a contract the frontend has to honour forever, plus a "which page am I
// on?" state to every list, to solve a problem that does not exist. The day a
// teacher can own enough classrooms for this to matter, the endpoint can grow
// paging additively (a default page size that returns everything today is
// forward-compatible; a required `?page=` is not).
//
// The ordering is `created_at DESC, id DESC` and the second key is not decoration:
// created_at defaults to now(), which is the TRANSACTION timestamp, so a seeding
// transaction or a rapid double-create gives two classrooms the same timestamp and
// their relative order becomes plan-dependent — a list that reshuffles on refresh.
func (p *Postgres) ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Classroom, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("classroom: repository is not connected")
	}
	query := `SELECT ` + classroomColumns + classroomSource +
		` WHERE c.owner_teacher_id = $1 ORDER BY c.created_at DESC, c.id DESC`

	rows, err := p.pool.Query(ctx, query, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil empty slice: a teacher with no classrooms must get `"classrooms": []`,
	// not null, or every frontend needs a null check before .map().
	out := make([]Classroom, 0, 8)
	for rows.Next() {
		c, err := scanClassroom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateDetails rewrites name and/or description.
//
// The two `CASE WHEN ... THEN ... ELSE <current value>` arms express "leave the
// other column alone" in the statement itself. The service knows which members the
// request carried; telling the database the same thing explicitly means a PATCH
// that only renames a classroom cannot blank its description, which is exactly the
// bug a naive SET description = $2 would produce.
func (p *Postgres) UpdateDetails(ctx context.Context, params UpdateParams) (*Classroom, error) {
	return inTx(ctx, p.pool, func(tx pgx.Tx) (*Classroom, error) {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE classrooms
			   SET name        = CASE WHEN $2::boolean THEN $3 ELSE name END,
			       description = CASE WHEN $4::boolean THEN $5 ELSE description END,
			       updated_at  = now()
			 WHERE id = $1
			RETURNING id`,
			params.ClassroomID, params.Name != nil, params.Name,
			params.DescriptionSet, params.Description,
		).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, translateWriteError(err)
		}
		return classroomByID(ctx, tx, id)
	})
}

// ListStudents returns the full roster, including DISABLED accounts.
//
// `added_at DESC, account ASC`: the newest addition is what a teacher who just
// pasted a list wants to see, and the account is the tiebreaker because added_at
// defaults to the transaction timestamp — a 30-student batch shares one timestamp
// exactly, and without the second key the roster would come back in a different
// order on every refresh. DISABLED students are NOT filtered out: they are still
// members of this course, and the DTO's status field is what tells the console to
// grey them out.
func (p *Postgres) ListStudents(ctx context.Context, classroomID uuid.UUID) ([]Student, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("classroom: repository is not connected")
	}
	const query = `
		SELECT u.id, u.account, u.display_name, u.status, cs.added_at
		  FROM classroom_students cs
		  JOIN users u ON u.id = cs.student_id
		 WHERE cs.classroom_id = $1
		 ORDER BY cs.added_at DESC, u.account ASC`

	rows, err := p.pool.Query(ctx, query, classroomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Student, 0, 32)
	for rows.Next() {
		var (
			s       Student
			status  string
			addedAt time.Time
		)
		if err := rows.Scan(&s.ID, &s.Account, &s.DisplayName, &status, &addedAt); err != nil {
			return nil, err
		}
		s.Status = user.Status(status)
		s.AddedAt = addedAt
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AddStudents grants access to a set of accounts in one statement.
//
// `ON CONFLICT (classroom_id, student_id) DO NOTHING` is what makes the operation
// idempotent: a student who is already on the roster is a success, not a conflict.
// The alternative — checking existence and then inserting — is a read-then-write
// race two concurrent imports could both win, and it would turn a retried request
// into a confusing failure for a teacher who did nothing wrong.
//
// The whole set is one statement: either every accepted student is granted or none
// is, so a partially applied import cannot leave the teacher unable to tell what
// happened. `added_by` records who granted access.
func (p *Postgres) AddStudents(ctx context.Context, classroomID uuid.UUID, studentIDs []uuid.UUID, addedBy uuid.UUID) error {
	if p == nil || p.pool == nil {
		return errors.New("classroom: repository is not connected")
	}
	const query = `
		INSERT INTO classroom_students (classroom_id, student_id, added_by)
		SELECT $1, student_id, $3 FROM unnest($2::uuid[]) AS student_id
		ON CONFLICT (classroom_id, student_id) DO NOTHING`

	if _, err := p.pool.Exec(ctx, query, classroomID, studentIDs, addedBy); err != nil {
		return translateWriteError(err)
	}
	return nil
}

// RemoveStudent deletes one grant.
//
// Removing a student who was never on the roster is ErrStudentNotAssigned (404),
// not a silent success: the caller asked to revoke a specific access and deserves
// to know that there was nothing to revoke — a typo in the student id would
// otherwise look like it worked.
func (p *Postgres) RemoveStudent(ctx context.Context, classroomID, studentID uuid.UUID) error {
	if p == nil || p.pool == nil {
		return errors.New("classroom: repository is not connected")
	}
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM classroom_students WHERE classroom_id = $1 AND student_id = $2`,
		classroomID, studentID)
	if err != nil {
		return translateWriteError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStudentNotAssigned
	}
	return nil
}

// Open creates a run and flips the classroom to OPEN in one transaction.
//
// The lock ordering and the checks are copied from §48 verbatim, and they have to
// stay inside this method: `SELECT ... FOR UPDATE` is only useful if the decision
// it protects is taken while the lock is held. Splitting "read the classroom" into
// the service and "write the run" here would leave a window in which a second
// request opens the same classroom, and the two would produce two live runs.
//
// Isolation is the default READ COMMITTED, which is enough precisely BECAUSE of the
// explicit row lock: every read that matters happens after FOR UPDATE, so the
// transaction sees the committed state of the row it holds. A stronger level
// (REPEATABLE READ / SERIALIZABLE) would add serialization failures the API would
// then have to retry, for no additional guarantee.
func (p *Postgres) Open(ctx context.Context, params OpenParams) (*Classroom, *Run, error) {
	result, err := inTx(ctx, p.pool, func(tx pgx.Tx) (*transitionResult, error) {
		var (
			ownerID uuid.UUID
			status  string
		)
		err := tx.QueryRow(ctx,
			`SELECT owner_teacher_id, status FROM classrooms WHERE id = $1 FOR UPDATE`,
			params.ClassroomID).Scan(&ownerID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		// Re-checked here and not only in the service: the service's read happened
		// before this transaction, and this is the copy of the check that is
		// actually protected by the lock.
		if ownerID != params.TeacherID {
			return nil, ErrNotOwner
		}
		switch Status(status) {
		case StatusOpen:
			// The loser of a double click. Its own sentinel so the API answers 409
			// rather than reporting a success that did not happen.
			return nil, ErrAlreadyOpen
		case StatusClosed:
		default:
			// Unreachable while classrooms_status_valid holds; reported instead of
			// guessed at, because guessing would silently pick a state.
			return nil, fmt.Errorf("classroom: unexpected stored status %q", status)
		}

		run := &Run{
			ID:              params.RunID,
			ClassroomID:     params.ClassroomID,
			Status:          StatusOpen,
			LiveKitRoomName: params.RoomName,
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO classroom_runs (id, classroom_id, status, livekit_room_name, opened_at)
			VALUES ($1, $2, 'OPEN', $3, now())
			RETURNING opened_at`,
			params.RunID, params.ClassroomID, params.RoomName,
		).Scan(&run.OpenedAt)
		if err != nil {
			return nil, translateOpenError(err)
		}

		// No `AND status = 'CLOSED'` in this WHERE: the row is locked, so the status
		// read above is still true here. Adding it would only hide a bug by making
		// the update match nothing.
		if _, err := tx.Exec(ctx, `
			UPDATE classrooms
			   SET status = 'OPEN', current_run_id = $2, updated_at = now()
			 WHERE id = $1`,
			params.ClassroomID, params.RunID); err != nil {
			return nil, translateWriteError(err)
		}

		updated, err := classroomByID(ctx, tx, params.ClassroomID)
		if err != nil {
			return nil, err
		}
		return &transitionResult{Classroom: updated, Run: run}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return result.Classroom, result.Run, nil
}

// Close ends the current run and flips the classroom to CLOSED in one transaction.
//
// The run is closed BEFORE the classroom is flipped, and both happen under the same
// row lock. Order matters for what a concurrent reader can observe: the classroom
// row is the one everybody reads to answer "is this classroom open?", so it changes
// last — a reader either sees (OPEN, run still running) or (CLOSED, run closed),
// never (CLOSED, run still running).
//
// ---------------------------------------------------------------------------
// SEAMS FOR LATER PHASES — deliberately empty in Phase 3:
//
//   - Phase 6 calls LiveKit TerminateRoom here, AFTER this transaction commits and
//     never before: the control plane must already say CLOSED when the media room
//     disappears, otherwise a student reconnecting into the still-live room would be
//     looking at a classroom the database believes is closed (§33). Terminating
//     inside the transaction would also hold a row lock across a network call.
//   - Phase 8 marks the run's student_sessions ROOM_CLOSED and broadcasts
//     ROOM_CLOSED to the connected consoles here — again after the commit, so a
//     listener that reacts by re-reading the classroom sees CLOSED.
//
// No empty helper functions are left behind for them: a stub that silently does
// nothing is worse than a comment, because a caller can wire it up and believe the
// work happens.
// ---------------------------------------------------------------------------
func (p *Postgres) Close(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, *Run, error) {
	result, err := inTx(ctx, p.pool, func(tx pgx.Tx) (*transitionResult, error) {
		var (
			ownerID      uuid.UUID
			status       string
			currentRunID *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT owner_teacher_id, status, current_run_id FROM classrooms WHERE id = $1 FOR UPDATE`,
			classroomID).Scan(&ownerID, &status, &currentRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if ownerID != teacherID {
			return nil, ErrNotOwner
		}
		switch Status(status) {
		case StatusClosed:
			return nil, ErrAlreadyClosed
		case StatusOpen:
		default:
			return nil, fmt.Errorf("classroom: unexpected stored status %q", status)
		}
		if currentRunID == nil {
			// Impossible while classrooms_run_consistency holds; failing loudly beats
			// writing a CLOSED classroom while a run stays OPEN forever.
			return nil, errors.New("classroom: OPEN classroom has no current run")
		}

		run := &Run{}
		var runStatus string
		var closedAt *time.Time
		err = tx.QueryRow(ctx, `
			UPDATE classroom_runs
			   SET status = 'CLOSED', closed_at = now()
			 WHERE id = $1 AND status = 'OPEN'
			RETURNING `+runColumns,
			*currentRunID,
		).Scan(&run.ID, &run.ClassroomID, &runStatus, &run.LiveKitRoomName, &run.OpenedAt, &closedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("classroom: current run is not OPEN")
		}
		if err != nil {
			return nil, translateWriteError(err)
		}
		run.Status = Status(runStatus)
		run.ClosedAt = closedAt

		if _, err := tx.Exec(ctx, `
			UPDATE classrooms
			   SET status = 'CLOSED', current_run_id = NULL, updated_at = now()
			 WHERE id = $1`,
			classroomID); err != nil {
			return nil, translateWriteError(err)
		}

		updated, err := classroomByID(ctx, tx, classroomID)
		if err != nil {
			return nil, err
		}
		return &transitionResult{Classroom: updated, Run: run}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return result.Classroom, result.Run, nil
}

// transitionResult carries the two rows Open and Close return through the generic
// transaction helper (a function may only have one result type parameter).
type transitionResult struct {
	Classroom *Classroom
	Run       *Run
}

// classroomByID is the one place a single classroom is read.
func classroomByID(ctx context.Context, q querier, id uuid.UUID) (*Classroom, error) {
	query := `SELECT ` + classroomColumns + classroomSource + ` WHERE c.id = $1`
	c, err := scanClassroom(q.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		// The driver error is dropped on purpose: "no rows in result set" is not a
		// business fact, and letting it escape would bypass the error envelope.
		return nil, ErrNotFound
	}
	return c, err
}

// scanClassroom reads one row of classroomColumns.
func scanClassroom(row pgx.Row) (*Classroom, error) {
	var (
		c           Classroom
		status      string
		currentRun  *uuid.UUID
		runID       *uuid.UUID
		runOpenedAt *time.Time
	)
	if err := row.Scan(
		&c.ID, &c.Name, &c.Description, &c.OwnerTeacherID, &status, &currentRun,
		&c.CreatedAt, &c.UpdatedAt, &c.StudentCount, &runID, &runOpenedAt,
	); err != nil {
		return nil, err
	}
	c.Status = Status(status)
	c.CurrentRunID = currentRun
	if runID != nil && runOpenedAt != nil {
		c.CurrentRun = &Run{
			ID:          *runID,
			ClassroomID: c.ID,
			Status:      StatusOpen,
			OpenedAt:    *runOpenedAt,
		}
	}
	return &c, nil
}

// inTx runs fn in a transaction and commits it, rolling back on any error.
//
// WHY a helper: every write in this file is "change a row, then read back what was
// stored", and the read must see the change. A data-modifying CTE cannot express
// that (the outer statement sees the pre-statement snapshot), so the two statements
// have to share a transaction — and having one place that cannot forget the
// rollback is worth more than the few lines it saves.
func inTx[T any](ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) (T, error)) (T, error) {
	var zero T
	if pool == nil {
		return zero, errors.New("classroom: repository is not connected")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	// Rollback after a successful Commit is a no-op that reports ErrTxClosed; it is
	// ignored because the commit's outcome is what the caller must see.
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := fn(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

// translateOpenError maps the one constraint that means "somebody else opened this
// classroom first" onto ErrAlreadyOpen.
//
// WHY this matters: the partial unique index classroom_runs_one_open_idx is the
// second line of defence behind the row lock, and the request that trips it lost a
// race, which is a 409 with a clear code — not a 500. Matching on the constraint
// name keeps a different unique violation (a room-name collision, which would be a
// real bug) from being disguised as a normal double click.
func translateOpenError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation && pgErr.ConstraintName == constraintOneOpenRun {
		return ErrAlreadyOpen
	}
	return translateWriteError(err)
}

// translateWriteError turns constraint violations into business errors or into
// readable internal errors.
//
// The driver's message (SQLSTATE, relation, constraint) must not reach a client
// (§58), so a violation that no rule accounts for is wrapped with a sentence a
// human can act on while the original error stays reachable for the log.
func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case sqlstateUniqueViolation:
		if pgErr.ConstraintName == constraintOneOpenRun {
			return ErrAlreadyOpen
		}
		return fmt.Errorf("classroom: a uniqueness constraint rejected the write (constraint %s): %w", pgErr.ConstraintName, pgErr)
	case sqlstateCheckViolation:
		return fmt.Errorf("classroom: %s: %w", checkConstraintHint(pgErr.ConstraintName), pgErr)
	case sqlstateForeignKeyViolation:
		return fmt.Errorf("classroom: referenced row does not exist (constraint %s): %w", pgErr.ConstraintName, pgErr)
	case sqlstateRestrictViolation:
		return fmt.Errorf("classroom: a row is still referenced (constraint %s): %w", pgErr.ConstraintName, pgErr)
	default:
		return err
	}
}

// checkConstraintHint explains a rejected constraint in the terms of the business
// rule rather than of the schema.
func checkConstraintHint(constraint string) string {
	switch constraint {
	case constraintOwnerIsTeacher:
		return "the classroom owner must be an account with role TEACHER"
	case constraintRunConsistency:
		return "an OPEN classroom must have a current run and a CLOSED one must not"
	case constraintRunsClosedAt:
		return "a run must be OPEN with no closed_at or CLOSED with one"
	default:
		return "a database constraint rejected the value"
	}
}
