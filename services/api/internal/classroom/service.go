package classroom

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Service implements the classroom use cases of §69: create, edit and list a
// teacher's classrooms, maintain the student roster, and open/close a classroom.
//
// Every method here takes the acting teacher's id and verifies ownership itself.
// That is not redundant with the route being under /teacher/**: a route group
// answers "is this caller a TEACHER?", never "is this YOUR classroom", and the
// second question is the one that decides whether a teacher can open somebody
// else's lesson (§37/§63).
type Service struct {
	repo     Repository
	accounts AccountDirectory
	// newID mints the run id. It is injectable so a test can assert the room name
	// is derived from the run id rather than invented separately.
	newID func() uuid.UUID
}

// NewService wires the service. accounts resolves submitted student accounts; it
// is the user repository in production (see AccountDirectory).
func NewService(repo Repository, accounts AccountDirectory) *Service {
	return &Service{repo: repo, accounts: accounts, newID: uuid.New}
}

// CreateInput is the request of Create.
type CreateInput struct {
	// TeacherID is the owner. It comes from the authenticated session and never
	// from the request body: accepting a client-supplied owner would let any
	// teacher create classrooms in another teacher's name.
	TeacherID   uuid.UUID
	Name        string
	Description *string
}

// UpdateInput is the request of Update.
//
// The two fields are optional, and the request must carry at least one of them.
// Name is a pointer (nil = not mentioned) while Description carries an extra flag,
// because "clear the description" (explicit null) and "leave the description
// alone" (member absent) are different requests and only one of them may wipe data.
type UpdateInput struct {
	ClassroomID    uuid.UUID
	TeacherID      uuid.UUID
	Name           *string
	DescriptionSet bool
	Description    *string
}

// AddStudentsInput is the request of AddStudents.
type AddStudentsInput struct {
	ClassroomID uuid.UUID
	TeacherID   uuid.UUID
	// Accounts are the accounts as submitted. They are trimmed and de-duplicated
	// here; each one is then either granted or returned in the result's Rejected
	// list with the code that explains why.
	Accounts []string
}

// AddStudentsResult is the outcome of a batch add.
type AddStudentsResult struct {
	// Students is the roster AFTER the operation — not just the rows this call
	// created. The caller asked "who is in this classroom now?", and answering with
	// only the delta would force the frontend to merge two lists itself, with a
	// merge bug waiting in every client.
	Students []Student
	// Rejected lists the accounts that were NOT granted, in submission order.
	Rejected []Rejection
}

// List returns the teacher's own classrooms.
//
// No pagination, by design: a teacher owns a handful of courses. See
// Repository.ListByOwner for the full trade-off.
func (s *Service) List(ctx context.Context, teacherID uuid.UUID) ([]Classroom, error) {
	return s.repo.ListByOwner(ctx, teacherID)
}

// Create makes a CLOSED classroom owned by the calling teacher.
//
// The status is not a parameter anywhere in this package: §7 fixes it at CLOSED,
// and a "create and open" shortcut would produce a classroom whose run was never
// created — the state the run-consistency constraint exists to forbid.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Classroom, error) {
	name, err := NormalizeName(in.Name)
	if err != nil {
		return nil, err
	}
	description, err := NormalizeDescription(in.Description)
	if err != nil {
		return nil, err
	}
	created, err := s.repo.Create(ctx, CreateParams{
		OwnerTeacherID: in.TeacherID,
		Name:           name,
		Description:    description,
	})
	if err != nil {
		return nil, err
	}
	// The id is logged, never the name: classroom names are user content, and a log
	// line is copied into tickets and dashboards (§59).
	logging.FromContext(ctx).Info("classroom created",
		"action", "create_classroom",
		"actor_user_id", in.TeacherID.String(),
		logging.FieldClassroomID, created.ID.String(),
	)
	return created, nil
}

// Get returns one classroom the teacher owns.
func (s *Service) Get(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, error) {
	return s.owned(ctx, classroomID, teacherID)
}

// Update edits the name and/or the description.
//
// Renaming an OPEN classroom is allowed, and deliberately so: fixing a typo in the
// course name while the lesson is running is harmless — students already in the room
// keep their session (it hangs off the run, not the name) and the name is not part
// of any identity or key. Forbidding it would only force a teacher to close the
// lesson to correct a spelling mistake.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*Classroom, error) {
	if in.Name == nil && !in.DescriptionSet {
		return nil, invalidf("at least one of name or description must be provided")
	}
	// Ownership before validation: a caller who may not touch this classroom learns
	// nothing about what a valid body would look like (§37).
	if _, err := s.owned(ctx, in.ClassroomID, in.TeacherID); err != nil {
		return nil, err
	}

	params := UpdateParams{ClassroomID: in.ClassroomID}
	if in.Name != nil {
		name, err := NormalizeName(*in.Name)
		if err != nil {
			return nil, err
		}
		params.Name = &name
	}
	if in.DescriptionSet {
		description, err := NormalizeDescription(in.Description)
		if err != nil {
			return nil, err
		}
		params.DescriptionSet = true
		params.Description = description
	}

	updated, err := s.repo.UpdateDetails(ctx, params)
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Info("classroom updated",
		"action", "update_classroom",
		"actor_user_id", in.TeacherID.String(),
		logging.FieldClassroomID, in.ClassroomID.String(),
		"name_changed", in.Name != nil,
		"description_changed", in.DescriptionSet,
	)
	return updated, nil
}

