package session_test

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/migrate"
	"github.com/classwatch/classwatch/services/api/internal/session"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
	"github.com/classwatch/classwatch/services/api/migrations"
)

// The session repository against a real PostgreSQL server: the partial unique index,
// the ON CONFLICT arbiter that implements §50, the compare-and-set that protects a
// session from a stale poll, and the JOIN that gives the monitoring wall its names.
//
// These are the properties the unit tests fake away. A fake can be made to reuse a row;
// only the database can prove that two concurrent joins cannot create two rows.
//
// Skipped when TEST_DATABASE_URL is unset, so `go test ./...` stays green without a
// server.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// fixture is one run with one teacher, two students, and a session repository.
type fixture struct {
	pool     *pgxpool.Pool
	repo     *session.Postgres
	teacher  *user.User
	studentA *user.User
	studentB *user.User

	classroomID uuid.UUID
	runID       uuid.UUID
	roomName    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	users := user.NewPostgres(pool)
	classrooms := classroom.NewPostgres(pool)
	ctx := context.Background()

	// TEACHER accounts must carry a password hash (the database enforces it); the value
	// is a placeholder because these tests never log in.
	hash := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$hashvalue"
	teacher, err := users.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("teacher"), DisplayName: "王老师",
		Role: user.RoleTeacher, PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create teacher: %v", err)
	}
	studentA, err := users.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("student"), DisplayName: "张三", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student A: %v", err)
	}
	studentB, err := users.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("student"), DisplayName: "李四", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student B: %v", err)
	}

	created, err := classrooms.Create(ctx, classroom.CreateParams{
		OwnerTeacherID: teacher.ID, Name: "C++ 算法训练",
	})
	if err != nil {
		t.Fatalf("create classroom: %v", err)
	}
	runID := uuid.New()
	if _, _, err := classrooms.Open(ctx, classroom.OpenParams{
		ClassroomID: created.ID, TeacherID: teacher.ID, RunID: runID, RoomName: classroom.RoomName(runID),
	}); err != nil {
		t.Fatalf("open classroom: %v", err)
	}
	// The monitoring wall reads the ROSTER (§29/§51), so the two students have to be
	// authorized for the classroom, not merely created. Creating the grant here and not
	// in the individual tests is what keeps "the wall lists exactly these people" true
	// for every test in this file.
	if err := classrooms.AddStudents(ctx, created.ID, []uuid.UUID{studentA.ID, studentB.ID}, teacher.ID); err != nil {
		t.Fatalf("add students to the roster: %v", err)
	}

	t.Cleanup(func() { cleanup(t, pool, teacher.ID, studentA.ID, studentB.ID) })

	return &fixture{
		pool: pool, repo: session.NewPostgres(pool),
		teacher: teacher, studentA: studentA, studentB: studentB,
		classroomID: created.ID, runID: runID, roomName: classroom.RoomName(runID),
	}
}

