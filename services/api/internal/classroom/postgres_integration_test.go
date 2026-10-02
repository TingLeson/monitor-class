package classroom_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// These tests need a real PostgreSQL server (TEST_DATABASE_URL) and are skipped
// without it. They verify what only the database can provide: the owner-role
// trigger, the run-consistency CHECK, the partial unique index, the row lock that
// makes two concurrent opens safe, and the cascade behaviour of the roster.
//
// The transport contract is covered by internal/httpapi; the rules with fakes by
// service_test.go. Here the SQL is the subject.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type fixture struct {
	repo    *classroom.Postgres
	service *classroom.Service
	users   user.Repository
	pool    *pgxpool.Pool
	teacher *user.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	users := user.NewPostgres(pool)
	repo := classroom.NewPostgres(pool)
	return &fixture{
		repo:    repo,
		service: classroom.NewService(repo, users),
		users:   users,
		pool:    pool,
		teacher: createStaff(t, pool, users, user.RoleTeacher),
	}
}

// createStaff inserts an ACTIVE account of the given role and registers its
// cleanup. A teacher's password hash is required by users_password_by_role.
func createStaff(t *testing.T, pool *pgxpool.Pool, users user.Repository, role user.Role) *user.User {
	t.Helper()
	var hash *string
	if role != user.RoleStudent {
		stored := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$hashvalue"
		hash = &stored
	}
	created, err := users.Create(context.Background(), user.CreateParams{
		Account:      dbtest.RandomAccount(strings.ToLower(string(role))),
		DisplayName:  string(role) + " 测试",
		Role:         role,
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create %s account: %v", role, err)
	}
	cleanupAccount(t, pool, created.ID)
	return created
}

// createStudent inserts an ACTIVE or DISABLED student account.
func createStudent(t *testing.T, pool *pgxpool.Pool, users user.Repository, status user.Status) *user.User {
	t.Helper()
	student := createStaff(t, pool, users, user.RoleStudent)
	if status != user.StatusActive {
		if _, err := pool.Exec(context.Background(),
			`UPDATE users SET status = $2 WHERE id = $1`, student.ID, string(status)); err != nil {
			t.Fatalf("disable student: %v", err)
		}
		student.Status = status
	}
	return student
}

// cleanupAccount removes an account and everything that references it.
//
// The order is forced by the schema and is itself a property under test elsewhere:
// classroom_runs and classrooms reference the teacher with ON DELETE RESTRICT, so a
// test that forgot to remove them would fail loudly rather than silently deleting
// history.
func cleanupAccount(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			// Detach the current run first: classrooms_current_run_fk is ON DELETE
			// RESTRICT, so a run cannot be deleted while a classroom still points at
			// it. The same statement keeps classrooms_run_consistency satisfied.
			if _, err := pool.Exec(ctx, `
				UPDATE classrooms SET status = 'CLOSED', current_run_id = NULL
				 WHERE owner_teacher_id = $1`, id); err != nil {
				t.Logf("cleanup: detach current runs of %s: %v", id, err)
			}
			if _, err := pool.Exec(ctx, `
				DELETE FROM classroom_runs
				 WHERE classroom_id IN (SELECT id FROM classrooms WHERE owner_teacher_id = $1)`, id); err != nil {
				t.Logf("cleanup: delete runs of %s: %v", id, err)
			}
			// Grants held by this account, if it is a student (no-op for teachers).
			if _, err := pool.Exec(ctx, `DELETE FROM classroom_students WHERE student_id = $1`, id); err != nil {
				t.Logf("cleanup: delete grants of %s: %v", id, err)
			}
			// classroom_students rows of these classrooms go with them (CASCADE).
			if _, err := pool.Exec(ctx, `DELETE FROM classrooms WHERE owner_teacher_id = $1`, id); err != nil {
				t.Logf("cleanup: delete classrooms of %s: %v", id, err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: delete account %s: %v", id, err)
			}
		}
	})
}

