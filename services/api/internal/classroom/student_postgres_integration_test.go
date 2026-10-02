package classroom_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The student read path against a real PostgreSQL server (TEST_DATABASE_URL); the
// whole file is skipped without it (dbtest.URL).
//
// These tests exist because the AUTHORIZATION LIVES IN SQL. The service tests use a
// fake that filters by a map, which proves the rules but cannot prove that the
// production query joins classroom_students on the caller's own id — a repository
// that read every classroom and let the service (or the handler, or the browser)
// filter would pass every unit test in this repository and leak the school's
// timetable. So the subject here is the JOIN: who it returns, who it excludes, and
// which columns it projects.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// studentReadFixture is two teachers, two students and classrooms spread across both
// teachers, with only some of them granted.
type studentReadFixture struct {
	*fixture // repo, service, pool and the first teacher
	other    *user.User
	studentA *user.User
	studentB *user.User
	// classroomOfTeacher is a classroom of the OTHER teacher, granted to nobody in
	// this fixture: it must never appear in a student's response.
	classroomOfTeacher *classroom.Classroom
}

func newStudentReadFixture(t *testing.T) *studentReadFixture {
	t.Helper()
	base := newFixture(t)
	f := &studentReadFixture{
		fixture:  base,
		other:    createStaff(t, base.pool, base.users, user.RoleTeacher),
		studentA: createStudent(t, base.pool, base.users, user.StatusActive),
		studentB: createStudent(t, base.pool, base.users, user.StatusActive),
	}
	f.classroomOfTeacher = createClassroomOwnedBy(t, base.service, f.other.ID, "别人的课堂")
	return f
}

// createClassroomOwnedBy creates a CLOSED classroom owned by an arbitrary teacher,
// which the shared fixture's createClassroom cannot do (it always uses its own).
func createClassroomOwnedBy(t *testing.T, service *classroom.Service, ownerID uuid.UUID, name string) *classroom.Classroom {
	t.Helper()
	created, err := service.Create(context.Background(), classroom.CreateInput{TeacherID: ownerID, Name: name})
	if err != nil {
		t.Fatalf("Create(%q) for %s: %v", name, ownerID, err)
	}
	return created
}

// grant adds the accounts to the classroom's roster through the service, which is
// what a teacher's import does (§11).
func (f *studentReadFixture) grant(t *testing.T, c *classroom.Classroom, accounts ...string) {
	t.Helper()
	result, err := f.service.AddStudents(context.Background(), classroom.AddStudentsInput{
		ClassroomID: c.ID,
		TeacherID:   c.OwnerTeacherID,
		Accounts:    accounts,
	})
	if err != nil {
		t.Fatalf("AddStudents(%s): %v", c.ID, err)
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("AddStudents(%s) rejected %v", c.ID, result.Rejected)
	}
}

func (f *studentReadFixture) open(t *testing.T, c *classroom.Classroom) *classroom.Run {
	t.Helper()
	_, run, err := f.service.Open(context.Background(), c.ID, c.OwnerTeacherID)
	if err != nil {
		t.Fatalf("Open(%s): %v", c.ID, err)
	}
	return run
}

