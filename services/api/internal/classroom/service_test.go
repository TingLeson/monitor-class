package classroom

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The tests in this file exercise the RULES with an in-memory repository. They are
// the tests that can state a rule precisely — "a blank name is refused", "a second
// open is ErrAlreadyOpen", "one mistyped account does not fail the import" —
// without a database, a router or a session. The SQL, the row lock and the
// constraints are covered by postgres_integration_test.go, and the transport
// contract by internal/httpapi/teacher_classrooms_test.go.

// ---------------------------------------------------------------------------
// Fake repository
// ---------------------------------------------------------------------------

type grant struct {
	studentID uuid.UUID
	addedAt   time.Time
}

// fakeRepo is an in-memory Repository that mirrors the semantics the real one
// guarantees: a classroom row with the run consistency invariant, idempotent
// grants and a run created on every open.
type fakeRepo struct {
	classrooms map[uuid.UUID]*Classroom
	runs       map[uuid.UUID]*Run
	grants     map[uuid.UUID][]grant
	students   map[uuid.UUID]Student
	// teacherNames stands in for the `JOIN users` the student read performs; see
	// seed.
	teacherNames map[uuid.UUID]string

	createErr         error
	getErr            error
	updateErr         error
	listStudentsErr   error
	addErr            error
	removeErr         error
	openErr           error
	closeErr          error
	listForStudentErr error
	getForStudentErr  error

	lastCreate     CreateParams
	lastUpdate     UpdateParams
	lastOpen       OpenParams
	lastAdded      []uuid.UUID
	lastAddedBy    uuid.UUID
	lastRemoved    uuid.UUID
	lastListOwner  uuid.UUID
	lastStudentIDs []uuid.UUID
	// lastStudentRead records the student id the two student-facing reads were
	// called with, so a test can assert the id came from the caller and not from
	// somewhere else in the classroom row.
	lastStudentRead   uuid.UUID
	lastClassroomRead uuid.UUID

	callCount int
	clock     time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		classrooms:   map[uuid.UUID]*Classroom{},
		runs:         map[uuid.UUID]*Run{},
		grants:       map[uuid.UUID][]grant{},
		students:     map[uuid.UUID]Student{},
		teacherNames: map[uuid.UUID]string{},
		clock:        time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC),
	}
}

// tick returns a distinct, increasing timestamp for every write so ordering
// assertions are deterministic instead of depending on how fast the test runs.
func (f *fakeRepo) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

// seed inserts a classroom directly, bypassing the rules (like manual SQL would).
//
// It also records a display name for the owner, because the real repository's
// student read JOINS users for it: without one, every student-view assertion would
// compare against an empty name and could not tell a correctly joined row from a
// missing join.
func (f *fakeRepo) seed(c *Classroom, run *Run, studentIDs ...uuid.UUID) {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = f.tick()
	}
	c.UpdatedAt = c.CreatedAt
	f.classrooms[c.ID] = c
	if c.OwnerTeacherID != uuid.Nil {
		if _, known := f.teacherNames[c.OwnerTeacherID]; !known {
			f.teacherNames[c.OwnerTeacherID] = "老师 " + c.OwnerTeacherID.String()[:8]
		}
	}
	if run != nil {
		f.runs[run.ID] = run
		c.CurrentRunID = &run.ID
		c.CurrentRun = run
		c.Status = StatusOpen
	}
	for _, id := range studentIDs {
		f.grants[c.ID] = append(f.grants[c.ID], grant{studentID: id, addedAt: f.tick()})
	}
}

func (f *fakeRepo) Create(_ context.Context, params CreateParams) (*Classroom, error) {
	f.callCount++
	f.lastCreate = params
	if f.createErr != nil {
		return nil, f.createErr
	}
	c := &Classroom{
		ID:             uuid.New(),
		Name:           params.Name,
		Description:    params.Description,
		OwnerTeacherID: params.OwnerTeacherID,
		Status:         StatusClosed,
		CreatedAt:      f.tick(),
	}
	c.UpdatedAt = c.CreatedAt
	f.classrooms[c.ID] = c
	copied := *c
	return &copied, nil
}

func (f *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*Classroom, error) {
	f.callCount++
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.classrooms[id]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *c
	copied.StudentCount = len(f.grants[id])
	if c.CurrentRun != nil {
		run := *c.CurrentRun
		copied.CurrentRun = &run
	}
	return &copied, nil
}

func (f *fakeRepo) ListByOwner(_ context.Context, ownerID uuid.UUID) ([]Classroom, error) {
	f.callCount++
	f.lastListOwner = ownerID
	out := []Classroom{}
	for _, c := range f.classrooms {
		if c.OwnerTeacherID != ownerID {
			continue
		}
		copied := *c
		copied.StudentCount = len(f.grants[c.ID])
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

func (f *fakeRepo) UpdateDetails(_ context.Context, params UpdateParams) (*Classroom, error) {
	f.callCount++
	f.lastUpdate = params
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	c, ok := f.classrooms[params.ClassroomID]
	if !ok {
		return nil, ErrNotFound
	}
	if params.Name != nil {
		c.Name = *params.Name
	}
	if params.DescriptionSet {
		c.Description = params.Description
	}
	c.UpdatedAt = f.tick()
	copied := *c
	return &copied, nil
}

func (f *fakeRepo) ListStudents(_ context.Context, classroomID uuid.UUID) ([]Student, error) {
	f.callCount++
	if f.listStudentsErr != nil {
		return nil, f.listStudentsErr
	}
	rows := make([]Student, 0, len(f.grants[classroomID]))
	for _, g := range f.grants[classroomID] {
		s, ok := f.students[g.studentID]
		if !ok {
			continue
		}
		s.AddedAt = g.addedAt
		rows = append(rows, s)
	}
	// Same ordering contract as the SQL: newest addition first, account ascending
	// as the tiebreaker (a batch shares one added_at).
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].AddedAt.Equal(rows[j].AddedAt) {
			return rows[i].AddedAt.After(rows[j].AddedAt)
		}
		return rows[i].Account < rows[j].Account
	})
	return rows, nil
}