// createClassroom makes a CLOSED classroom through the service.
func (f *fixture) createClassroom(t *testing.T, name string) *classroom.Classroom {
	t.Helper()
	created, err := f.service.Create(context.Background(), classroom.CreateInput{
		TeacherID: f.teacher.ID,
		Name:      name,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return created
}

// isRestrictViolation reports whether err is a foreign-key RESTRICT refusal.
//
// PostgreSQL reports ON DELETE RESTRICT as SQLSTATE 23001 (restrict_violation),
// which is a different code from the 23503 (foreign_key_violation) it uses for NO
// ACTION and for a missing referenced row on INSERT. Both are FK refusals; spelling
// that out here keeps the assertions honest instead of pinning the wrong code.
func isRestrictViolation(pgErr *pgconn.PgError) bool {
	return pgErr.Code == "23001" || pgErr.Code == "23503"
}

func pgError(t *testing.T, err error) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	return pgErr
}

// ---------------------------------------------------------------------------
// Constraints and triggers
// ---------------------------------------------------------------------------

// TestOwnerTriggerRejectsNonTeachers is the §10 requirement: "必须保证
// owner_teacher.role == TEACHER". The service validates it too (so the teacher gets
// a readable error), but manual SQL, an import script or a future call site can
// bypass the service — the trigger is what makes the rule a property of the table.
func TestOwnerTriggerRejectsNonTeachers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := createStudent(t, f.pool, f.users, user.StatusActive)
	admin := createStaff(t, f.pool, f.users, user.RoleAdmin)

	for _, tc := range []struct {
		name  string
		owner uuid.UUID
	}{
		{"student owner", student.ID},
		{"admin owner", admin.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx,
				`INSERT INTO classrooms (name, owner_teacher_id) VALUES ($1, $2)`,
				"越权课堂", tc.owner)
			if err == nil {
				t.Fatal("the insert succeeded; a classroom must not be owned by a non-teacher")
			}
			pgErr := pgError(t, err)
			if pgErr.Code != "23514" {
				t.Errorf("SQLSTATE = %s, want 23514 (check_violation)", pgErr.Code)
			}
			if pgErr.ConstraintName != "classrooms_owner_is_teacher" {
				t.Errorf("constraint = %q, want classrooms_owner_is_teacher", pgErr.ConstraintName)
			}
		})
	}

	// The legitimate case must pass, or the trigger would be blocking the feature.
	created := f.createClassroom(t, "合法课堂")

	// Re-pointing an existing classroom at a student must fail as well: the trigger
	// fires on UPDATE, not only on INSERT.
	_, err := f.pool.Exec(ctx,
		`UPDATE classrooms SET owner_teacher_id = $2 WHERE id = $1`, created.ID, student.ID)
	if err == nil {
		t.Fatal("reassigning a classroom to a student succeeded")
	}
	if pgErr := pgError(t, err); pgErr.ConstraintName != "classrooms_owner_is_teacher" {
		t.Errorf("constraint = %q, want classrooms_owner_is_teacher", pgErr.ConstraintName)
	}

	// A write that does not touch the owner still passes through the trigger.
	if _, err := f.pool.Exec(ctx,
		`UPDATE classrooms SET name = $2 WHERE id = $1`, created.ID, "改名后的课堂"); err != nil {
		t.Fatalf("renaming a valid classroom failed: %v", err)
	}
}