// visibleMismatch compares the student's visible set with the expected one.
//
// WHY a set comparison and not a length: the tests share one database, so other
// packages' classrooms exist in `classrooms` at the same time. What must hold for
// THIS student is exact — every classroom he is entitled to, and no other — while
// the table as a whole is nobody's business. Comparing the sets is also what makes
// "the other teacher's classroom and the ungranted one do not show up" an assertion
// rather than an accident of the fixture.
func visibleMismatch(got []classroom.StudentClassroom, want map[uuid.UUID]bool) string {
	seen := map[uuid.UUID]bool{}
	for _, c := range got {
		seen[c.ID] = true
		if !want[c.ID] {
			return "a classroom the student is not authorized for was returned: " + c.ID.String()
		}
	}
	for id, expected := range want {
		if expected && !seen[id] {
			return "an authorized classroom is missing from the list: " + id.String()
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Membership
// ---------------------------------------------------------------------------

// TestStudentReadAuthorizesInSQL is §14 as an executable rule: a student's list is
// exactly the classrooms they hold a grant for.
func TestStudentReadAuthorizesInSQL(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	mine := f.createClassroom(t, "C++ 算法训练")
	f.grant(t, mine, f.studentA.Account)
	// Granted to the OTHER student only: invisible to A.
	theirs := createClassroomOwnedBy(t, f.service, f.other.ID, "别人的课堂")
	f.grant(t, theirs, f.studentB.Account)

	// --- A sees exactly one classroom ---
	got, err := f.service.ListStudentClassrooms(ctx, f.studentA.ID)
	if err != nil {
		t.Fatalf("ListStudentClassrooms(A) = %v", err)
	}
	if mismatch := visibleMismatch(got, map[uuid.UUID]bool{mine.ID: true}); mismatch != "" {
		t.Fatalf("%s (list = %v)", mismatch, classroomIDs(got))
	}
	if f.classroomOfTeacher.ID == got[0].ID {
		t.Error("a classroom of another teacher leaked into the student's list")
	}

	// --- and nothing else on the same rows ---
	view := got[0]
	if view.TeacherDisplayName != f.teacher.DisplayName {
		t.Errorf("teacher display name = %q, want %q (the JOIN must read the OWNER's name)",
			view.TeacherDisplayName, f.teacher.DisplayName)
	}
	if view.Status != classroom.StatusClosed {
		t.Errorf("status = %s, want CLOSED for a classroom nobody opened", view.Status)
	}
	if view.CurrentRun != nil {
		t.Errorf("currentRun = %+v, want nil before the first open", view.CurrentRun)
	}

	// --- the detail read agrees with the list ---
	found, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, mine.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom(A, mine) = %v", err)
	}
	if found.ID != mine.ID || found.Name != mine.Name {
		t.Errorf("detail = %s/%q, want %s/%q", found.ID, found.Name, mine.ID, mine.Name)
	}

	// --- an authorized classroom of somebody else stays invisible to the others ---
	if _, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, theirs.ID); !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Errorf("A reading B's classroom: err = %v, want ErrStudentNotAssigned", err)
	}
	if _, err := f.service.GetStudentClassroom(ctx, f.studentB.ID, mine.ID); !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Errorf("B reading A's classroom: err = %v, want ErrStudentNotAssigned", err)
	}
	// The classroom that exists but is granted to nobody.
	if _, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, f.classroomOfTeacher.ID); !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Errorf("A reading an ungranted classroom: err = %v, want ErrStudentNotAssigned", err)
	}
}