// ListStudents returns the roster of a classroom the teacher owns.
//
// DISABLED accounts are included (see Repository.ListStudents): the roster answers
// "who was authorized for this course", not "who can log in right now".
func (s *Service) ListStudents(ctx context.Context, classroomID, teacherID uuid.UUID) ([]Student, error) {
	if _, err := s.owned(ctx, classroomID, teacherID); err != nil {
		return nil, err
	}
	return s.repo.ListStudents(ctx, classroomID)
}

// AddStudents grants access to a list of accounts, one account at a time.
//
// PARTIAL SUCCESS is the contract, and it is a product decision, not a shortcut:
// importing a class list always contains two mistyped accounts. Failing the whole
// request would leave the teacher to work out which entries were wrong by
// bisecting their own list — the classic way an import feature becomes something
// people do by hand instead. Returning a per-account code (STUDENT_NOT_FOUND /
// NOT_A_STUDENT / ACCOUNT_DISABLED / INVALID_REQUEST) tells them exactly which
// lines to fix, and the codes are the same vocabulary the rest of the API uses so
// the frontend does not parse prose to build that message.
//
// The roster it returns is the state AFTER the call, so the teacher sees the
// result of what did succeed without a second request.
//
// Two rules that are deliberately NOT rejections:
//   - An account already on the roster is a success. It is exactly what the
//     teacher asked for, and rejecting it would make re-pasting a list (with one
//     name added) report the whole existing class as errors.
//   - An account repeated inside one request is de-duplicated, not reported.
//
// A DB or infrastructure failure is different: it aborts the whole call. Partial
// success is about the CONTENT of the request, never about the health of the
// database — reporting "some students were not added" because of a transient
// failure would invite the teacher to retry and could hide a real outage.
func (s *Service) AddStudents(ctx context.Context, in AddStudentsInput) (*AddStudentsResult, error) {
	if len(in.Accounts) == 0 {
		return nil, invalidf("accounts must contain at least one account")
	}
	if len(in.Accounts) > MaxAccountsPerRequest {
		// A whole-request rejection rather than a per-account one: the bound exists
		// to cap the work one authenticated request can ask for (each account is a
		// lookup), so trimming the list and proceeding would defeat it.
		return nil, invalidf("accounts must contain at most %d accounts", MaxAccountsPerRequest)
	}
	if _, err := s.owned(ctx, in.ClassroomID, in.TeacherID); err != nil {
		return nil, err
	}

	accounts := DedupeAccounts(in.Accounts)
	accepted := make([]uuid.UUID, 0, len(accounts))
	rejected := make([]Rejection, 0)

	// One lookup per account, so each rejection can name its own reason. A single
	// `WHERE account = ANY($1)` would be one round trip instead of up to 100, but it
	// would return a set — and "which of my two mistyped accounts was the wrong
	// one?" is the whole point of this response. The batch is bounded at
	// MaxAccountsPerRequest, and the account column is uniquely indexed, so even the
	// worst case is 100 index lookups.
	for _, account := range accounts {
		if err := user.ValidateAccount(account); err != nil {
			rejected = append(rejected, Rejection{Account: account, Code: apperr.CodeInvalidRequest})
			continue
		}
		student, err := s.accounts.FindByAccount(ctx, account)
		switch {
		case errors.Is(err, user.ErrNotFound):
			rejected = append(rejected, Rejection{Account: account, Code: apperr.CodeStudentNotFound})
		case err != nil:
			return nil, err
		case student == nil:
			// The directory contract says a found account is returned; nil with no
			// error is a bug, and refusing it here is better than dereferencing it
			// into a panic the recovery middleware turns into an opaque 500.
			return nil, fmt.Errorf("classroom: account lookup returned no account for %q", account)
		case student.Role != user.RoleStudent:
			// Checked before the status: "this is a teacher account" is the more
			// fundamental problem with the line, and telling the teacher their
			// colleague's account is disabled would be a misleading fix to make.
			rejected = append(rejected, Rejection{Account: account, Code: apperr.CodeNotAStudent})
		case student.Status != user.StatusActive:
			rejected = append(rejected, Rejection{Account: account, Code: apperr.CodeAccountDisabled})
		default:
			accepted = append(accepted, student.ID)
		}
	}

	if len(accepted) > 0 {
		// Granting while the classroom is OPEN is allowed on purpose: a student who
		// arrives late must be addable to the lesson in progress, and a monitoring
		// system that forces the teacher to close the room first would be useless
		// exactly when it is needed.
		if err := s.repo.AddStudents(ctx, in.ClassroomID, accepted, in.TeacherID); err != nil {
			return nil, err
		}
	}

	roster, err := s.repo.ListStudents(ctx, in.ClassroomID)
	if err != nil {
		return nil, err
	}

	logging.FromContext(ctx).Info("classroom students added",
		"action", "add_classroom_students",
		"actor_user_id", in.TeacherID.String(),
		logging.FieldClassroomID, in.ClassroomID.String(),
		"granted", len(accepted),
		"rejected", len(rejected),
	)
	return &AddStudentsResult{Students: roster, Rejected: rejected}, nil
}