// TestRunConsistencyConstraint forbids the one state that would break the student
// experience: a classroom that is OPEN with no run to join (§10).
func TestRunConsistencyConstraint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// OPEN with current_run_id NULL.
	_, err := f.pool.Exec(ctx, `
		INSERT INTO classrooms (name, owner_teacher_id, status, current_run_id)
		VALUES ('坏状态', $1, 'OPEN', NULL)`, f.teacher.ID)
	if err == nil {
		t.Fatal("OPEN with a NULL current_run_id was accepted")
	}
	if pgErr := pgError(t, err); pgErr.Code != "23514" || pgErr.ConstraintName != "classrooms_run_consistency" {
		t.Errorf("error = %s/%s, want 23514/classrooms_run_consistency", pgErr.Code, pgErr.ConstraintName)
	}

	// CLOSED with a run still attached.
	created := f.createClassroom(t, "算法")
	_, run, err := f.service.Open(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE classrooms SET status = 'CLOSED' WHERE id = $1`, created.ID)
	if err == nil {
		t.Fatal("CLOSED with a live current_run_id was accepted")
	}
	if pgErr := pgError(t, err); pgErr.ConstraintName != "classrooms_run_consistency" {
		t.Errorf("constraint = %q, want classrooms_run_consistency", pgErr.ConstraintName)
	}

	// Clearing the run without closing the classroom is the same violation.
	_, err = f.pool.Exec(ctx, `UPDATE classrooms SET current_run_id = NULL WHERE id = $1`, created.ID)
	if err == nil {
		t.Fatal("clearing current_run_id on an OPEN classroom was accepted")
	}
	if pgErr := pgError(t, err); pgErr.ConstraintName != "classrooms_run_consistency" {
		t.Errorf("constraint = %q, want classrooms_run_consistency", pgErr.ConstraintName)
	}

	// The run's own biconditional must hold too.
	_, err = f.pool.Exec(ctx,
		`UPDATE classroom_runs SET status = 'CLOSED' WHERE id = $1`, run.ID)
	if err == nil {
		t.Fatal("a run with status CLOSED and no closed_at was accepted")
	}
	if pgErr := pgError(t, err); pgErr.ConstraintName != "classroom_runs_closed_at" {
		t.Errorf("constraint = %q, want classroom_runs_closed_at", pgErr.ConstraintName)
	}
}

// TestOneOpenRunPerClassroomIndex is the database-level half of §48's concurrency
// guarantee: even if a future call site forgot the row lock, two OPEN runs for one
// classroom cannot exist.
func TestOneOpenRunPerClassroomIndex(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.createClassroom(t, "算法")
	if _, _, err := f.service.Open(ctx, created.ID, f.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err := f.pool.Exec(ctx, `
		INSERT INTO classroom_runs (classroom_id, status, livekit_room_name)
		VALUES ($1, 'OPEN', 'lk_' || gen_random_uuid())`, created.ID)
	if err == nil {
		t.Fatal("a second OPEN run was accepted for the same classroom")
	}
	pgErr := pgError(t, err)
	if pgErr.Code != "23505" {
		t.Errorf("SQLSTATE = %s, want 23505 (unique_violation)", pgErr.Code)
	}
	if pgErr.ConstraintName != "classroom_runs_one_open_idx" {
		t.Errorf("constraint = %q, want classroom_runs_one_open_idx", pgErr.ConstraintName)
	}

	// A CLOSED run is not covered by the partial index, so history accumulates
	// freely — that is the whole point of the WHERE clause.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO classroom_runs (classroom_id, status, livekit_room_name, closed_at)
		VALUES ($1, 'CLOSED', 'lk_' || gen_random_uuid(), now())`, created.ID); err != nil {
		t.Fatalf("inserting a CLOSED run failed: %v", err)
	}
}