// TestStudentReadCannotDistinguishMissingFromForbidden is the §14 anti-enumeration
// rule: the database answers both questions with the same empty result, so no layer
// above it could tell a student that a given classroom id exists.
func TestStudentReadCannotDistinguishMissingFromForbidden(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	forbidden := f.createClassroom(t, "不属于这个学生的课堂")
	unknown := uuid.New()

	_, errForbidden := f.service.GetStudentClassroom(ctx, f.studentA.ID, forbidden.ID)
	_, errUnknown := f.service.GetStudentClassroom(ctx, f.studentA.ID, unknown)
	if !errors.Is(errForbidden, classroom.ErrStudentNotAssigned) || !errors.Is(errUnknown, classroom.ErrStudentNotAssigned) {
		t.Fatalf("errors = (%v, %v), want ErrStudentNotAssigned for both", errForbidden, errUnknown)
	}
	if errForbidden.Error() != errUnknown.Error() {
		t.Errorf("the two cases produced different errors (%q vs %q), which is what lets ids be enumerated",
			errForbidden, errUnknown)
	}
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// TestStudentReadPutsOpenClassroomsFirst: the portal's first need is "which lesson
// can I enter now?" (§14), and the order has to be stable so the list does not jump
// between refreshes.
func TestStudentReadPutsOpenClassroomsFirst(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	closedNewest := f.createClassroom(t, "最新的未开启课堂")
	closedOlder := f.createClassroom(t, "较早的未开启课堂")
	openOldest := f.createClassroom(t, "最早开启的课堂")
	openNewer := f.createClassroom(t, "稍后开启的课堂")
	for _, c := range []*classroom.Classroom{closedNewest, closedOlder, openOldest, openNewer} {
		f.grant(t, c, f.studentA.Account)
	}
	// created_at defaults to now(), which is the TRANSACTION timestamp: these four
	// rows share one value, and the id tiebreaker would then decide the order by
	// luck. Setting the timestamps explicitly is what makes "newest first" an
	// assertion about the query instead of about a UUID comparison. (The tiebreaker
	// itself is exercised by the seeding path in production, where created_at is a
	// transaction timestamp for real.)
	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	for i, c := range []*classroom.Classroom{closedOlder, openOldest, openNewer, closedNewest} {
		if _, err := f.pool.Exec(ctx, `UPDATE classrooms SET created_at = $2 WHERE id = $1`,
			c.ID, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("set created_at of %s: %v", c.ID, err)
		}
	}
	// Both statuses are represented and the OPEN ones were created FIRST, so an
	// implementation that ordered by created_at alone would return them last.
	runOldest := f.open(t, openOldest)
	runNewer := f.open(t, openNewer)

	got, err := f.service.ListStudentClassrooms(ctx, f.studentA.ID)
	if err != nil {
		t.Fatalf("ListStudentClassrooms(A) = %v", err)
	}
	want := map[uuid.UUID]bool{closedNewest.ID: true, closedOlder.ID: true, openOldest.ID: true, openNewer.ID: true}
	if mismatch := visibleMismatch(got, want); mismatch != "" {
		t.Fatalf("%s (list = %v)", mismatch, classroomIDs(got))
	}
	if len(got) != 4 {
		t.Fatalf("got %d classrooms, want the student's 4 (%v)", len(got), classroomIDs(got))
	}

	// OPEN before CLOSED — the whole point of the ordering — and CLOSED still
	// present, which is §14's "暂不可进入" card.
	for i, c := range got {
		if i < 2 && c.Status != classroom.StatusOpen {
			t.Errorf("position %d is %s, want OPEN first (order = %s)", i, c.Status, statuses(got))
		}
		if i >= 2 && c.Status != classroom.StatusClosed {
			t.Errorf("position %d is %s, want CLOSED last (order = %s)", i, c.Status, statuses(got))
		}
	}
	// Inside a status group: newest first, id DESC as the tiebreaker.
	if got[0].ID != openNewer.ID || got[1].ID != openOldest.ID {
		t.Errorf("OPEN group = %v, want [%s %s] (created_at DESC, id DESC)",
			classroomIDs(got[:2]), openNewer.ID, openOldest.ID)
	}
	if got[2].ID != closedNewest.ID || got[3].ID != closedOlder.ID {
		t.Errorf("CLOSED group = %v, want [%s %s]", classroomIDs(got[2:]), closedNewest.ID, closedOlder.ID)
	}

	// currentRun: the run of THIS open, and nothing invented.
	if got[0].CurrentRun == nil {
		t.Fatal("an OPEN classroom must carry its current run")
	}
	if got[0].CurrentRun.ID != runNewer.ID || !got[0].CurrentRun.OpenedAt.Equal(runNewer.OpenedAt) {
		t.Errorf("currentRun = %+v, want %s opened at %s", got[0].CurrentRun, runNewer.ID, runNewer.OpenedAt)
	}
	if got[1].CurrentRun == nil || got[1].CurrentRun.ID != runOldest.ID {
		t.Errorf("older OPEN classroom currentRun = %+v, want %s", got[1].CurrentRun, runOldest.ID)
	}
	for _, c := range got[2:] {
		if c.CurrentRun != nil {
			t.Errorf("CLOSED classroom %s reports a current run: %+v", c.ID, c.CurrentRun)
		}
	}

	// Reopening replaces the run rather than reusing it (§8) — visible to the student
	// as a new openedAt, which is why the field is part of the DTO.
	if _, _, err := f.service.Close(ctx, openNewer.ID, f.teacher.ID); err != nil {
		t.Fatalf("Close(openNewer): %v", err)
	}
	reopened := f.open(t, openNewer)
	afterReopen, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, openNewer.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom after reopen: %v", err)
	}
	if afterReopen.CurrentRun == nil || afterReopen.CurrentRun.ID != reopened.ID {
		t.Errorf("currentRun = %+v, want the newly created run %s", afterReopen.CurrentRun, reopened.ID)
	}
	if afterReopen.CurrentRun.ID == runNewer.ID {
		t.Error("the previous run was reused across a close/open cycle (§8 forbids it)")
	}
}

