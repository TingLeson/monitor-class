package realtime

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresAudience resolves the audience of a broadcast from the classroom domain.
//
// # Why this is a query of its own and not a call to internal/classroom
//
// The question here is "WHO is on this classroom's roster and who owns it?", asked on a
// path that has no teacher and no request behind it (a signed webhook from LiveKit).
// internal/classroom's reads are ownership-scoped by design — Get(classroomID,
// teacherID), ListStudents(classroomID, teacherID) — and that is exactly right for the
// HTTP surface: there, "not yours" must be indistinguishable from "not there". Here the
// caller is the server itself and the ownership question has already been answered by
// the media plane's signature, so re-using the request-scoped reads would mean inventing
// a teacher id to satisfy an authorization check that is not the one being performed.
//
// What this type must NOT do is decide anything: it reads the same columns the roster
// endpoints read, and the scoping rules ("who receives what") live in Service.
type PostgresAudience struct {
	pool *pgxpool.Pool
}

// NewAudience wraps a pool. A nil pool produces an audience that resolves nothing,
// which is the honest state of an API running without a database.
func NewAudience(pool *pgxpool.Pool) *PostgresAudience { return &PostgresAudience{pool: pool} }

// classroomAudienceQuery is the projection both entry points share. One query, two
// keys: the run variant prepends a join on classroom_runs instead of duplicating the
// roster logic.
//
// LEFT JOIN on classroom_students is deliberate: a classroom with an empty roster must
// come back as one row with NULL student columns (a valid audience with nobody in it),
// not as no row at all — "this classroom has no students" and "this classroom does not
// exist" lead to different decisions, and only one of them is worth a warning.
//
// JOIN users (inner): a grant's student_id is NOT NULL and foreign-key enforced, so the
// account exists; an inner join makes a broken grant show up as a missing recipient
// (loud) instead of a message addressed to a nameless student (quiet).
const classroomAudienceQuery = `
	SELECT c.id, c.name, c.owner_teacher_id, cs.student_id, u.display_name
	FROM classrooms c
	LEFT JOIN classroom_students cs ON cs.classroom_id = c.id
	LEFT JOIN users u ON u.id = cs.student_id
	WHERE %s
	ORDER BY u.account ASC, cs.student_id ASC`

// runAudienceQuery is the same projection keyed by the run, which is what an event about
// one student session gives us.
const runAudienceQuery = `
	SELECT c.id, c.name, c.owner_teacher_id, cs.student_id, u.display_name
	FROM classroom_runs r
	JOIN classrooms c ON c.id = r.classroom_id
	LEFT JOIN classroom_students cs ON cs.classroom_id = c.id
	LEFT JOIN users u ON u.id = cs.student_id
	WHERE r.id = $1
	ORDER BY u.account ASC, cs.student_id ASC`

// ClassroomAudience resolves one classroom.
func (p *PostgresAudience) ClassroomAudience(ctx context.Context, classroomID uuid.UUID) (*AudienceView, error) {
	if p == nil || p.pool == nil || classroomID == uuid.Nil {
		return nil, nil
	}
	return p.scan(ctx, fmt.Sprintf(classroomAudienceQuery, "c.id = $1"), classroomID)
}

// RunAudience resolves the classroom a run belongs to.
func (p *PostgresAudience) RunAudience(ctx context.Context, runID uuid.UUID) (*AudienceView, error) {
	if p == nil || p.pool == nil || runID == uuid.Nil {
		return nil, nil
	}
	return p.scan(ctx, runAudienceQuery, runID)
}

func (p *PostgresAudience) scan(ctx context.Context, query string, arg any) (*AudienceView, error) {
	rows, err := p.pool.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var view *AudienceView
	for rows.Next() {
		var (
			classroomID uuid.UUID
			name        string
			owner       uuid.UUID
			studentID   *uuid.UUID
			displayName *string
		)
		if err := rows.Scan(&classroomID, &name, &owner, &studentID, &displayName); err != nil {
			return nil, err
		}
		if view == nil {
			view = &AudienceView{ClassroomID: classroomID, ClassroomName: name, OwnerTeacherID: owner}
		}
		// A NULL student is the empty-roster row of the LEFT JOIN, not a student: see
		// the query's comment.
		if studentID != nil && displayName != nil {
			view.Students = append(view.Students, StudentRef{StudentID: *studentID, DisplayName: *displayName})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if view == nil {
		// The classroom (or run) does not exist. nil and no error: the caller logs it and
		// sends nothing, because there is nobody to tell.
		return nil, nil
	}
	return view, nil
}

// Compile-time assertion: the Postgres audience is what the service is written against.
var _ Audience = (*PostgresAudience)(nil)
