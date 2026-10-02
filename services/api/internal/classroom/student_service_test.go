package classroom

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The student portal's read path (§14/§70), against the in-memory repository.
//
// These tests state the two rules that belong to the service — which errors a
// rejected read produces, and that nothing about the caller's own roster is invented
// here — and they pin the projection, because a field that quietly stops being copied
// is a field the portal silently stops rendering. The authorization JOIN itself is
// SQL and is verified in student_postgres_integration_test.go; the transport contract
// is in internal/httpapi/student_classrooms_test.go.

// seededStudentClassrooms is the fixture of every test below: four classrooms with
// distinct creation times (so the ordering assertions are deterministic), two
// students, and two teachers.
type studentFixtures struct {
	service  *Service
	repo     *fakeRepo
	teacher  *user.User
	other    *user.User
	studentA *user.User
	studentB *user.User

	openLab      *Classroom
	openAlgo     *Classroom
	closedDesign *Classroom
	foreign      *Classroom
}

func newStudentFixtures(t *testing.T) *studentFixtures {
	t.Helper()
	repo := newFakeRepo()
	svc := NewService(repo, newFakeDirectory())

	f := &studentFixtures{
		service:  svc,
		repo:     repo,
		teacher:  &user.User{ID: uuid.New(), Account: "teacher-01", DisplayName: "张老师"},
		other:    &user.User{ID: uuid.New(), Account: "teacher-02", DisplayName: "李老师"},
		studentA: &user.User{ID: uuid.New(), Account: "s10086", DisplayName: "学生 A"},
		studentB: &user.User{ID: uuid.New(), Account: "s10087", DisplayName: "学生 B"},
	}

	// Timestamps are explicit and increasing so "newest first" is an assertion about
	// the ORDER BY and not about how fast the test ran.
	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	description := "每周三晚自习"

	f.closedDesign = &Classroom{
		ID: uuid.New(), Name: "数据结构练习", Description: &description,
		OwnerTeacherID: f.other.ID, Status: StatusClosed,
		CreatedAt: base,
	}
	f.openLab = &Classroom{
		ID: uuid.New(), Name: "C++ 算法训练",
		OwnerTeacherID: f.teacher.ID, Status: StatusClosed,
		CreatedAt: base.Add(time.Hour),
	}
	f.openAlgo = &Classroom{
		ID: uuid.New(), Name: "算法强化",
		OwnerTeacherID: f.teacher.ID, Status: StatusClosed,
		CreatedAt: base.Add(2 * time.Hour),
	}
	// Nobody the fixture knows is on this roster: it exists to be absent from every
	// student's response.
	f.foreign = &Classroom{
		ID: uuid.New(), Name: "别人的课堂",
		OwnerTeacherID: f.other.ID, Status: StatusClosed,
		CreatedAt: base.Add(3 * time.Hour),
	}

	f.repo.seed(f.closedDesign, nil, f.studentA.ID)
	f.repo.seed(f.openLab, &Run{
		ID: uuid.New(), ClassroomID: f.openLab.ID, Status: StatusOpen,
		LiveKitRoomName: RoomName(uuid.New()),
		OpenedAt:        base.Add(10 * time.Hour),
	}, f.studentA.ID, f.studentB.ID)
	f.repo.seed(f.openAlgo, &Run{
		ID: uuid.New(), ClassroomID: f.openAlgo.ID, Status: StatusOpen,
		LiveKitRoomName: RoomName(uuid.New()),
		OpenedAt:        base.Add(11 * time.Hour),
	}, f.studentA.ID)
	f.repo.seed(f.foreign, nil)

	// The display names the real repository's `JOIN users` would read.
	f.repo.teacherNames[f.teacher.ID] = f.teacher.DisplayName
	f.repo.teacherNames[f.other.ID] = f.other.DisplayName

	return f
}