// ---------------------------------------------------------------------------
// The SQL projection
// ---------------------------------------------------------------------------

// TestStudentReadResponseCannotContainAnotherStudent is §26 as a database-level
// assertion: the query that feeds the student DTO has no column that could describe
// another student, so no handler change can leak one.
//
// It works on the marshalled JSON, not on the Go struct: what reaches a browser is
// the JSON, and a struct field with a json tag is what a client sees even when a
// test asserts the struct is "fine".
func TestStudentReadResponseCannotContainAnotherStudent(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	shared := f.createClassroom(t, "两个学生共同的课堂")
	f.grant(t, shared, f.studentA.Account, f.studentB.Account)

	got, err := f.service.ListStudentClassrooms(ctx, f.studentA.ID)
	if err != nil {
		t.Fatalf("ListStudentClassrooms(A) = %v", err)
	}
	if mismatch := visibleMismatch(got, map[uuid.UUID]bool{shared.ID: true}); mismatch != "" {
		t.Fatalf("%s (list = %v)", mismatch, classroomIDs(got))
	}

	// marshalled renders the two reads exactly as the handler will.
	marshalled := func(t *testing.T, value any) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(encoded)
	}

	listJSON := marshalled(t, got)
	detailJSON := marshalled(t, got[0])

	// Positive control: the response really is about the shared classroom, so a
	// "nothing forbidden is present" assertion cannot pass by returning nothing.
	if !strings.Contains(listJSON, shared.ID.String()) {
		t.Fatalf("the fixture did not produce the expected row: %s", listJSON)
	}
	for _, forbidden := range []string{
		"StudentCount",
		"studentCount",
		"Students",
		"student_id",
		"Account",
		f.studentB.ID.String(),
		f.studentB.Account,
		f.studentB.DisplayName,
	} {
		if strings.Contains(listJSON, forbidden) {
			t.Errorf("the student LIST response contains %q: %s", forbidden, listJSON)
		}
		if strings.Contains(detailJSON, forbidden) {
			t.Errorf("the student DETAIL response contains %q: %s", forbidden, detailJSON)
		}
	}

	// The projection is closed: exactly the members of the frozen student DTO. A
	// renamed leak ("participants", "peerCount") would be caught here even though the
	// substring checks above would not know its name.
	allowed := map[string]bool{
		"ID": true, "Name": true, "Description": true, "Status": true,
		"TeacherDisplayName": true, "CurrentRun": true, "CreatedAt": true,
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(listJSON), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, row := range decoded {
		for key := range row {
			if !allowed[key] {
				t.Errorf("the student classroom type has a member the isolation rule did not account for: %q", key)
			}
		}
	}

	// And the second student, who is on the same roster, still cannot read it through
	// the detail endpoint owned by A's grant.
	if _, err := f.service.GetStudentClassroom(ctx, f.studentB.ID, shared.ID); err != nil {
		t.Errorf("B is on the roster of the same classroom and must be able to read it: %v", err)
	}

	// The teacher's own view still carries the roster size — the isolation is a
	// property of the STUDENT type, not a removal of the teacher's data.
	teacherView, err := f.service.List(ctx, f.teacher.ID)
	if err != nil {
		t.Fatalf("teacher List() = %v", err)
	}
	for _, c := range teacherView {
		if c.ID == shared.ID && c.StudentCount != 2 {
			t.Errorf("teacher studentCount = %d, want 2 (the teacher view must be unaffected)", c.StudentCount)
		}
	}
}