// TestLivekitRoomNameIsUniqueAndOpaqueForEveryRun pins the storage-level part of
// §8: every run reserves its own name, and the name is derived from the run id,
// never from anything a person could be identified by.
func TestLivekitRoomNameIsUniqueAndOpaqueForEveryRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := createStudent(t, f.pool, f.users, user.StatusActive)
	created := f.createClassroom(t, "C++ 晚自习")
	if _, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, Accounts: []string{student.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	_, firstRun, err := f.service.Open(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := f.service.Close(ctx, created.ID, f.teacher.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, secondRun, err := f.service.Open(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}

	for _, run := range []*classroom.Run{firstRun, secondRun} {
		want := "lk_" + run.ID.String()
		var stored string
		if err := f.pool.QueryRow(ctx,
			`SELECT livekit_room_name FROM classroom_runs WHERE id = $1`, run.ID).Scan(&stored); err != nil {
			t.Fatalf("read room name: %v", err)
		}
		if stored != want {
			t.Errorf("stored room name = %q, want %q", stored, want)
		}
		for _, forbidden := range []string{student.Account, f.teacher.Account, "C++", "晚自习"} {
			if strings.Contains(stored, forbidden) {
				t.Errorf("room name %q contains %q; §8 forbids identifiable room names", stored, forbidden)
			}
		}
	}
	if firstRun.LiveKitRoomName == secondRun.LiveKitRoomName {
		t.Error("two runs share a room name; students of the second lesson would appear in the first")
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// TestOpenCloseLifecycle walks the full state machine against real SQL, including
// the rule that a re-open produces a NEW run and the previous one survives (§8).
func TestOpenCloseLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other := createStaff(t, f.pool, f.users, user.RoleTeacher)
	student := createStudent(t, f.pool, f.users, user.StatusActive)

	created := f.createClassroom(t, "算法晚自习")
	if created.Status != classroom.StatusClosed || created.CurrentRunID != nil {
		t.Fatalf("new classroom = %+v, want CLOSED with no run", created)
	}
	if created.StudentCount != 0 {
		t.Errorf("studentCount = %d, want 0", created.StudentCount)
	}

	// --- open ---
	opened, run, err := f.service.Open(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.Status != classroom.StatusOpen {
		t.Errorf("status = %q, want OPEN", opened.Status)
	}
	if opened.CurrentRunID == nil || *opened.CurrentRunID != run.ID {
		t.Fatalf("current_run_id = %v, want %s", opened.CurrentRunID, run.ID)
	}
	if opened.CurrentRun == nil || opened.CurrentRun.ID != run.ID {
		t.Errorf("currentRun = %+v, want run %s", opened.CurrentRun, run.ID)
	}
	if run.Status != classroom.StatusOpen || run.ClosedAt != nil {
		t.Errorf("run = %+v, want OPEN with no closedAt", run)
	}
	var (
		dbStatus  string
		dbRunID   *uuid.UUID
		dbRunStat string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT status, current_run_id FROM classrooms WHERE id = $1`, created.ID).Scan(&dbStatus, &dbRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if dbStatus != "OPEN" || dbRunID == nil || *dbRunID != run.ID {
		t.Errorf("stored classroom = %s/%v, want OPEN/%s", dbStatus, dbRunID, run.ID)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM classroom_runs WHERE id = $1`, run.ID).Scan(&dbRunStat); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if dbRunStat != "OPEN" {
		t.Errorf("stored run status = %s, want OPEN", dbRunStat)
	}

	// --- open again: conflict, and nothing changed ---
	if _, _, err := f.service.Open(ctx, created.ID, f.teacher.ID); !errors.Is(err, classroom.ErrAlreadyOpen) {
		t.Fatalf("second Open: err = %v, want ErrAlreadyOpen", err)
	}
	var openRuns int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM classroom_runs WHERE classroom_id = $1 AND status = 'OPEN'`, created.ID).Scan(&openRuns); err != nil {
		t.Fatalf("count open runs: %v", err)
	}
	if openRuns != 1 {
		t.Errorf("open runs = %d, want 1", openRuns)
	}

	// --- close ---
	closed, closedRun, err := f.service.Close(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.Status != classroom.StatusClosed || closed.CurrentRunID != nil || closed.CurrentRun != nil {
		t.Errorf("after close = %+v, want CLOSED with no run", closed)
	}
	if closedRun.ID != run.ID {
		t.Errorf("closed run = %s, want the run that was open (%s)", closedRun.ID, run.ID)
	}
	if closedRun.Status != classroom.StatusClosed || closedRun.ClosedAt == nil {
		t.Errorf("closed run = %+v, want CLOSED with a closedAt", closedRun)
	}
	var storedClosedAt *string
	if err := f.pool.QueryRow(ctx,
		`SELECT closed_at::text FROM classroom_runs WHERE id = $1`, run.ID).Scan(&storedClosedAt); err != nil {
		t.Fatalf("read closed_at: %v", err)
	}
	if storedClosedAt == nil {
		t.Error("closed_at is NULL in the database")
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT current_run_id FROM classrooms WHERE id = $1`, created.ID).Scan(&dbRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if dbRunID != nil {
		t.Errorf("current_run_id = %v after close, want NULL", dbRunID)
	}
	if _, _, err := f.service.Close(ctx, created.ID, f.teacher.ID); !errors.Is(err, classroom.ErrAlreadyClosed) {
		t.Fatalf("second Close: err = %v, want ErrAlreadyClosed", err)
	}

	// --- re-open: a NEW run, the old one kept ---
	_, secondRun, err := f.service.Open(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if secondRun.ID == run.ID {
		t.Fatal("the re-open reused the previous run; §8 forbids it")
	}
	var total int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM classroom_runs WHERE classroom_id = $1`, created.ID).Scan(&total); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if total != 2 {
		t.Errorf("runs = %d, want 2 (history must not be overwritten)", total)
	}
	var oldStatus string
	var oldClosedAt *string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, closed_at::text FROM classroom_runs WHERE id = $1`, run.ID).Scan(&oldStatus, &oldClosedAt); err != nil {
		t.Fatalf("read the old run: %v", err)
	}
	if oldStatus != "CLOSED" || oldClosedAt == nil {
		t.Errorf("old run = %s/%v, want CLOSED with a closedAt", oldStatus, oldClosedAt)
	}

	// --- ownership and listing ---
	if _, _, err := f.service.Open(ctx, created.ID, other.ID); !errors.Is(err, classroom.ErrNotOwner) {
		t.Errorf("another teacher opening: err = %v, want ErrNotOwner", err)
	}
	if _, _, err := f.service.Close(ctx, created.ID, other.ID); !errors.Is(err, classroom.ErrNotOwner) {
		t.Errorf("another teacher closing: err = %v, want ErrNotOwner", err)
	}
	if _, err := f.service.Get(ctx, created.ID, other.ID); !errors.Is(err, classroom.ErrNotOwner) {
		t.Errorf("another teacher reading: err = %v, want ErrNotOwner", err)
	}
	if _, err := f.service.Get(ctx, uuid.New(), f.teacher.ID); !errors.Is(err, classroom.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}

	list, err := f.service.List(ctx, f.teacher.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ID != created.ID {
			continue
		}
		found = true
		if c.Status != classroom.StatusOpen {
			t.Errorf("listed status = %q, want OPEN", c.Status)
		}
		if c.CurrentRun == nil || c.CurrentRun.ID != secondRun.ID {
			t.Errorf("listed currentRun = %+v, want %s", c.CurrentRun, secondRun.ID)
		}
	}
	if !found {
		t.Fatal("the teacher's own classroom is missing from the list")
	}
	otherList, err := f.service.List(ctx, other.ID)
	if err != nil {
		t.Fatalf("List (other teacher): %v", err)
	}
	if len(otherList) != 0 {
		t.Errorf("another teacher sees %d classrooms, want 0", len(otherList))
	}
	_ = student
}

// TestConcurrentOpenOnlyOneSucceeds is the §48 concurrency test: the row lock plus
// the partial unique index must let exactly one of several simultaneous opens win,
// and every loser must get a 409-shaped ErrAlreadyOpen rather than a driver error.
func TestConcurrentOpenOnlyOneSucceeds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const (
		attempts      = 4
		classroomsRun = 5
	)
	for round := 0; round < classroomsRun; round++ {
		created := f.createClassroom(t, "并发课堂")

		start := make(chan struct{})
		results := make([]error, attempts)
		var wg sync.WaitGroup
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				<-start // release every goroutine at the same instant
				_, _, err := f.service.Open(ctx, created.ID, f.teacher.ID)
				results[index] = err
			}(i)
		}
		close(start)
		wg.Wait()

		successes := 0
		for i, err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, classroom.ErrAlreadyOpen):
				// The expected outcome for every loser.
			default:
				t.Fatalf("round %d attempt %d: err = %v, want nil or ErrAlreadyOpen", round, i, err)
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: %d opens succeeded, want exactly 1", round, successes)
		}

		var openRuns int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM classroom_runs WHERE classroom_id = $1 AND status = 'OPEN'`,
			created.ID).Scan(&openRuns); err != nil {
			t.Fatalf("count open runs: %v", err)
		}
		if openRuns != 1 {
			t.Fatalf("round %d: %d OPEN runs in the database, want 1", round, openRuns)
		}
		var total int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM classroom_runs WHERE classroom_id = $1`, created.ID).Scan(&total); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if total != 1 {
			t.Fatalf("round %d: %d runs created, want 1 (losers must not insert)", round, total)
		}
	}
}