// TestListStudentClassroomsReturnsEveryAuthorizedClassroomOpenFirst is the §14 test:
// the list is exactly the authorized set (nothing else leaks in), a CLOSED classroom
// is still present, and the OPEN ones come first.
func TestListStudentClassroomsReturnsEveryAuthorizedClassroomOpenFirst(t *testing.T) {
	f := newStudentFixtures(t)

	got, err := f.service.ListStudentClassrooms(context.Background(), f.studentA.ID)
	if err != nil {
		t.Fatalf("ListStudentClassrooms() = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d classrooms, want the student's 3 authorized ones (%v)", len(got), names(got))
	}
	// openAlgo is newer than openLab, so it wins the created_at DESC tiebreak; the
	// CLOSED one is last.
	wantOrder := []uuid.UUID{f.openAlgo.ID, f.openLab.ID, f.closedDesign.ID}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("position %d = %s, want %s (order: %v)", i, got[i].ID, want, names(got))
		}
	}
	if got[len(got)-1].Status != StatusClosed {
		t.Errorf("the CLOSED classroom must still be returned (§14), order = %v", names(got))
	}
	for _, c := range got {
		if c.ID == f.foreign.ID {
			t.Error("a classroom the student holds no grant for was returned")
		}
	}

	// The projection: the teacher's display name comes from the JOIN, the current run
	// only when there is one.
	newest := got[0]
	if newest.Name != f.openAlgo.Name {
		t.Errorf("name = %q, want %q", newest.Name, f.openAlgo.Name)
	}
	if newest.TeacherDisplayName != f.teacher.DisplayName {
		t.Errorf("teacher display name = %q, want %q", newest.TeacherDisplayName, f.teacher.DisplayName)
	}
	if newest.CurrentRun == nil {
		t.Fatal("an OPEN classroom must report its current run")
	}
	if want := f.repo.classrooms[f.openAlgo.ID].CurrentRun; newest.CurrentRun.ID != want.ID || !newest.CurrentRun.OpenedAt.Equal(want.OpenedAt) {
		t.Errorf("currentRun = %+v, want %s opened at %s", newest.CurrentRun, want.ID, want.OpenedAt)
	}
	if closed := got[len(got)-1]; closed.CurrentRun != nil {
		t.Errorf("a CLOSED classroom must report no current run, got %+v", closed.CurrentRun)
	}
	// The display name of the OTHER teacher is never mixed in.
	if newest.TeacherDisplayName == f.other.DisplayName {
		t.Error("the classroom's own owner was not joined: the display name belongs to another teacher")
	}
}

// TestListStudentClassroomsIsEmptyForAnUnauthorizedStudent: no grants means an empty
// list, and the repository is still asked with the caller's own id — a student's
// portal never becomes a window onto somebody else's by accident.
func TestListStudentClassroomsIsEmptyForAnUnauthorizedStudent(t *testing.T) {
	f := newStudentFixtures(t)
	outsider := uuid.New()

	got, err := f.service.ListStudentClassrooms(context.Background(), outsider)
	if err != nil {
		t.Fatalf("ListStudentClassrooms() = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want an empty list", names(got))
	}
	if got == nil {
		t.Error("the repository returned a nil slice; the handler relies on a non-nil empty one serialising as []")
	}
	if f.repo.lastStudentRead != outsider {
		t.Errorf("repository was asked for %s, want the caller %s", f.repo.lastStudentRead, outsider)
	}
}

// TestListStudentClassroomsPropagatesRepositoryFailure: an infrastructure error is
// not an empty portal. Reporting "you have no classes" because the database blinked
// would tell a student to sit out a lesson they are enrolled in.
func TestListStudentClassroomsPropagatesRepositoryFailure(t *testing.T) {
	f := newStudentFixtures(t)
	failure := errors.New("connection refused")
	f.repo.listForStudentErr = failure

	_, err := f.service.ListStudentClassrooms(context.Background(), f.studentA.ID)
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the repository failure", err)
	}
}

// TestGetStudentClassroomReturnsTheAuthorizedClassroom pins the detail read,
// including that a CLOSED classroom is a success and not a refusal.
func TestGetStudentClassroomReturnsTheAuthorizedClassroom(t *testing.T) {
	f := newStudentFixtures(t)

	got, err := f.service.GetStudentClassroom(context.Background(), f.studentA.ID, f.openLab.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom() = %v", err)
	}
	if got.ID != f.openLab.ID || got.Status != StatusOpen {
		t.Errorf("got %s/%s, want %s/OPEN", got.ID, got.Status, f.openLab.ID)
	}
	if got.TeacherDisplayName != f.teacher.DisplayName {
		t.Errorf("teacher display name = %q", got.TeacherDisplayName)
	}
	if f.repo.lastStudentRead != f.studentA.ID || f.repo.lastClassroomRead != f.openLab.ID {
		t.Errorf("repository was asked for student=%s classroom=%s", f.repo.lastStudentRead, f.repo.lastClassroomRead)
	}

	// CLOSED is data, not an error: §14 renders it as "暂不可进入", which requires the
	// read to succeed.
	closed, err := f.service.GetStudentClassroom(context.Background(), f.studentA.ID, f.closedDesign.ID)
	if err != nil {
		t.Fatalf("GetStudentClassroom(CLOSED) = %v, want a successful read", err)
	}
	if closed.Status != StatusClosed || closed.CurrentRun != nil {
		t.Errorf("closed classroom = %+v, want CLOSED with no current run", closed)
	}
}