// cleanup removes the rows this fixture created, in the order the RESTRICT foreign keys
// allow. It never fails the test: a leftover row in a shared test database is a
// nuisance, not a verdict.
func cleanup(t *testing.T, pool *pgxpool.Pool, teacherID uuid.UUID, otherIDs ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// The teacher is deleted last: classrooms reference it, and the rows above are
	// already gone by then.
	accounts := append(append([]uuid.UUID{}, otherIDs...), teacherID)
	for _, id := range accounts {
		if _, err := pool.Exec(ctx, `DELETE FROM student_sessions WHERE student_id = $1`, id); err != nil {
			t.Logf("cleanup: sessions of %s: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE classrooms SET status = 'CLOSED', current_run_id = NULL WHERE owner_teacher_id = $1`, teacherID); err != nil {
		t.Logf("cleanup: detach runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM classroom_runs WHERE classroom_id IN (SELECT id FROM classrooms WHERE owner_teacher_id = $1)`, teacherID); err != nil {
		t.Logf("cleanup: runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM classroom_students WHERE classroom_id IN (SELECT id FROM classrooms WHERE owner_teacher_id = $1)`, teacherID); err != nil {
		t.Logf("cleanup: grants: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM classrooms WHERE owner_teacher_id = $1`, teacherID); err != nil {
		t.Logf("cleanup: classrooms: %v", err)
	}
	for _, id := range accounts {
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Logf("cleanup: account %s: %v", id, err)
		}
	}
}

// create makes (or reuses) a session through the repository.
func (f *fixture) create(t *testing.T, studentID uuid.UUID) *session.StudentSession {
	t.Helper()
	stored, err := f.repo.CreateOrReuse(context.Background(), session.CreateOrReuseParams{
		SessionID: uuid.New(), ClassroomRunID: f.runID, StudentID: studentID,
	})
	if err != nil {
		t.Fatalf("CreateOrReuse: %v", err)
	}
	return stored
}

// ---------------------------------------------------------------------------
// Migration 0007
// ---------------------------------------------------------------------------

// TestStudentSessionsMigrationIsIdempotentAndEnforcesItsInvariants is the §12/§44/§50
// acceptance test for the new table. It runs on a throwaway database because the
// assertions are about the FIRST application of the migration.
func TestStudentSessionsMigrationIsIdempotentAndEnforcesItsInvariants(t *testing.T) {
	pool := dbtest.FreshPool(t) // already migrated once, from scratch
	ctx := context.Background()

	// The embedded 0007 file must be part of the binary's schema history.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "student_sessions") {
			found = true
		}
	}
	if !found {
		t.Fatal("0007_student_sessions.sql is not embedded in the binary")
	}

	// Idempotency: a second run applies nothing.
	applied, err := migrate.Up(ctx, pool)
	if err != nil {
		t.Fatalf("second migrate.Up(): %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate.Up() applied %d migrations, want 0", len(applied))
	}

	// The schema shape the session code depends on.
	var indexDef string
	err = pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE tablename = 'student_sessions' AND indexname = 'student_sessions_active_idx'`).Scan(&indexDef)
	if err != nil {
		t.Fatalf("student_sessions_active_idx is missing: %v", err)
	}
	if !strings.Contains(indexDef, "UNIQUE") || !strings.Contains(indexDef, "WHERE") {
		t.Errorf("student_sessions_active_idx = %q, want a partial UNIQUE index", indexDef)
	}
	var runIndexExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_indexes
		 WHERE tablename = 'student_sessions' AND indexname = 'student_sessions_run_idx')`).Scan(&runIndexExists); err != nil {
		t.Fatalf("look up student_sessions_run_idx: %v", err)
	}
	if !runIndexExists {
		t.Error("student_sessions_run_idx is missing (the monitoring wall's access path)")
	}
}

// TestIdentityMustEqualTheSessionID is the §44 rule as a database guarantee: any
// attempt to use a human-readable participant identity fails, whatever the writer
// believes.
func TestIdentityMustEqualTheSessionID(t *testing.T) {
	pool := dbtest.Pool(t)
	f := newFixture(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO student_sessions (classroom_run_id, student_id, livekit_identity, status)
		VALUES ($1, $2, 'zhangsan', 'CONNECTING')`, f.runID, f.studentA.ID)
	if err == nil {
		t.Fatal("the database accepted a human-readable livekit_identity")
	}
	if !strings.Contains(err.Error(), "student_sessions_identity_is_id") {
		t.Errorf("error = %v, want the identity check constraint to reject it", err)
	}
}

// TestActiveSessionIsUniquePerRunAndStudent is §50 at the database level: two active
// sessions for one student in one run are a state the schema cannot represent, which is
// what keeps a second tile of the same person off the wall.
func TestActiveSessionIsUniquePerRunAndStudent(t *testing.T) {
	pool := dbtest.Pool(t)
	f := newFixture(t)
	ctx := context.Background()

	first := f.create(t, f.studentA.ID)

	// A second ACTIVE row for the same (run, student) is refused...
	otherID := uuid.New().String()
	_, err := pool.Exec(ctx, `
		INSERT INTO student_sessions (id, classroom_run_id, student_id, livekit_identity, status)
		VALUES ($3::uuid, $1, $2, $3, 'ONLINE')`, f.runID, f.studentA.ID, otherID)
	if err == nil {
		t.Fatal("the database accepted a second active session for the same student and run")
	}
	if !strings.Contains(err.Error(), "student_sessions_active_idx") {
		t.Errorf("error = %v, want the partial unique index to reject it", err)
	}

	// ...while a TERMINAL one is allowed: leaving is a decision, and a re-entry in the
	// same lesson must be recorded rather than overwritten (§50).
	if _, err := f.repo.Leave(ctx, first.ID, f.studentA.ID); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	afterLeft := uuid.New().String()
	if _, err := pool.Exec(ctx, `
		INSERT INTO student_sessions (id, classroom_run_id, student_id, livekit_identity, status)
		VALUES ($3::uuid, $1, $2, $3, 'CONNECTING')`, f.runID, f.studentA.ID, afterLeft); err != nil {
		t.Fatalf("a session after LEFT was refused: %v", err)
	}
}

// TestStatusVocabularyIsEnforced: the six V1 states are the contract between the
// database, the Go constants and the frontend; a seventh must be impossible.
func TestStatusVocabularyIsEnforced(t *testing.T) {
	pool := dbtest.Pool(t)
	f := newFixture(t)

	identity := uuid.New().String()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO student_sessions (id, classroom_run_id, student_id, livekit_identity, status)
		VALUES ($3::uuid, $1, $2, $3, 'PRE_JOIN')`, f.runID, f.studentA.ID, identity)
	if err == nil {
		t.Fatal("the database accepted PRE_JOIN, which is a frontend-only state (§12)")
	}
	if !strings.Contains(err.Error(), "student_sessions_status_valid") {
		t.Errorf("error = %v, want the status check constraint to reject it", err)
	}
}

// ---------------------------------------------------------------------------
// CreateOrReuse
// ---------------------------------------------------------------------------

// TestCreateOrReuseInsertsThenReuses is the §50 behaviour on a real database: the row
// is created once, reused on every rejoin, and keeps the identity LiveKit uses to
// replace the stale connection.
func TestCreateOrReuseInsertsThenReuses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := f.create(t, f.studentA.ID)
	if first.Status != session.StatusConnecting {
		t.Errorf("status = %s, want CONNECTING", first.Status)
	}
	if first.LiveKitIdentity != first.ID.String() {
		t.Errorf("identity = %q, want the session id %s (§44)", first.LiveKitIdentity, first.ID)
	}
	if first.ConnectedAt != nil || first.LeftAt != nil {
		t.Errorf("a new session has timestamps set: %+v", first)
	}

	second := f.create(t, f.studentA.ID)
	if second.ID != first.ID {
		t.Fatalf("second join created session %s, want the recycled %s", second.ID, first.ID)
	}
	if second.LiveKitIdentity != first.LiveKitIdentity {
		t.Errorf("identity changed across a rejoin: %q → %q", first.LiveKitIdentity, second.LiveKitIdentity)
	}
	if second.CreatedAt.UnixMilli() != first.CreatedAt.UnixMilli() {
		t.Errorf("the reused row was re-created: created_at %s → %s", first.CreatedAt, second.CreatedAt)
	}

	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM student_sessions WHERE classroom_run_id = $1 AND student_id = $2`,
		f.runID, f.studentA.ID).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("rows = %d, want exactly 1 for one student in one run", count)
	}
}

// TestCreateOrReuseReactivatesATerminalLookingState: a DISCONNECTED session is reused
// and reset, and the first-time timestamps survive the reconnect.
func TestCreateOrReuseReactivatesATerminalLookingState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := f.create(t, f.studentA.ID)
	// Promote it to ONLINE with both timestamps, then disconnect it.
	if _, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: first.ID, From: session.StatusConnecting, To: session.StatusOnline,
		MarkConnected: true, MarkScreenStarted: true,
	}); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if _, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: first.ID, From: session.StatusOnline, To: session.StatusDisconnected,
	}); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}

	rejoined := f.create(t, f.studentA.ID)
	if rejoined.ID != first.ID || rejoined.Status != session.StatusConnecting {
		t.Fatalf("rejoin = %s/%s, want the same row back in CONNECTING", rejoined.ID, rejoined.Status)
	}
	if rejoined.ConnectedAt == nil || rejoined.ScreenStartedAt == nil {
		t.Error("the first-time timestamps were erased by the rejoin")
	}
}

// TestConcurrentJoinsCollapseIntoOneSession is the reason the repository uses
// `INSERT ... ON CONFLICT DO UPDATE` instead of a read-then-write: a double click or a
// retried request must not be able to produce two sessions, and it must not fail.
func TestConcurrentJoinsCollapseIntoOneSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const racers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*session.StudentSession
		errs    []error
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored, err := f.repo.CreateOrReuse(ctx, session.CreateOrReuseParams{
				SessionID: uuid.New(), ClassroomRunID: f.runID, StudentID: f.studentA.ID,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, stored)
		}()
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("concurrent joins failed: %v", errs)
	}
	ids := map[uuid.UUID]bool{}
	for _, stored := range results {
		ids[stored.ID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("%d concurrent joins produced %d distinct sessions, want 1", racers, len(ids))
	}
}

// ---------------------------------------------------------------------------
// Leave
// ---------------------------------------------------------------------------

// TestLeaveIsIdempotentAndKeepsTheFirstTimestamp: a retried leave is a success, and the
// lesson's record does not move.
func TestLeaveIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.create(t, f.studentA.ID)

	first, err := f.repo.Leave(ctx, created.ID, f.studentA.ID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if first.Status != session.StatusLeft || first.LeftAt == nil {
		t.Fatalf("session = %+v, want LEFT with left_at", first)
	}

	time.Sleep(10 * time.Millisecond)
	second, err := f.repo.Leave(ctx, created.ID, f.studentA.ID)
	if err != nil {
		t.Fatalf("second Leave: %v", err)
	}
	if !second.LeftAt.Equal(*first.LeftAt) {
		t.Errorf("left_at moved on a repeated leave: %s → %s", first.LeftAt, second.LeftAt)
	}
}

// TestLeaveIsScopedToTheOwner: a session id that belongs to somebody else matches no
// row, and the caller cannot tell that apart from an unknown id (§58).
func TestLeaveIsScopedToTheOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.create(t, f.studentA.ID)

	if _, err := f.repo.Leave(ctx, created.ID, f.studentB.ID); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("another student's leave: err = %v, want ErrSessionNotFound", err)
	}
	if _, err := f.repo.Leave(ctx, uuid.New(), f.studentA.ID); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("unknown session: err = %v, want ErrSessionNotFound", err)
	}
	// The session is untouched by the refused calls.
	roster, err := f.repo.ListRosterByRun(ctx, f.classroomID, f.runID)
	if err != nil {
		t.Fatalf("ListRosterByRun: %v", err)
	}
	// Two authorized students, but only one of them has a session: the other is a row
	// with a nil Session, which is the whole point of the roster read.
	if len(roster) != 2 {
		t.Fatalf("roster = %+v, want the two authorized students", roster)
	}
	for _, entry := range roster {
		if entry.StudentID == f.studentA.ID {
			if entry.Session == nil || entry.Session.Status != session.StatusConnecting {
				t.Errorf("session = %+v, want the student's own session untouched", entry.Session)
			}
		} else if entry.Session != nil {
			t.Errorf("student B has a session they never created: %+v", entry.Session)
		}
	}
}

// ---------------------------------------------------------------------------
// ApplyObservation
// ---------------------------------------------------------------------------

// TestApplyObservationWritesTheTransitionAndItsTimestamps covers the three write shapes:
// first connect, screen started, and the most recent loss.
func TestApplyObservationWritesTheTransitionAndItsTimestamps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.create(t, f.studentA.ID)

	online, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: created.ID, From: session.StatusConnecting, To: session.StatusOnline,
		MarkConnected: true, MarkScreenStarted: true,
	})
	if err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if online == nil || online.Status != session.StatusOnline {
		t.Fatalf("stored = %+v, want ONLINE", online)
	}
	if online.ConnectedAt == nil || online.ScreenStartedAt == nil {
		t.Fatalf("timestamps = %+v, want connected_at and screen_started_at", online)
	}
	firstConnected := *online.ConnectedAt

	lost, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: created.ID, From: session.StatusOnline, To: session.StatusScreenLost,
		MarkScreenLost: true,
	})
	if err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if lost == nil || lost.Status != session.StatusScreenLost || lost.ScreenLostAt == nil {
		t.Fatalf("stored = %+v, want SCREEN_LOST with screen_lost_at", lost)
	}

	// Back online: connected_at must NOT move (it is the first time), and the loss is
	// kept as history.
	again, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: created.ID, From: session.StatusScreenLost, To: session.StatusOnline,
		MarkConnected: true, MarkScreenStarted: true,
	})
	if err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if !again.ConnectedAt.Equal(firstConnected) {
		t.Errorf("connected_at moved: %s → %s", firstConnected, again.ConnectedAt)
	}
	if again.ScreenLostAt == nil {
		t.Error("screen_lost_at was cleared by a restore")
	}
}

// TestApplyObservationIsCompareAndSet: the write is guarded by the status it was
// computed from, so a poll that started before a leave cannot resurrect the session.
func TestApplyObservationIsCompareAndSet(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.create(t, f.studentA.ID)
	if _, err := f.repo.Leave(ctx, created.ID, f.studentA.ID); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	// The monitor computed "CONNECTING → DISCONNECTED" before the leave landed.
	stored, err := f.repo.ApplyObservation(ctx, session.ObservationChange{
		SessionID: created.ID, From: session.StatusConnecting, To: session.StatusDisconnected,
	})
	if err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	if stored != nil {
		t.Fatalf("stored = %+v, want nil (the guard must not match)", stored)
	}
	roster, err := f.repo.ListRosterByRun(ctx, f.classroomID, f.runID)
	if err != nil {
		t.Fatalf("ListRosterByRun: %v", err)
	}
	studentAEntry := rosterEntryOf(t, roster, f.studentA.ID)
	if studentAEntry.Session == nil || studentAEntry.Session.Status != session.StatusLeft {
		t.Errorf("session = %+v, want the LEFT decision to stand", studentAEntry.Session)
	}
}

// ---------------------------------------------------------------------------
// ListRosterByRun (§29/§51)
// ---------------------------------------------------------------------------

// TestListRosterByRunIsTheRosterNotTheSessions is the Phase 7 read: the wall lists every
// authorized student, the one who never entered is a row with a nil session rather than a
// missing row, and a student who left stays visible as LEFT.
func TestListRosterByRunIsTheRosterNotTheSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Only student A enters.
	created := f.create(t, f.studentA.ID)
	if _, err := f.repo.Leave(ctx, created.ID, f.studentA.ID); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	// A session of another run must not appear.
	_, otherStudent := otherRun(t, f)

	roster, err := f.repo.ListRosterByRun(ctx, f.classroomID, f.runID)
	if err != nil {
		t.Fatalf("ListRosterByRun: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("roster = %d rows, want 2 (every authorized student)", len(roster))
	}
	names := map[uuid.UUID]string{}
	for _, entry := range roster {
		names[entry.StudentID] = entry.DisplayName
		if entry.Session != nil && entry.Session.ClassroomRunID != f.runID {
			t.Errorf("session %s belongs to run %s, not %s", entry.Session.ID, entry.Session.ClassroomRunID, f.runID)
		}
	}
	if names[f.studentA.ID] != "张三" || names[f.studentB.ID] != "李四" {
		t.Errorf("display names = %v, want the joined account names", names)
	}

	// A student who joined and then left is LEFT — not absent, and not null: leaving is
	// a decision the wall must show.
	joinedThenLeft := rosterEntryOf(t, roster, f.studentA.ID).Session
	if joinedThenLeft == nil || joinedThenLeft.Status != session.StatusLeft {
		t.Errorf("student A session = %+v, want LEFT (the wall shows that they left)", joinedThenLeft)
	}
	// A student who never pressed "进入课堂" has no session at all.
	if waiting := rosterEntryOf(t, roster, f.studentB.ID).Session; waiting != nil {
		t.Errorf("student B session = %+v, want nil (never entered)", waiting)
	}
	if _, ok := names[otherStudent]; ok {
		t.Error("a student of another classroom leaked into the wall")
	}
}

// TestListRosterByRunOrdersByAccountAndPrefersTheActiveSession covers the two rules that
// make the grid usable: the order is the account's (stable between polls, and independent
// of who joined first), and a student who left and re-entered is shown by their ACTIVE
// session rather than by the LEFT record they left behind.
func TestListRosterByRunOrdersByAccountAndPrefersTheActiveSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	users := user.NewPostgres(f.pool)
	classrooms := classroom.NewPostgres(f.pool)

	// A third authorized student. The account is random like the fixture's (a fixed one
	// would collide with a previous run of this suite) and the expected order is computed
	// from the generated values below, so the test does not depend on the draw.
	early, err := users.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("a"), DisplayName: "最早", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create third student: %v", err)
	}
	if err := classrooms.AddStudents(ctx, f.classroomID, []uuid.UUID{early.ID}, f.teacher.ID); err != nil {
		t.Fatalf("add third student: %v", err)
	}
	// The grant has to go before the account can (classroom_students → users is
	// ON DELETE RESTRICT), and this cleanup runs before the fixture's own.
	t.Cleanup(func() {
		if _, err := f.pool.Exec(ctx, `DELETE FROM classroom_students WHERE student_id = $1`, early.ID); err != nil {
			t.Logf("cleanup: grant of %s: %v", early.ID, err)
		}
		if _, err := f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, early.ID); err != nil {
			t.Logf("cleanup: account %s: %v", early.ID, err)
		}
	})

	// Student A joins, leaves, and joins again: two rows in the same run, one terminal.
	first := f.create(t, f.studentA.ID)
	if _, err := f.repo.Leave(ctx, first.ID, f.studentA.ID); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	second := f.create(t, f.studentA.ID)
	if second.ID == first.ID {
		t.Fatal("the re-entry reused the LEFT session, want a new row")
	}

	roster, err := f.repo.ListRosterByRun(ctx, f.classroomID, f.runID)
	if err != nil {
		t.Fatalf("ListRosterByRun: %v", err)
	}
	if len(roster) != 3 {
		t.Fatalf("roster = %d rows, want 3 (one per authorized student)", len(roster))
	}
	// The expected order is computed from the accounts that were actually generated: the
	// fixture's accounts are random, so hard-coding them would assert the test's luck.
	// The comparison lowercases, mirroring the citext column the ORDER BY runs on.
	type member struct {
		id      uuid.UUID
		account string
	}
	members := []member{
		{early.ID, early.Account}, {f.studentA.ID, f.studentA.Account}, {f.studentB.ID, f.studentB.Account},
	}
	sort.Slice(members, func(i, j int) bool {
		left, right := strings.ToLower(members[i].account), strings.ToLower(members[j].account)
		if left != right {
			return left < right
		}
		return members[i].id.String() < members[j].id.String()
	})
	for i, want := range members {
		if roster[i].StudentID != want.id {
			t.Errorf("row %d = %s, want %s (account %s)", i, roster[i].StudentID, want.id, want.account)
		}
	}
	active := rosterEntryOf(t, roster, f.studentA.ID).Session
	if active == nil || active.ID != second.ID || active.Status != session.StatusConnecting {
		t.Errorf("student A session = %+v, want the ACTIVE re-entry %s", active, second.ID)
	}
}

// rosterEntryOf finds one student's row in a roster read.
func rosterEntryOf(t *testing.T, roster []session.RosterEntry, studentID uuid.UUID) session.RosterEntry {
	t.Helper()
	for _, entry := range roster {
		if entry.StudentID == studentID {
			return entry
		}
	}
	t.Fatalf("student %s is missing from the roster: %+v", studentID, roster)
	return session.RosterEntry{}
}

// otherRun creates a second open run (a second classroom, so the one-open-run index
// stays satisfied) and returns its id with the student whose session was created in it.
func otherRun(t *testing.T, f *fixture) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	users := user.NewPostgres(f.pool)
	classrooms := classroom.NewPostgres(f.pool)

	student, err := users.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("student"), DisplayName: "王五", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create third student: %v", err)
	}
	other, err := classrooms.Create(ctx, classroom.CreateParams{
		OwnerTeacherID: f.teacher.ID, Name: "其他课堂",
	})
	if err != nil {
		t.Fatalf("create second classroom: %v", err)
	}
	runID := uuid.New()
	if _, _, err := classrooms.Open(ctx, classroom.OpenParams{
		ClassroomID: other.ID, TeacherID: f.teacher.ID, RunID: runID, RoomName: classroom.RoomName(runID),
	}); err != nil {
		t.Fatalf("open second classroom: %v", err)
	}
	t.Cleanup(func() { cleanup(t, f.pool, student.ID) })
	if _, err := f.repo.CreateOrReuse(ctx, session.CreateOrReuseParams{
		SessionID: uuid.New(), ClassroomRunID: runID, StudentID: student.ID,
	}); err != nil {
		t.Fatalf("create session in the second run: %v", err)
	}
	return runID, student.ID
}

// TestRepositoryWithoutAPoolFailsLoudly: a nil repository must return an error rather
// than panic, because the API can run in a degraded mode where a service is wired but
// nothing is connected.
func TestRepositoryWithoutAPoolFailsLoudly(t *testing.T) {
	repo := session.NewPostgres(nil)
	ctx := context.Background()
	if _, err := repo.CreateOrReuse(ctx, session.CreateOrReuseParams{SessionID: uuid.New()}); err == nil {
		t.Error("CreateOrReuse on a nil pool succeeded")
	}
	if _, err := repo.Leave(ctx, uuid.New(), uuid.New()); err == nil {
		t.Error("Leave on a nil pool succeeded")
	}
	if _, err := repo.ListRosterByRun(ctx, uuid.New(), uuid.New()); err == nil {
		t.Error("ListRosterByRun on a nil pool succeeded")
	}
	if _, err := repo.ApplyObservation(ctx, session.ObservationChange{SessionID: uuid.New()}); err == nil {
		t.Error("ApplyObservation on a nil pool succeeded")
	}
}