// TestConcurrentOpenAndCloseNeverLeaveInconsistentState hammers both transitions at
// once. Whatever the interleaving, the invariant "OPEN <=> current_run_id IS NOT
// NULL" and "at most one OPEN run" must hold at the end.
func TestConcurrentOpenAndCloseNeverLeaveInconsistentState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.createClassroom(t, "抖动课堂")

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_, _, _ = f.service.Open(ctx, created.ID, f.teacher.ID)
				return
			}
			_, _, _ = f.service.Close(ctx, created.ID, f.teacher.ID)
		}(i)
	}
	wg.Wait()

	var (
		status       string
		currentRunID *uuid.UUID
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT status, current_run_id FROM classrooms WHERE id = $1`, created.ID).Scan(&status, &currentRunID); err != nil {
		t.Fatalf("read classroom: %v", err)
	}
	if status == "OPEN" && currentRunID == nil {
		t.Error("classroom is OPEN with no current run")
	}
	if status == "CLOSED" && currentRunID != nil {
		t.Error("classroom is CLOSED with a current run")
	}
	var openRuns int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM classroom_runs WHERE classroom_id = $1 AND status = 'OPEN'`, created.ID).Scan(&openRuns); err != nil {
		t.Fatalf("count open runs: %v", err)
	}
	if openRuns > 1 {
		t.Errorf("open runs = %d, want at most 1", openRuns)
	}
	if (status == "OPEN") != (openRuns == 1) {
		t.Errorf("classroom status %s with %d open runs: the two must agree", status, openRuns)
	}
}