func (f *fakeRepo) AddStudents(_ context.Context, classroomID uuid.UUID, studentIDs []uuid.UUID, addedBy uuid.UUID) error {
	f.callCount++
	f.lastAdded = append([]uuid.UUID{}, studentIDs...)
	f.lastAddedBy = addedBy
	if f.addErr != nil {
		return f.addErr
	}
	if _, ok := f.classrooms[classroomID]; !ok {
		return ErrNotFound
	}
	existing := map[uuid.UUID]bool{}
	for _, g := range f.grants[classroomID] {
		existing[g.studentID] = true
	}
	for _, id := range studentIDs {
		if existing[id] {
			// ON CONFLICT DO NOTHING: already granted is a success, and the grant's
			// original added_at is not touched.
			continue
		}
		existing[id] = true
		f.grants[classroomID] = append(f.grants[classroomID], grant{studentID: id, addedAt: f.tick()})
	}
	return nil
}

func (f *fakeRepo) RemoveStudent(_ context.Context, classroomID, studentID uuid.UUID) error {
	f.callCount++
	f.lastRemoved = studentID
	if f.removeErr != nil {
		return f.removeErr
	}
	grants := f.grants[classroomID]
	for i, g := range grants {
		if g.studentID == studentID {
			f.grants[classroomID] = append(grants[:i], grants[i+1:]...)
			return nil
		}
	}
	return ErrStudentNotAssigned
}