// TestStudentReadColumnsAreLive closes the remaining gap between "the type has the
// right fields" and "the SQL fills them": each derived column is compared with the
// row it comes from, read back with a separate query.
func TestStudentReadColumnsAreLive(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	description := "每周三晚自习，请提前十分钟进入"
	created, err := f.service.Create(ctx, classroom.CreateInput{
		TeacherID:   f.teacher.ID,
		Name:        "有描述的课堂",
		Description: &description,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.grant(t, created, f.studentA.Account)
	run := f.open(t, created)

	got, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, created.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom: %v", err)
	}
	if got.Name != created.Name {
		t.Errorf("name = %q, want %q", got.Name, created.Name)
	}
	if got.Description == nil || *got.Description != description {
		t.Errorf("description = %v, want %q", got.Description, description)
	}
	if got.Status != classroom.StatusOpen {
		t.Errorf("status = %s, want OPEN", got.Status)
	}
	if got.TeacherDisplayName != f.teacher.DisplayName {
		t.Errorf("teacher display name = %q, want %q", got.TeacherDisplayName, f.teacher.DisplayName)
	}
	if got.CurrentRun == nil || got.CurrentRun.ID != run.ID {
		t.Fatalf("currentRun = %+v, want %s", got.CurrentRun, run.ID)
	}
	// The timestamp is compared to the RUN row, not to the clock: a test that reads
	// the clock is a test that fails on a slow machine.
	var storedOpenedAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT opened_at FROM classroom_runs WHERE id = $1`, run.ID).Scan(&storedOpenedAt); err != nil {
		t.Fatalf("read the stored run: %v", err)
	}
	if !got.CurrentRun.OpenedAt.Equal(storedOpenedAt) {
		t.Errorf("openedAt = %s, want the stored %s", got.CurrentRun.OpenedAt, storedOpenedAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("createdAt is missing")
	}

	// A description that was never set arrives as nil, never as an empty string: the
	// two would render differently and only one of them is "no description".
	withoutDescription := f.createClassroom(t, "没有描述的课堂")
	f.grant(t, withoutDescription, f.studentA.Account)
	plain, err := f.service.GetStudentClassroom(ctx, f.studentA.ID, withoutDescription.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom(no description): %v", err)
	}
	if plain.Description != nil {
		t.Errorf("description = %q, want nil", *plain.Description)
	}
}

// TestDisabledStudentGrantStillListsTheClassroom documents a deliberate choice: the
// grant decides visibility, the ACCOUNT's status decides whether the student can log
// in at all (RequireSession answers ACCOUNT_DISABLED). A disabled account is filtered
// one layer above this query, so a re-enabled student finds their classrooms again
// without an administrator re-importing them.
//
// The account is disabled AFTER the grant, which is the only order that exists in
// production: AddStudents refuses to grant a disabled account (ACCOUNT_DISABLED), and
// the later disable is what this test is about.
func TestDisabledStudentGrantStillListsTheClassroom(t *testing.T) {
	f := newStudentReadFixture(t)
	ctx := context.Background()

	student := createStudent(t, f.pool, f.users, user.StatusActive)
	c := f.createClassroom(t, "停用学生仍然在名单里")
	f.grant(t, c, student.Account)

	if _, err := f.pool.Exec(ctx, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, student.ID); err != nil {
		t.Fatalf("disable the account: %v", err)
	}

	got, err := f.service.ListStudentClassrooms(ctx, student.ID)
	if err != nil {
		t.Fatalf("ListStudentClassrooms(disabled) = %v", err)
	}
	if mismatch := visibleMismatch(got, map[uuid.UUID]bool{c.ID: true}); mismatch != "" {
		t.Fatalf("%s (list = %v)", mismatch, classroomIDs(got))
	}
}

// classroomIDs renders the ids of a result for failure messages. Names are left out
// on purpose: they are user content, and a failing assertion does not need them.
func classroomIDs(classrooms []classroom.StudentClassroom) []string {
	out := make([]string, 0, len(classrooms))
	for _, c := range classrooms {
		out = append(out, c.ID.String()[:8])
	}
	return out
}

func statuses(classrooms []classroom.StudentClassroom) []string {
	out := make([]string, 0, len(classrooms))
	for _, c := range classrooms {
		out = append(out, string(c.Status))
	}
	return out
}