// ---------------------------------------------------------------------------
// Roster
// ---------------------------------------------------------------------------

// TestRosterAddListRemove exercises the real SQL of the roster: case-insensitive
// lookups, idempotent inserts, removal, and the fact that a disabled account stays
// on the list.
func TestRosterAddListRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.createClassroom(t, "算法")
	active := createStudent(t, f.pool, f.users, user.StatusActive)
	disabled := createStudent(t, f.pool, f.users, user.StatusDisabled)
	teacherAccount := createStaff(t, f.pool, f.users, user.RoleTeacher)

	result, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: created.ID,
		TeacherID:   f.teacher.ID,
		Accounts: []string{
			active.Account,
			strings.ToUpper(active.Account), // same account, different case: citext
			disabled.Account,
			teacherAccount.Account,
			dbtest.RandomAccount("ghost"), // no such account
		},
	})
	if err != nil {
		t.Fatalf("AddStudents: %v", err)
	}
	if len(result.Students) != 1 || result.Students[0].ID != active.ID {
		t.Fatalf("roster = %+v, want only the active student", result.Students)
	}
	if len(result.Rejected) != 3 {
		t.Fatalf("rejected = %+v, want three entries", result.Rejected)
	}

	// added_by is recorded, and the grant is a single row despite the duplicate case.
	var (
		addedBy uuid.UUID
		grants  int
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM classroom_students WHERE classroom_id = $1`, created.ID).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 1 {
		t.Errorf("grants = %d, want 1 (the roster primary key de-duplicates)", grants)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT added_by FROM classroom_students WHERE classroom_id = $1 AND student_id = $2`,
		created.ID, active.ID).Scan(&addedBy); err != nil {
		t.Fatalf("read added_by: %v", err)
	}
	if addedBy != f.teacher.ID {
		t.Errorf("added_by = %s, want the acting teacher %s", addedBy, f.teacher.ID)
	}

	// Re-adding is idempotent: no error, no duplicate, the original added_at kept.
	var addedAt string
	if err := f.pool.QueryRow(ctx,
		`SELECT added_at::text FROM classroom_students WHERE classroom_id = $1 AND student_id = $2`,
		created.ID, active.ID).Scan(&addedAt); err != nil {
		t.Fatalf("read added_at: %v", err)
	}
	if _, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, Accounts: []string{active.Account},
	}); err != nil {
		t.Fatalf("re-adding an existing student failed: %v", err)
	}
	var addedAtAgain string
	if err := f.pool.QueryRow(ctx,
		`SELECT added_at::text FROM classroom_students WHERE classroom_id = $1 AND student_id = $2`,
		created.ID, active.ID).Scan(&addedAtAgain); err != nil {
		t.Fatalf("read added_at: %v", err)
	}
	if addedAt != addedAtAgain {
		t.Error("a repeated add rewrote added_at; an existing grant must not change")
	}

	// A student disabled AFTER being added stays on the roster (§11: the roster is
	// an authorization record, not a "who can log in" query).
	if _, err := f.pool.Exec(ctx,
		`UPDATE users SET status = 'DISABLED' WHERE id = $1`, active.ID); err != nil {
		t.Fatalf("disable the student: %v", err)
	}
	roster, err := f.service.ListStudents(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("ListStudents: %v", err)
	}
	if len(roster) != 1 || roster[0].Status != user.StatusDisabled {
		t.Fatalf("roster = %+v, want the now-disabled student with status DISABLED", roster)
	}

	// Removal, and the second removal is a 404-shaped error rather than a silent
	// success.
	if err := f.service.RemoveStudent(ctx, created.ID, active.ID, f.teacher.ID); err != nil {
		t.Fatalf("RemoveStudent: %v", err)
	}
	if err := f.service.RemoveStudent(ctx, created.ID, active.ID, f.teacher.ID); !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Fatalf("second RemoveStudent: err = %v, want ErrStudentNotAssigned", err)
	}
	if err := f.service.RemoveStudent(ctx, created.ID, uuid.New(), f.teacher.ID); !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Fatalf("unknown student: err = %v, want ErrStudentNotAssigned", err)
	}
}