// RemoveStudent revokes one student's access to a classroom the teacher owns.
func (s *Service) RemoveStudent(ctx context.Context, classroomID, studentID, teacherID uuid.UUID) error {
	if _, err := s.owned(ctx, classroomID, teacherID); err != nil {
		return err
	}
	if err := s.repo.RemoveStudent(ctx, classroomID, studentID); err != nil {
		return err
	}
	logging.FromContext(ctx).Info("classroom student removed",
		"action", "remove_classroom_student",
		"actor_user_id", teacherID.String(),
		logging.FieldClassroomID, classroomID.String(),
		"student_id", studentID.String(),
	)
	return nil
}

// Open flips a CLOSED classroom to OPEN and creates a new ClassroomRun (§48).
//
// The pre-flight checks below exist to produce a precise error on the common,
// non-racing path. They are NOT what makes the operation safe: between them and the
// write another request can change the row, so Repository.Open repeats all three
// checks under a `SELECT ... FOR UPDATE` — that transaction, not this function, is
// the authority (§33: the database and the control plane decide, nothing else).
//
// Creating a NEW run on every open is the rule this method exists to enforce (§8).
// There is no "reuse the last run" branch to add later by accident: the run id is
// minted here, so the only way to end up with a reused run is to stop calling this.
func (s *Service) Open(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, *Run, error) {
	current, err := s.owned(ctx, classroomID, teacherID)
	if err != nil {
		return nil, nil, err
	}
	if current.Status == StatusOpen {
		return nil, nil, ErrAlreadyOpen
	}

	runID := s.newID()
	opened, run, err := s.repo.Open(ctx, OpenParams{
		ClassroomID: classroomID,
		TeacherID:   teacherID,
		RunID:       runID,
		// The room name is derived from the id and never from anything a person is
		// identifiable by (§8). Storing it now reserves the unique name; Phase 6
		// creates the actual LiveKit room with exactly this name.
		RoomName: RoomName(runID),
	})
	if err != nil {
		return nil, nil, err
	}
	logging.FromContext(ctx).Info("classroom opened",
		"action", "open_classroom",
		"actor_user_id", teacherID.String(),
		logging.FieldClassroomID, classroomID.String(),
		logging.FieldRunID, run.ID.String(),
	)
	return opened, run, nil
}

// Close ends the current run and flips the classroom to CLOSED (§49).
//
// Phase 6 will call LiveKit TerminateRoom and Phase 8 will mark this run's
// student_sessions ROOM_CLOSED and broadcast ROOM_CLOSED — both AFTER this call
// returns, so the database already says CLOSED when anything downstream reacts (see
// the seam note on Repository.Close). Phase 3 has neither, and no stub is left here
// pretending otherwise.
func (s *Service) Close(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, *Run, error) {
	current, err := s.owned(ctx, classroomID, teacherID)
	if err != nil {
		return nil, nil, err
	}
	if current.Status == StatusClosed {
		return nil, nil, ErrAlreadyClosed
	}

	closed, run, err := s.repo.Close(ctx, classroomID, teacherID)
	if err != nil {
		return nil, nil, err
	}
	logging.FromContext(ctx).Info("classroom closed",
		"action", "close_classroom",
		"actor_user_id", teacherID.String(),
		logging.FieldClassroomID, classroomID.String(),
		logging.FieldRunID, run.ID.String(),
	)
	return closed, run, nil
}

// owned loads a classroom and verifies that teacherID owns it.
//
// WHY this runs on every endpoint including the read-only ones: §37 and §63 make
// server-side ownership checking mandatory for the classroom resource, and the only
// way to make it a property of the system rather than of the endpoints somebody
// remembered is for there to be exactly one way to load a classroom for a request.
// A classroom that does not exist is ErrNotFound; one owned by somebody else is
// ErrNotOwner — two different product answers (§58).
func (s *Service) owned(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, error) {
	if classroomID == uuid.Nil || teacherID == uuid.Nil {
		// A nil uuid can never match a row (the column is NOT NULL and generated), so
		// this only reaches here through a programming error. Answering "not found"
		// keeps a nil id from ever being used as a wildcard by a future query change.
		return nil, ErrNotFound
	}
	current, err := s.repo.GetByID(ctx, classroomID)
	if err != nil {
		return nil, err
	}
	if current.OwnerTeacherID != teacherID {
		return nil, fmt.Errorf("%w: classroom %s", ErrNotOwner, classroomID)
	}
	return current, nil
}