func (f *fakeRepo) Open(_ context.Context, params OpenParams) (*Classroom, *Run, error) {
	f.callCount++
	f.lastOpen = params
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	c, ok := f.classrooms[params.ClassroomID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	if c.OwnerTeacherID != params.TeacherID {
		return nil, nil, ErrNotOwner
	}
	if c.Status == StatusOpen {
		return nil, nil, ErrAlreadyOpen
	}
	run := &Run{
		ID:              params.RunID,
		ClassroomID:     params.ClassroomID,
		Status:          StatusOpen,
		LiveKitRoomName: params.RoomName,
		OpenedAt:        f.tick(),
	}
	f.runs[run.ID] = run
	c.Status = StatusOpen
	c.CurrentRunID = &run.ID
	c.CurrentRun = run
	c.UpdatedAt = f.tick()

	copied := *c
	runCopy := *run
	return &copied, &runCopy, nil
}

func (f *fakeRepo) Close(_ context.Context, classroomID, teacherID uuid.UUID) (*Classroom, *Run, error) {
	f.callCount++
	if f.closeErr != nil {
		return nil, nil, f.closeErr
	}
	c, ok := f.classrooms[classroomID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	if c.OwnerTeacherID != teacherID {
		return nil, nil, ErrNotOwner
	}
	if c.Status == StatusClosed {
		return nil, nil, ErrAlreadyClosed
	}
	run, ok := f.runs[*c.CurrentRunID]
	if !ok {
		return nil, nil, errors.New("fake: current run is missing")
	}
	closedAt := f.tick()
	run.Status = StatusClosed
	run.ClosedAt = &closedAt
	c.Status = StatusClosed
	c.CurrentRunID = nil
	c.CurrentRun = nil
	c.UpdatedAt = f.tick()

	copied := *c
	runCopy := *run
	return &copied, &runCopy, nil
}

// ---------------------------------------------------------------------------
// Student-facing reads
// ---------------------------------------------------------------------------

// ListForStudent mirrors the authorization JOIN of the real repository: only the
// classrooms this student holds a grant for, ordered the way the SQL orders them
// (`status DESC, created_at DESC, id DESC`). Mirroring the shape — not just the
// result — is what makes the service tests below meaningful: if the fake returned
// every classroom and the service filtered, these tests would still pass while the
// production query leaked (§14).
func (f *fakeRepo) ListForStudent(_ context.Context, studentID uuid.UUID) ([]StudentClassroom, error) {
	f.callCount++
	f.lastStudentRead = studentID
	if f.listForStudentErr != nil {
		return nil, f.listForStudentErr
	}
	out := make([]StudentClassroom, 0, 4)
	for id, grants := range f.grants {
		if !hasGrant(grants, studentID) {
			continue
		}
		c, ok := f.classrooms[id]
		if !ok {
			continue
		}
		out = append(out, f.studentView(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status == StatusOpen
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// GetForStudent mirrors the same JOIN with the classroom id added to the predicate.
func (f *fakeRepo) GetForStudent(_ context.Context, studentID, classroomID uuid.UUID) (*StudentClassroom, error) {
	f.callCount++
	f.lastStudentRead = studentID
	f.lastClassroomRead = classroomID
	if f.getForStudentErr != nil {
		return nil, f.getForStudentErr
	}
	c, ok := f.classrooms[classroomID]
	if !ok || !hasGrant(f.grants[classroomID], studentID) {
		return nil, ErrStudentNotAssigned
	}
	view := f.studentView(c)
	return &view, nil
}

// GetStudentEntry mirrors the same JOIN as GetForStudent, but projects the run in
// full — including the LiveKit room name, which the portal read must never carry.
func (f *fakeRepo) GetStudentEntry(_ context.Context, studentID, classroomID uuid.UUID) (*StudentEntry, error) {
	f.callCount++
	f.lastStudentRead = studentID
	f.lastClassroomRead = classroomID
	if f.getForStudentErr != nil {
		return nil, f.getForStudentErr
	}
	c, ok := f.classrooms[classroomID]
	if !ok || !hasGrant(f.grants[classroomID], studentID) {
		return nil, ErrStudentNotAssigned
	}
	entry := StudentEntry{ClassroomID: c.ID, Status: c.Status}
	if c.CurrentRun != nil {
		run := *c.CurrentRun
		run.ClassroomID = c.ID
		entry.Run = &run
	}
	return &entry, nil
}

// GetRunByID mirrors the single-row run read by primary key.
func (f *fakeRepo) GetRunByID(_ context.Context, runID uuid.UUID) (*Run, error) {
	f.callCount++
	run, ok := f.runs[runID]
	if !ok {
		return nil, ErrNotFound
	}
	stored := *run
	return &stored, nil
}

// studentView projects a classroom the way the SQL projection does.
func (f *fakeRepo) studentView(c *Classroom) StudentClassroom {
	view := StudentClassroom{
		ID:                 c.ID,
		Name:               c.Name,
		Description:        c.Description,
		Status:             c.Status,
		TeacherDisplayName: f.teacherNames[c.OwnerTeacherID],
		CreatedAt:          c.CreatedAt,
	}
	if c.CurrentRun != nil {
		view.CurrentRun = &StudentCurrentRun{ID: c.CurrentRun.ID, OpenedAt: c.CurrentRun.OpenedAt}
	}
	return view
}

func hasGrant(grants []grant, studentID uuid.UUID) bool {
	for _, g := range grants {
		if g.studentID == studentID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fake account directory
// ---------------------------------------------------------------------------
type fakeDirectory struct {
	byAccount map[string]*user.User
	err       error
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{byAccount: map[string]*user.User{}}
}

// add registers an account that can be found case-insensitively, like citext.
func (f *fakeDirectory) add(account string, role user.Role, status user.Status) *user.User {
	u := &user.User{
		ID:          uuid.New(),
		Account:     account,
		DisplayName: string(role) + " " + account,
		Role:        role,
		Status:      status,
	}
	f.byAccount[strings.ToLower(account)] = u
	return u
}

func (f *fakeDirectory) FindByAccount(_ context.Context, account string) (*user.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.byAccount[strings.ToLower(account)]
	if !ok {
		return nil, user.ErrNotFound
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	service *Service
	repo    *fakeRepo
	dir     *fakeDirectory
	teacher *user.User
	other   *user.User
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := newFakeRepo()
	dir := newFakeDirectory()
	svc := NewService(repo, dir)
	teacher := &user.User{ID: uuid.New(), Account: "teacher-01", DisplayName: "张老师", Role: user.RoleTeacher, Status: user.StatusActive}
	other := &user.User{ID: uuid.New(), Account: "teacher-02", DisplayName: "李老师", Role: user.RoleTeacher, Status: user.StatusActive}
	return &harness{service: svc, repo: repo, dir: dir, teacher: teacher, other: other}
}

// addStudent registers an ACTIVE student account in both the directory and the
// fake repository's roster projection (the JOIN the SQL performs).
func (h *harness) addStudent(account string) *user.User {
	u := h.dir.add(account, user.RoleStudent, user.StatusActive)
	h.repo.students[u.ID] = Student{
		ID: u.ID, Account: u.Account, DisplayName: u.DisplayName, Status: u.Status,
	}
	return u
}

// mustCreate creates a classroom through the service and fails the test otherwise.
func (h *harness) mustCreate(t *testing.T, name string) *Classroom {
	t.Helper()
	c, err := h.service.Create(context.Background(), CreateInput{TeacherID: h.teacher.ID, Name: name})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateClassroomStartsClosedAndOwned(t *testing.T) {
	h := newHarness(t)
	description := "  每周三 19:00 的算法训练  "
	created, err := h.service.Create(context.Background(), CreateInput{
		TeacherID:   h.teacher.ID,
		Name:        "  C++ 晚自习 ",
		Description: &description,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Status != StatusClosed {
		t.Errorf("status = %q, want CLOSED (§7: a new classroom is never open)", created.Status)
	}
	if created.CurrentRunID != nil || created.CurrentRun != nil {
		t.Errorf("a new classroom must have no run: currentRunID=%v currentRun=%v", created.CurrentRunID, created.CurrentRun)
	}
	if created.OwnerTeacherID != h.teacher.ID {
		t.Errorf("owner = %s, want %s", created.OwnerTeacherID, h.teacher.ID)
	}
	if created.Name != "C++ 晚自习" {
		t.Errorf("name = %q, want the trimmed value", created.Name)
	}
	if created.Description == nil || *created.Description != "每周三 19:00 的算法训练" {
		t.Errorf("description = %v, want the trimmed value", created.Description)
	}
	if h.repo.lastCreate.OwnerTeacherID != h.teacher.ID {
		t.Errorf("repository saw owner %s, want the caller's id", h.repo.lastCreate.OwnerTeacherID)
	}
}

func TestCreateValidatesName(t *testing.T) {
	long := strings.Repeat("课", MaxNameLength+1)
	justRight := strings.Repeat("课", MaxNameLength)
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty", "", true},
		{"blank", "   \t ", true},
		{"newline inside", "C++\n晚自习", true},
		{"tab inside", "C++\t晚自习", true},
		{"nul byte", "C++\x00", true},
		{"too long", long, true},
		{"maximum length", justRight, false},
		{"trimmed to valid", "  算法  ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			created, err := h.service.Create(context.Background(), CreateInput{TeacherID: h.teacher.ID, Name: tc.input})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
				if InvalidMessage(err) == "" {
					t.Error("an invalid-request error must carry a message for the teacher")
				}
				if h.repo.callCount != 0 {
					t.Error("an invalid name must be rejected before the repository is touched")
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := len([]rune(created.Name)); got > MaxNameLength {
				t.Errorf("stored name has %d characters, want at most %d", got, MaxNameLength)
			}
		})
	}
}

func TestCreateValidatesDescription(t *testing.T) {
	blank := "   "
	exact := strings.Repeat("说", MaxDescriptionLength)
	tooLong := strings.Repeat("说", MaxDescriptionLength+1)
	nul := "hello\x00world"
	tests := []struct {
		name    string
		input   *string
		wantNil bool
		wantErr bool
	}{
		{"absent", nil, true, false},
		{"empty", ptr(""), true, false},
		{"blank", &blank, true, false},
		{"maximum length", &exact, false, false},
		{"too long", &tooLong, false, true},
		{"nul byte", &nul, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			created, err := h.service.Create(context.Background(), CreateInput{
				TeacherID: h.teacher.ID, Name: "算法", Description: tc.input,
			})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.wantNil && created.Description != nil {
				t.Errorf("description = %q, want nil (a blank description means 'none')", *created.Description)
			}
			if !tc.wantNil && created.Description == nil {
				t.Error("description = nil, want the stored value")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Read + ownership
// ---------------------------------------------------------------------------

func TestGetEnforcesOwnership(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	if _, err := h.service.Get(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Get by owner: %v", err)
	}
	_, err := h.service.Get(context.Background(), owned.ID, h.other.ID)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner (§37/§58: not-yours is not the same as not-found)", err)
	}
	_, err = h.service.Get(context.Background(), uuid.New(), h.teacher.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// A nil id can never match a row; it must not be treated as a wildcard.
	if _, err := h.service.Get(context.Background(), uuid.Nil, h.teacher.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil id: err = %v, want ErrNotFound", err)
	}
}

func TestListReturnsOnlyTheCallersClassrooms(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(t, "mine")
	h.repo.seed(&Classroom{ID: uuid.New(), Name: "theirs", OwnerTeacherID: h.other.ID}, nil)

	list, err := h.service.List(context.Background(), h.teacher.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "mine" {
		t.Fatalf("List returned %d classrooms (%v), want only the caller's", len(list), list)
	}
	if h.repo.lastListOwner != h.teacher.ID {
		t.Errorf("repository was asked for owner %s, want %s", h.repo.lastListOwner, h.teacher.ID)
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func TestUpdateRequiresAtLeastOneField(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	_, err := h.service.Update(context.Background(), UpdateInput{ClassroomID: owned.ID, TeacherID: h.teacher.ID})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest for a body with no fields", err)
	}
	if h.repo.lastUpdate.ClassroomID != uuid.Nil {
		t.Error("an empty PATCH must not reach the repository")
	}
}

func TestUpdateRenamesWithoutTouchingTheDescription(t *testing.T) {
	h := newHarness(t)
	original := "原始说明"
	created, err := h.service.Create(context.Background(), CreateInput{
		TeacherID: h.teacher.ID, Name: "算法", Description: &original,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newName := "算法（进阶）"
	updated, err := h.service.Update(context.Background(), UpdateInput{
		ClassroomID: created.ID, TeacherID: h.teacher.ID, Name: &newName,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != newName {
		t.Errorf("name = %q, want %q", updated.Name, newName)
	}
	if updated.Description == nil || *updated.Description != original {
		t.Errorf("description = %v, want it untouched (%q)", updated.Description, original)
	}
	if h.repo.lastUpdate.DescriptionSet {
		t.Error("a rename must not tell the repository the description was set")
	}
}

func TestUpdateClearsDescriptionWithExplicitNull(t *testing.T) {
	h := newHarness(t)
	original := "说明"
	created, err := h.service.Create(context.Background(), CreateInput{
		TeacherID: h.teacher.ID, Name: "算法", Description: &original,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := h.service.Update(context.Background(), UpdateInput{
		ClassroomID: created.ID, TeacherID: h.teacher.ID, DescriptionSet: true, Description: nil,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Description != nil {
		t.Errorf("description = %q, want nil after an explicit null", *updated.Description)
	}
}

func TestUpdateValidatesBeforeWriting(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	blank := "   "
	if _, err := h.service.Update(context.Background(), UpdateInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Name: &blank,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("blank name: err = %v, want ErrInvalidRequest", err)
	}

	tooLong := strings.Repeat("说", MaxDescriptionLength+1)
	if _, err := h.service.Update(context.Background(), UpdateInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, DescriptionSet: true, Description: &tooLong,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("long description: err = %v, want ErrInvalidRequest", err)
	}
	if h.repo.lastUpdate.ClassroomID != uuid.Nil {
		t.Error("an invalid PATCH must not reach the repository")
	}
}

func TestUpdateRejectsAnotherTeachersClassroom(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	newName := "renamed"

	_, err := h.service.Update(context.Background(), UpdateInput{
		ClassroomID: owned.ID, TeacherID: h.other.ID, Name: &newName,
	})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	if h.repo.classrooms[owned.ID].Name != "算法" {
		t.Error("another teacher's request changed the classroom")
	}
}

// ---------------------------------------------------------------------------
// Open / Close
// ---------------------------------------------------------------------------

func TestOpenCreatesANewRunOnEveryOpen(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	ctx := context.Background()

	first, firstRun, err := h.service.Open(ctx, owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if first.Status != StatusOpen {
		t.Errorf("status = %q, want OPEN", first.Status)
	}
	if first.CurrentRunID == nil || *first.CurrentRunID != firstRun.ID {
		t.Errorf("current_run_id = %v, want the new run %s (§48 step 7)", first.CurrentRunID, firstRun.ID)
	}
	if firstRun.Status != StatusOpen || firstRun.ClosedAt != nil {
		t.Errorf("run = %+v, want OPEN with no closedAt", firstRun)
	}
	if want := "lk_" + firstRun.ID.String(); h.repo.runs[firstRun.ID].LiveKitRoomName != want {
		t.Errorf("room name = %q, want %q (opaque, derived from the run id only)", h.repo.runs[firstRun.ID].LiveKitRoomName, want)
	}
	if strings.Contains(firstRun.LiveKitRoomName, h.teacher.Account) {
		t.Error("the room name must not contain an account (§8)")
	}

	if _, _, err := h.service.Open(ctx, owned.ID, h.teacher.ID); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("second Open while OPEN: err = %v, want ErrAlreadyOpen", err)
	}

	closed, closedRun, err := h.service.Close(ctx, owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.Status != StatusClosed || closed.CurrentRunID != nil {
		t.Errorf("after close: status=%q currentRunID=%v, want CLOSED and nil", closed.Status, closed.CurrentRunID)
	}
	if closedRun.ID != firstRun.ID {
		t.Errorf("closed run = %s, want the run that was open (%s)", closedRun.ID, firstRun.ID)
	}
	if closedRun.Status != StatusClosed || closedRun.ClosedAt == nil {
		t.Errorf("closed run = %+v, want CLOSED with a closedAt", closedRun)
	}

	second, secondRun, err := h.service.Open(ctx, owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if secondRun.ID == firstRun.ID {
		t.Fatal("the re-open reused the previous run; §8 forbids it")
	}
	if second.CurrentRunID == nil || *second.CurrentRunID != secondRun.ID {
		t.Errorf("current_run_id = %v, want the new run %s", second.CurrentRunID, secondRun.ID)
	}
	if _, still := h.repo.runs[firstRun.ID]; !still {
		t.Error("the previous run disappeared; history must survive a re-open (§8)")
	}
	if h.repo.runs[firstRun.ID].ClosedAt == nil {
		t.Error("the previous run lost its closedAt")
	}
}

func TestOpenIsRejectedForAnotherTeacher(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	_, _, err := h.service.Open(context.Background(), owned.ID, h.other.ID)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner (§4: only the owner may open)", err)
	}
	if h.repo.classrooms[owned.ID].Status != StatusClosed {
		t.Error("the classroom was opened by a non-owner")
	}
}

func TestCloseIsRejectedWhenAlreadyClosed(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	_, _, err := h.service.Close(context.Background(), owned.ID, h.teacher.ID)
	if !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("err = %v, want ErrAlreadyClosed", err)
	}
}

func TestCloseIsRejectedForAnotherTeacher(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	if _, _, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, _, err := h.service.Close(context.Background(), owned.ID, h.other.ID)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	if h.repo.classrooms[owned.ID].Status != StatusOpen {
		t.Error("a non-owner closed the classroom")
	}
}

// TestOpenMintsAnOpaqueRoomNameForEveryRun pins the naming rule of §8 at the point
// where it is decided, not only in the SQL.
func TestOpenMintsAnOpaqueRoomNameForEveryRun(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "C++ 晚自习")
	student := h.addStudent("s10086")

	_, run, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if run.LiveKitRoomName != "lk_"+run.ID.String() {
		t.Errorf("room name = %q, want lk_<run_uuid>", run.LiveKitRoomName)
	}
	for _, forbidden := range []string{"C++", "晚自习", student.Account, h.teacher.Account, h.teacher.DisplayName} {
		if strings.Contains(run.LiveKitRoomName, forbidden) {
			t.Errorf("room name %q contains %q; §8 forbids identifiable names", run.LiveKitRoomName, forbidden)
		}
	}
}

// ---------------------------------------------------------------------------
// Roster
// ---------------------------------------------------------------------------

func TestAddStudentsGrantsActiveStudents(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	alice := h.addStudent("s10001")
	bob := h.addStudent("s10002")

	result, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID,
		Accounts: []string{"  s10001 ", "S10002"},
	})
	if err != nil {
		t.Fatalf("AddStudents: %v", err)
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("rejected = %v, want none", result.Rejected)
	}
	if len(result.Students) != 2 {
		t.Fatalf("roster has %d entries, want 2 (the state AFTER the call)", len(result.Students))
	}
	if h.repo.lastAddedBy != h.teacher.ID {
		t.Errorf("added_by = %s, want the acting teacher %s", h.repo.lastAddedBy, h.teacher.ID)
	}
	granted := map[uuid.UUID]bool{}
	for _, id := range h.repo.lastAdded {
		granted[id] = true
	}
	if !granted[alice.ID] || !granted[bob.ID] {
		t.Errorf("granted = %v, want both %s and %s", granted, alice.ID, bob.ID)
	}
}

func TestAddStudentsClassifiesEveryRejection(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	active := h.addStudent("s20001")
	h.dir.add("s20002", user.RoleStudent, user.StatusDisabled)
	h.dir.add("T9001", user.RoleTeacher, user.StatusActive)
	h.dir.add("T9002", user.RoleTeacher, user.StatusDisabled)
	h.dir.add("A9001", user.RoleAdmin, user.StatusActive)

	result, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID,
		Accounts: []string{
			"s20001",       // ok
			"s99999",       // no such account
			"T9001",        // a teacher
			"T9002",        // a teacher AND disabled: the role wins (see the service)
			"A9001",        // an admin
			"s20002",       // a disabled student
			"bad account!", // not a valid account format at all
			" ",            // blank
		},
	})
	if err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	want := map[string]apperr.Code{
		"s99999":       apperr.CodeStudentNotFound,
		"T9001":        apperr.CodeNotAStudent,
		"T9002":        apperr.CodeNotAStudent,
		"A9001":        apperr.CodeNotAStudent,
		"s20002":       apperr.CodeAccountDisabled,
		"bad account!": apperr.CodeInvalidRequest,
		"":             apperr.CodeInvalidRequest,
	}
	if len(result.Rejected) != len(want) {
		t.Fatalf("rejected %d accounts (%v), want %d", len(result.Rejected), result.Rejected, len(want))
	}
	for _, rejection := range result.Rejected {
		code, ok := want[rejection.Account]
		if !ok {
			t.Errorf("unexpected rejection for %q", rejection.Account)
			continue
		}
		if rejection.Code != code {
			t.Errorf("%q: code = %s, want %s", rejection.Account, rejection.Code, code)
		}
		delete(want, rejection.Account)
	}
	for account := range want {
		t.Errorf("no rejection for %q", account)
	}
	if len(result.Students) != 1 || result.Students[0].ID != active.ID {
		t.Fatalf("roster = %v, want only the one active student", result.Students)
	}
	if len(h.repo.lastAdded) != 1 || h.repo.lastAdded[0] != active.ID {
		t.Errorf("granted = %v, want only %s (a rejected account must not be granted)", h.repo.lastAdded, active.ID)
	}
}

func TestAddStudentsIsIdempotentAndDeduplicates(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	alice := h.addStudent("s30001")
	bob := h.addStudent("s30002")

	// The same account twice, in two spellings (accounts are citext), plus one that
	// is already on the roster.
	result, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID,
		Accounts: []string{"s30001", "S30001", "s30002", "s30002"},
	})
	if err != nil {
		t.Fatalf("AddStudents: %v", err)
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("rejected = %v, want none: a repeated or already-granted account is a success", result.Rejected)
	}
	if len(result.Students) != 2 {
		t.Fatalf("roster has %d entries, want 2", len(result.Students))
	}
	if len(h.repo.lastAdded) != 2 {
		t.Errorf("granted %d ids, want 2 (de-duplicated)", len(h.repo.lastAdded))
	}

	// Re-running the identical import must not fail and must not duplicate.
	again, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID,
		Accounts: []string{"s30001", "s30002"},
	})
	if err != nil {
		t.Fatalf("second AddStudents: %v", err)
	}
	if len(again.Students) != 2 || len(again.Rejected) != 0 {
		t.Fatalf("second run: students=%v rejected=%v, want the same two and no rejections", again.Students, again.Rejected)
	}
	ids := map[uuid.UUID]bool{}
	for _, s := range again.Students {
		if ids[s.ID] {
			t.Errorf("student %s appears twice in the roster", s.ID)
		}
		ids[s.ID] = true
	}
	if !ids[alice.ID] || !ids[bob.ID] {
		t.Errorf("roster = %v, want both original students", ids)
	}
}

func TestAddStudentsBoundsTheBatch(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty list: err = %v, want ErrInvalidRequest", err)
	}

	accounts := make([]string, MaxAccountsPerRequest+1)
	for i := range accounts {
		accounts[i] = fmt.Sprintf("s%05d", i)
	}
	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: accounts,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized list: err = %v, want ErrInvalidRequest", err)
	}
	if h.repo.lastAdded != nil {
		t.Error("an oversized batch must be rejected before anything is granted")
	}
}

func TestAddStudentsIsAllowedWhileTheClassroomIsOpen(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	if _, _, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	late := h.addStudent("s40001")

	result, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{late.Account},
	})
	if err != nil {
		t.Fatalf("AddStudents while OPEN: %v", err)
	}
	if len(result.Students) != 1 {
		t.Fatalf("roster = %v, want the late student", result.Students)
	}
}

func TestAddStudentsRejectsAnotherTeachersClassroom(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	student := h.addStudent("s50001")

	_, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.other.ID, Accounts: []string{student.Account},
	})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner (§11: only the owner may add)", err)
	}
	if h.repo.lastAdded != nil {
		t.Error("a non-owner's import reached the repository")
	}
}

func TestAddStudentsPropagatesInfrastructureFailures(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	student := h.addStudent("s60001")

	// A database failure is NOT a per-account rejection: the whole call fails, so a
	// transient outage cannot look like "these accounts are wrong".
	h.dir.err = errors.New("database is down")
	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{student.Account},
	}); err == nil || errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want an infrastructure error", err)
	}
	h.dir.err = nil

	h.repo.addErr = errors.New("insert failed")
	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{student.Account},
	}); err == nil || errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want the repository error", err)
	}
}

func TestListStudentsIncludesDisabledAccounts(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	active := h.addStudent("s70001")
	disabled := h.dir.add("s70002", user.RoleStudent, user.StatusDisabled)

	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{active.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}
	// A student disabled AFTER being granted stays on the roster (the account row is
	// what changes, not the grant).
	h.repo.grants[owned.ID] = append(h.repo.grants[owned.ID], grant{studentID: disabled.ID, addedAt: h.repo.tick()})
	h.repo.students[disabled.ID] = Student{
		ID: disabled.ID, Account: disabled.Account, DisplayName: disabled.DisplayName, Status: user.StatusDisabled,
	}

	roster, err := h.service.ListStudents(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("ListStudents: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("roster = %v, want both students (a disabled account is still a member)", roster)
	}
	var found *Student
	for i := range roster {
		if roster[i].ID == disabled.ID {
			found = &roster[i]
		}
	}
	if found == nil {
		t.Fatal("the disabled student is missing from the roster")
	}
	if found.Status != user.StatusDisabled {
		t.Errorf("status = %q, want DISABLED so the console can render it", found.Status)
	}
}

func TestListStudentsRejectsAnotherTeacher(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	if _, err := h.service.ListStudents(context.Background(), owned.ID, h.other.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
}

// ---------------------------------------------------------------------------
// Remove
// ---------------------------------------------------------------------------

func TestRemoveStudentRevokesAccess(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	student := h.addStudent("s80001")
	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{student.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	if err := h.service.RemoveStudent(context.Background(), owned.ID, student.ID, h.teacher.ID); err != nil {
		t.Fatalf("RemoveStudent: %v", err)
	}
	roster, err := h.service.ListStudents(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("ListStudents: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("roster = %v, want empty", roster)
	}
}

func TestRemoveStudentNotOnTheRoster(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")

	err := h.service.RemoveStudent(context.Background(), owned.ID, uuid.New(), h.teacher.ID)
	if !errors.Is(err, ErrStudentNotAssigned) {
		t.Fatalf("err = %v, want ErrStudentNotAssigned (404, not a silent success)", err)
	}
}

func TestRemoveStudentRejectsAnotherTeacher(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	student := h.addStudent("s90001")
	if _, err := h.service.AddStudents(context.Background(), AddStudentsInput{
		ClassroomID: owned.ID, TeacherID: h.teacher.ID, Accounts: []string{student.Account},
	}); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	if err := h.service.RemoveStudent(context.Background(), owned.ID, student.ID, h.other.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	if len(h.repo.grants[owned.ID]) != 1 {
		t.Error("a non-owner removed a student")
	}
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func TestDedupeAccountsKeepsOrderAndFirstSpelling(t *testing.T) {
	got := DedupeAccounts([]string{" s1 ", "S1", "s2", "", "  ", "s2"})
	// The two blank entries collapse into one: they are the same (empty) account,
	// and the caller reports it once.
	want := []string{"s1", "s2", ""}
	if len(got) != len(want) {
		t.Fatalf("DedupeAccounts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DedupeAccounts = %q, want %q", got, want)
		}
	}
}

func TestInvalidMessageOnlyAnswersForInvalidRequests(t *testing.T) {
	if msg := InvalidMessage(invalidf("name is required")); msg != "name is required" {
		t.Errorf("InvalidMessage = %q", msg)
	}
	if msg := InvalidMessage(ErrNotFound); msg != "" {
		t.Errorf("InvalidMessage(ErrNotFound) = %q, want empty", msg)
	}
	if msg := InvalidMessage(fmt.Errorf("wrapped: %w", invalidf("bad"))); msg != "bad" {
		t.Errorf("InvalidMessage over a wrapped error = %q, want %q", msg, "bad")
	}
}

func ptr[T any](value T) *T { return &value }

// ---------------------------------------------------------------------------
// Phase 6 seams: room teardown (§49) and the join/leave reads
// ---------------------------------------------------------------------------

// fakeTerminator records the room teardown calls Close makes.
type fakeTerminator struct {
	roomNames []string
	err       error
}

func (f *fakeTerminator) TerminateRoom(_ context.Context, roomName string) error {
	f.roomNames = append(f.roomNames, roomName)
	return f.err
}

// TestCloseTerminatesTheMediaRoom is §49: the classroom is closed in the database
// first, and the media room is cleaned up afterwards.
func TestCloseTerminatesTheMediaRoom(t *testing.T) {
	h := newHarness(t)
	terminator := &fakeTerminator{}
	h.service.WithRoomTerminator(terminator)
	owned := h.mustCreate(t, "算法")
	_, run, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, _, err := h.service.Close(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(terminator.roomNames) != 1 || terminator.roomNames[0] != run.LiveKitRoomName {
		t.Fatalf("terminated rooms = %v, want [%s]", terminator.roomNames, run.LiveKitRoomName)
	}
	// The control plane decided first: the classroom is CLOSED whether or not the
	// media call happened.
	if h.repo.classrooms[owned.ID].Status != StatusClosed {
		t.Error("the classroom is not CLOSED after a close")
	}
}

// TestCloseSucceedsWhenTheRoomCannotBeTerminated is the §33 trade-off: a media-plane
// failure must not roll back, fail or even delay a control-plane decision that already
// committed.
func TestCloseSucceedsWhenTheRoomCannotBeTerminated(t *testing.T) {
	h := newHarness(t)
	terminator := &fakeTerminator{err: errors.New("livekit: delete room: connection refused")}
	h.service.WithRoomTerminator(terminator)
	owned := h.mustCreate(t, "算法")
	if _, _, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}

	closed, _, err := h.service.Close(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Close() = %v, want success despite the media failure", err)
	}
	if closed.Status != StatusClosed {
		t.Errorf("status = %s, want CLOSED", closed.Status)
	}
	if len(terminator.roomNames) != 1 {
		t.Errorf("the terminator was not called: %v", terminator.roomNames)
	}
}

// TestCloseWithoutAMediaPlane: the degraded deployment (LiveKit unreachable at boot)
// must still be able to close a classroom.
func TestCloseWithoutAMediaPlane(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	if _, _, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := h.service.Close(context.Background(), owned.ID, h.teacher.ID); err != nil {
		t.Fatalf("Close() = %v, want success without a media plane", err)
	}
}

// TestStudentEntryIsRosterAuthorized: the join read must answer "not assigned" for a
// classroom the student is not on, and must carry the room name only when a run exists.
func TestStudentEntryIsRosterAuthorized(t *testing.T) {
	// The fixture's openLab is already OPEN with a run and studentA + studentB on the
	// roster; the room name comes from the stored run, not from a re-derivation.
	f := newStudentFixtures(t)

	entry, err := f.service.StudentEntry(context.Background(), f.studentA.ID, f.openLab.ID)
	if err != nil {
		t.Fatalf("StudentEntry: %v", err)
	}
	if entry.Status != StatusOpen || entry.Run == nil {
		t.Fatalf("entry = %+v, want an OPEN classroom with its run", entry)
	}
	if entry.Run.LiveKitRoomName != f.repo.runs[entry.Run.ID].LiveKitRoomName {
		t.Errorf("room name = %q, want the stored %q", entry.Run.LiveKitRoomName, f.repo.runs[entry.Run.ID].LiveKitRoomName)
	}

	// Not on the roster: the same answer as "no such classroom" (the fixture's
	// "foreign" classroom has studentA on nobody's roster).
	if _, err := f.service.StudentEntry(context.Background(), f.studentA.ID, f.foreign.ID); !errors.Is(err, ErrStudentNotAssigned) {
		t.Errorf("unassigned student: err = %v, want ErrStudentNotAssigned", err)
	}
	// A nil id can never match a grant row.
	if _, err := f.service.StudentEntry(context.Background(), uuid.Nil, f.openLab.ID); !errors.Is(err, ErrStudentNotAssigned) {
		t.Errorf("nil student: err = %v, want ErrStudentNotAssigned", err)
	}
}

// TestStudentEntryReportsAClosedClassroom: the status is data, not an error — the join
// endpoint decides what a CLOSED classroom means (§58's 409).
func TestStudentEntryReportsAClosedClassroom(t *testing.T) {
	f := newStudentFixtures(t)
	if err := f.repo.AddStudents(context.Background(), f.closedDesign.ID, []uuid.UUID{f.studentA.ID}, f.other.ID); err != nil {
		t.Fatalf("AddStudents: %v", err)
	}

	entry, err := f.service.StudentEntry(context.Background(), f.studentA.ID, f.closedDesign.ID)
	if err != nil {
		t.Fatalf("StudentEntry: %v", err)
	}
	if entry.Status != StatusClosed || entry.Run != nil {
		t.Errorf("entry = %+v, want CLOSED with no run", entry)
	}
}

// TestRunByIDResolvesTheRoomName: the leave path needs the room of a session's run, and
// the naming scheme must not be re-implemented outside this package.
func TestRunByIDResolvesTheRoomName(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	_, run, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	found, err := h.service.RunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if found.LiveKitRoomName != run.LiveKitRoomName {
		t.Errorf("room name = %q, want %q", found.LiveKitRoomName, run.LiveKitRoomName)
	}
	if _, err := h.service.RunByID(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: err = %v, want ErrNotFound", err)
	}
	if _, err := h.service.RunByID(context.Background(), uuid.Nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("nil run: err = %v, want ErrNotFound", err)
	}
}

// TestClassroomReadCarriesTheRoomName: the classroom read used by the teacher media
// token and the monitor must resolve the room of the current run, while the HTTP DTOs
// keep it out of every response (asserted in internal/httpapi).
func TestClassroomReadCarriesTheRoomName(t *testing.T) {
	h := newHarness(t)
	owned := h.mustCreate(t, "算法")
	_, run, err := h.service.Open(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	loaded, err := h.service.Get(context.Background(), owned.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.CurrentRun == nil {
		t.Fatal("CurrentRun is nil for an OPEN classroom")
	}
	if loaded.CurrentRun.LiveKitRoomName != run.LiveKitRoomName {
		t.Errorf("room name = %q, want %q", loaded.CurrentRun.LiveKitRoomName, run.LiveKitRoomName)
	}
	if loaded.CurrentRun.Status != StatusOpen {
		t.Errorf("run status = %q, want OPEN", loaded.CurrentRun.Status)
	}
}