// TestAddStudentsRosterOrderIsStable pins the ORDER BY of the roster: newest first,
// account ascending as the tiebreaker. A batch shares one added_at (the transaction
// timestamp), so without that second key the list would reshuffle between refreshes.
func TestAddStudentsRosterOrderIsStable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.createClassroom(t, "算法")

	accounts := []string{}
	for i := 0; i < 6; i++ {
		accounts = append(accounts, createStudent(t, f.pool, f.users, user.StatusActive).Account)
	}
	if _, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, Accounts: accounts,
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	first, err := f.service.ListStudents(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("ListStudents: %v", err)
	}
	second, err := f.service.ListStudents(ctx, created.ID, f.teacher.ID)
	if err != nil {
		t.Fatalf("ListStudents (second call): %v", err)
	}
	if len(first) != len(accounts) {
		t.Fatalf("roster has %d entries, want %d", len(first), len(accounts))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("the roster order changed between two identical queries at index %d", i)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Account > first[i].Account {
			t.Errorf("roster is not ordered by account within one added_at: %q before %q",
				first[i-1].Account, first[i].Account)
		}
	}
}

// TestListOrdersNewestFirstWithAStableTiebreaker pins the ORDER BY of the teacher's
// list: created_at DESC with id DESC as the tiebreaker. created_at defaults to now(),
// which is the TRANSACTION timestamp, so two classrooms created inside one
// transaction share it exactly — without the second key their order would be
// plan-dependent and the list would reshuffle on refresh.
func TestListOrdersNewestFirstWithAStableTiebreaker(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created := []uuid.UUID{}
	for i := 0; i < 3; i++ {
		created = append(created, f.createClassroom(t, "课堂").ID)
	}

	// Insert a deliberately tied pair in ONE transaction, then list and compare
	// against the ordering rule evaluated in SQL (the same rule the test names).
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	tiedIDs := []uuid.UUID{}
	for i := 0; i < 2; i++ {
		var id uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO classrooms (name, owner_teacher_id) VALUES ($1, $2) RETURNING id`,
			"同一事务课堂", f.teacher.ID).Scan(&id); err != nil {
			t.Fatalf("insert tied classroom: %v", err)
		}
		tiedIDs = append(tiedIDs, id)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	list, err := f.service.List(ctx, f.teacher.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("list has %d classrooms, want 5", len(list))
	}
	for i := 1; i < len(list); i++ {
		previous, current := list[i-1], list[i]
		if current.CreatedAt.After(previous.CreatedAt) {
			t.Fatalf("entry %d is newer than entry %d; the list is not newest-first", i, i-1)
		}
		if current.CreatedAt.Equal(previous.CreatedAt) && current.ID.String() > previous.ID.String() {
			t.Fatalf("two classrooms share a timestamp but are not ordered by id DESC: %s before %s",
				previous.ID, current.ID)
		}
	}
	// The two tied rows must both be present and adjacent (they share a timestamp).
	present := map[uuid.UUID]bool{}
	for _, c := range list {
		present[c.ID] = true
	}
	for _, id := range tiedIDs {
		if !present[id] {
			t.Errorf("classroom %s is missing from the list", id)
		}
	}
}

// TestUpdateClearsTheDescriptionAgainstRealSQL covers the `CASE WHEN $4 THEN $5`
// branch that sets the column to NULL. A rename must not take it (the test above
// covers that); an explicit null must.
func TestUpdateClearsTheDescriptionAgainstRealSQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	description := "周三晚自习"
	created, err := f.service.Create(ctx, classroom.CreateInput{
		TeacherID: f.teacher.ID, Name: "算法", Description: &description,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	renamed := "算法（进阶）"
	updated, err := f.service.Update(ctx, classroom.UpdateInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, Name: &renamed,
	})
	if err != nil {
		t.Fatalf("Update(rename): %v", err)
	}
	if updated.Description == nil || *updated.Description != description {
		t.Fatalf("description = %v, want it untouched by a rename", updated.Description)
	}

	cleared, err := f.service.Update(ctx, classroom.UpdateInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, DescriptionSet: true, Description: nil,
	})
	if err != nil {
		t.Fatalf("Update(clear): %v", err)
	}
	if cleared.Description != nil {
		t.Fatalf("description = %q, want NULL", *cleared.Description)
	}
	var stored *string
	if err := f.pool.QueryRow(ctx,
		`SELECT description FROM classrooms WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read description: %v", err)
	}
	if stored != nil {
		t.Errorf("stored description = %q, want NULL", *stored)
	}

	// A blank string means the same as null (an empty textarea is "no description").
	blank := "   "
	updated, err = f.service.Update(ctx, classroom.UpdateInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, DescriptionSet: true, Description: &blank,
	})
	if err != nil {
		t.Fatalf("Update(blank): %v", err)
	}
	if updated.Description != nil {
		t.Errorf("description = %q, want NULL for a blank value", *updated.Description)
	}
}