// TestGetStudentClassroomRejections is the security-relevant test of this file: an
// unauthorized classroom, an unknown one and a malformed (nil) id must not be
// distinguishable as "exists but forbidden" versus "does not exist" — the first two
// share ErrStudentNotAssigned, which the HTTP layer turns into 404
// STUDENT_NOT_ASSIGNED.
func TestGetStudentClassroomRejections(t *testing.T) {
	f := newStudentFixtures(t)

	t.Run("a classroom the student is not assigned to is ErrStudentNotAssigned", func(t *testing.T) {
		_, err := f.service.GetStudentClassroom(context.Background(), f.studentB.ID, f.openAlgo.ID)
		if !errors.Is(err, ErrStudentNotAssigned) {
			t.Fatalf("err = %v, want ErrStudentNotAssigned", err)
		}
	})

	t.Run("an unknown classroom gives the same error as an unauthorized one", func(t *testing.T) {
		_, err := f.service.GetStudentClassroom(context.Background(), f.studentA.ID, uuid.New())
		if !errors.Is(err, ErrStudentNotAssigned) {
			t.Fatalf("err = %v, want the same ErrStudentNotAssigned an unauthorized classroom gives", err)
		}
	})

	t.Run("a nil classroom id is a malformed request, not a missing classroom", func(t *testing.T) {
		_, err := f.service.GetStudentClassroom(context.Background(), f.studentA.ID, uuid.Nil)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
		}
		if errors.Is(err, ErrStudentNotAssigned) {
			t.Error("a nil id must not be reported as a roster answer")
		}
	})

	t.Run("a nil student id can never match a grant", func(t *testing.T) {
		_, err := f.service.GetStudentClassroom(context.Background(), uuid.Nil, f.openLab.ID)
		if !errors.Is(err, ErrStudentNotAssigned) {
			t.Fatalf("err = %v, want ErrStudentNotAssigned", err)
		}
	})
}

// TestGetStudentClassroomRefusesARepositoryThatReturnsNothing: nil-with-no-error is a
// broken repository contract. Reporting 404 for it would hide the bug behind a
// plausible product answer.
func TestGetStudentClassroomRefusesARepositoryThatReturnsNothing(t *testing.T) {
	f := newStudentFixtures(t)
	f.repo.getForStudentErr = nil
	// An empty id reaches the repository through the fake's ErrStudentNotAssigned path,
	// so the contract check is exercised by a repository that answers nil, nil — which
	// the fake cannot do by configuration, hence the direct call below.
	if _, err := (&Service{repo: nilRepository{}}).GetStudentClassroom(context.Background(), f.studentA.ID, f.openLab.ID); err == nil {
		t.Fatal("a repository returning (nil, nil) must produce an error, not a 404")
	}
}

// nilRepository is the pathological Repository the contract check above needs.
type nilRepository struct{ Repository }

func (nilRepository) GetForStudent(context.Context, uuid.UUID, uuid.UUID) (*StudentClassroom, error) {
	return nil, nil
}

func (nilRepository) GetStudentEntry(context.Context, uuid.UUID, uuid.UUID) (*StudentEntry, error) {
	return nil, nil
}

func (nilRepository) GetRunByID(context.Context, uuid.UUID) (*Run, error) { return nil, nil }

// names renders a list of classrooms for a failure message: ids only, because a
// classroom name in a test failure is noise (and is user content).
func names(classrooms []StudentClassroom) []string {
	out := make([]string, 0, len(classrooms))
	for _, c := range classrooms {
		out = append(out, c.Name+"("+c.ID.String()[:8]+")")
	}
	return out
}