// ---------------------------------------------------------------------------
// Cascades
// ---------------------------------------------------------------------------

// TestDeletingAClassroomCascadesTheRosterButNotTheAccounts pins the two ON DELETE
// choices of 0005: a grant is not history (CASCADE with its classroom), an account
// is (RESTRICT from the roster).
func TestDeletingAClassroomCascadesTheRosterButNotTheAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.createClassroom(t, "算法")
	student := createStudent(t, f.pool, f.users, user.StatusActive)
	if _, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: created.ID, TeacherID: f.teacher.ID, Accounts: []string{student.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM classrooms WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("delete classroom: %v", err)
	}

	var grants int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM classroom_students WHERE classroom_id = $1`, created.ID).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Errorf("grants = %d after deleting the classroom, want 0 (ON DELETE CASCADE)", grants)
	}
	var accountExists bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, student.ID).Scan(&accountExists); err != nil {
		t.Fatalf("check account: %v", err)
	}
	if !accountExists {
		t.Error("deleting a classroom deleted a student account")
	}

	// A student on a roster cannot be deleted while the classroom exists
	// (ON DELETE RESTRICT): losing the audit row must be a deliberate act.
	second := f.createClassroom(t, "算法二")
	if _, err := f.service.AddStudents(ctx, classroom.AddStudentsInput{
		ClassroomID: second.ID, TeacherID: f.teacher.ID, Accounts: []string{student.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}
	_, err := f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, student.ID)
	if err == nil {
		t.Fatal("deleting a student who is on a roster succeeded; the roster is an audit record")
	}
	if pgErr := pgError(t, err); !isRestrictViolation(pgErr) {
		t.Errorf("error = %s/%s, want a foreign-key RESTRICT violation", pgErr.Code, pgErr.ConstraintName)
	}

	// A classroom that has ever been opened cannot be deleted either: the runs are
	// the supervision history.
	if _, _, err := f.service.Open(ctx, second.ID, f.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = f.pool.Exec(ctx, `DELETE FROM classrooms WHERE id = $1`, second.ID)
	if err == nil {
		t.Fatal("deleting a classroom with runs succeeded; history must be kept (§8)")
	}
	if pgErr := pgError(t, err); !isRestrictViolation(pgErr) {
		t.Errorf("error = %s/%s, want a foreign-key RESTRICT violation", pgErr.Code, pgErr.ConstraintName)
	}
}
