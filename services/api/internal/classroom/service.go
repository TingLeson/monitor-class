package classroom

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// rooms is the media-plane cleanup used by Close (§49). A nil value means "no
	// media plane is wired" — the classroom lifecycle is then purely a control-plane
	// operation, which is exactly the state the unit tests run in.
	rooms RoomTerminator
	// runtime is the event path of §47/§48/§49: the session states a close ends, and the
	// WebSocket messages both lifecycle transitions produce. A nil value means "no
	// runtime layer is wired", which is a supported state (§52's single-instance
	// deployment without a hub, and every unit test that is about the classroom rules).
	runtime RuntimeHooks
	// newID mints the run id. It is injectable so a test can assert the room name
	// is derived from the run id rather than invented separately.
	newID func() uuid.UUID
}

// RuntimeHooks is the runtime layer as the classroom lifecycle sees it (§47/§48/§49).
//
// WHY the close flow's two steps are one port: they are one sequence — the sessions of
// the run end, then the clients are told — and a caller that could do one without the
// other would be able to leave the teacher's console showing a lesson that is over. The
// implementation is internal/session's Processor, which owns both halves; this package
// only decides WHEN they happen (after the commit, never before).
//
// Every method returns an error and none of them may fail the request: the classroom's
// state change is already committed when they run, and a broadcast is not a transaction
// participant (§33). The service logs the failure and still answers the teacher.
type RuntimeHooks interface {
	// CloseRunSessions marks the run's active student sessions ROOM_CLOSED, records one
	// event per row and tells the owner about each. It returns how many it closed, which
	// is zero when they were already terminal (the room_finished webhook got there
	// first) — a normal outcome, not a failure.
	CloseRunSessions(ctx context.Context, runID uuid.UUID) (int, error)
	// RoomOpened announces a new run to the students it authorizes (§48).
	RoomOpened(ctx context.Context, classroomID, runID uuid.UUID) error
	// RoomClosed announces the end of a run to its students and its owner (§49).
	RoomClosed(ctx context.Context, classroomID, runID uuid.UUID) error
}

// RoomTerminator is the one media-plane operation the classroom lifecycle needs in
// Phase 6: closing a classroom must end its media room (§49).
//
// WHY this is an interface declared here rather than *media.Client: the classroom
// domain must stay testable without a LiveKit server, and a one-method port is the
// smallest thing that says "this is the only media call this package makes".
type RoomTerminator interface {
	TerminateRoom(ctx context.Context, roomName string) error
}

// terminateRoomTimeout bounds the media-plane cleanup that follows a close. The
// database has already committed by then, so this is a bounded courtesy call and not
// a step the answer depends on.
const terminateRoomTimeout = 3 * time.Second

// runtimeHookTimeout bounds each post-commit runtime step (closing the run's sessions,
// broadcasting ROOM_OPENED/ROOM_CLOSED). Same reasoning as terminateRoomTimeout: the
// decision is already committed, and a slow listener must not extend a teacher's request.
const runtimeHookTimeout = 3 * time.Second

// NewService wires the service. accounts resolves submitted student accounts; it
// is the user repository in production (see AccountDirectory).
func NewService(repo Repository, accounts AccountDirectory) *Service {
	return &Service{repo: repo, accounts: accounts, newID: uuid.New}
}

// WithRoomTerminator attaches the media-plane room teardown of §49.
//
// WHY a setter instead of a constructor argument: every existing caller and test
// keeps working without a media plane — and, more importantly, "no terminator" is a
// state the code has to handle anyway, because STARTUP_REQUIRE_DEPENDENCIES=false
// allows the API to run with LiveKit unreachable. Making it an explicit,
// optional attachment keeps that degraded mode visible at the wiring site in main.go
// rather than hidden behind a nil interface argument in every test.
func (s *Service) WithRoomTerminator(rooms RoomTerminator) *Service {
	s.rooms = rooms
	return s
}

// WithRuntimeHooks attaches the event path of §47/§48/§49 (session_events, the
// ROOM_OPENED/ROOM_CLOSED broadcasts).
//
// Same reasoning as WithRoomTerminator: the classroom lifecycle is complete without it,
// and making it an explicit attachment keeps a deployment without a realtime layer
// visible at the wiring site instead of implied by a nil interface argument.
func (s *Service) WithRuntimeHooks(hooks RuntimeHooks) *Service {
	s.runtime = hooks
	return s
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
	// §48: the students' dashboards update without a reload. AFTER the commit, and
	// never able to fail the open — the classroom IS open, and a broadcast is a
	// courtesy that the next page load or poll repairs.
	s.announceOpen(ctx, classroomID, run)
	return opened, run, nil
}

// Close ends the current run and flips the classroom to CLOSED (§49).
//
// The order is: commit the control plane, THEN tell the media plane and the clients.
// Terminating the LiveKit room is cleanup that makes the media plane catch up with a
// decision the database has already recorded — never the other way round (§33). Two
// consequences, both deliberate:
//
//   - A failing TerminateRoom does NOT fail the request and does NOT roll anything
//     back. The classroom IS closed; the room is merely behind, and LiveKit's own
//     empty_timeout collects it once the last participant drops. Reporting a 500 here
//     would tell the teacher the close failed while the students' dashboards already
//     show it as closed.
//   - The cleanup runs on a context detached from the request. The commit is done, so
//     a browser that navigated away must not cancel the teardown, and a bug in the
//     media call must not be able to extend the response beyond terminateRoomTimeout.
//
// Phase 8 adds the two runtime steps of §49, in this order and all after the commit:
// mark the run's student sessions ROOM_CLOSED (+ one event each), then broadcast
// ROOM_CLOSED. The LiveKit room_finished webhook arrives afterwards and is idempotent
// against both — the sessions are already terminal, so it closes nothing and sends
// nothing.
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
	// Control plane first (the sessions of a finished run are over, and the record of it
	// must exist even if everything after this line fails), then the media plane, then
	// the clients.
	s.closeRunSessions(ctx, classroomID, run)
	s.terminateRoom(ctx, classroomID, run)
	s.announceClose(ctx, classroomID, run)
	return closed, run, nil
}

// closeRunSessions marks the run's active sessions ROOM_CLOSED (§49).
//
// A failure is a Warn and nothing else. The alternative — failing the teacher's close
// because a student row could not be updated — would be a lie: the classroom is closed,
// the run is closed, and no API can reopen them. What is left behind is a session row
// that still says ONLINE; the room_finished webhook (which is already on its way, the
// room having been terminated) closes it, and the monitor endpoint's fallback keeps the
// wall honest in the meantime.
func (s *Service) closeRunSessions(ctx context.Context, classroomID uuid.UUID, run *Run) {
	if s.runtime == nil || run == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeHookTimeout)
	defer cancel()

	closed, err := s.runtime.CloseRunSessions(cleanupCtx, run.ID)
	if err != nil {
		logging.FromContext(ctx).Warn("student sessions were not closed with the run",
			"action", "close_run_sessions_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"error", err,
			"consequence", "the classroom IS closed; the room_finished webhook closes the remaining sessions",
		)
		return
	}
	logging.FromContext(ctx).Info("student sessions closed with the run",
		"action", "close_run_sessions",
		logging.FieldClassroomID, classroomID.String(),
		logging.FieldRunID, run.ID.String(),
		"closed", closed,
	)
}

// announceOpen broadcasts ROOM_OPENED (§48).
func (s *Service) announceOpen(ctx context.Context, classroomID uuid.UUID, run *Run) {
	if s.runtime == nil || run == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeHookTimeout)
	defer cancel()
	if err := s.runtime.RoomOpened(cleanupCtx, classroomID, run.ID); err != nil {
		logging.FromContext(ctx).Warn("room opened was not broadcast",
			"action", "room_opened_broadcast_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"error", err,
			"consequence", "the classroom IS open; students see it on their next load or poll",
		)
	}
}

// announceClose broadcasts ROOM_CLOSED (§49).
func (s *Service) announceClose(ctx context.Context, classroomID uuid.UUID, run *Run) {
	if s.runtime == nil || run == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeHookTimeout)
	defer cancel()
	if err := s.runtime.RoomClosed(cleanupCtx, classroomID, run.ID); err != nil {
		logging.FromContext(ctx).Warn("room closed was not broadcast",
			"action", "room_closed_broadcast_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"error", err,
			"consequence", "the classroom IS closed; students see it on their next load or poll",
		)
	}
}

// terminateRoom ends the media room of a run that was just closed.
//
// It never returns an error: the caller's state change has already succeeded, and the
// only correct responses to a media-plane failure are a log line and the knowledge
// that LiveKit will expire the room on its own (§49).
func (s *Service) terminateRoom(ctx context.Context, classroomID uuid.UUID, run *Run) {
	if s.rooms == nil || run == nil || run.LiveKitRoomName == "" {
		// No media plane wired, or a run without a room name (impossible while the
		// column is NOT NULL, but a nil dereference here would abort the response of
		// an operation that already succeeded).
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminateRoomTimeout)
	defer cancel()

	if err := s.rooms.TerminateRoom(cleanupCtx, run.LiveKitRoomName); err != nil {
		// The room name is logged (it is an opaque uuid, not a person §8) because
		// "which room stayed alive?" is the only actionable part of this line. The
		// error's own text is logged: it is a media-plane error, not a credential.
		logging.FromContext(ctx).Warn("media room was not terminated after close",
			"action", "terminate_room_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"error", err,
			"consequence", "the room is cleaned up by LiveKit's empty timeout instead",
		)
	}
}

// ListStudentClassrooms returns the classrooms the authenticated student may see.
//
// This is the whole of §14 and, in Phase 4, the whole of the student portal: the
// authorization is the repository's JOIN on classroom_students, so there is nothing
// to check here and — importantly — nothing to filter here either.
//
// There is NO "is this classroom OPEN?" logic in this method or in the one below.
// The status is DATA the student's card renders ("已开启" / "暂不可进入" §14); the
// decision "may this student actually enter right now?" belongs to the screen gate
// of Phase 5, which is the first place that has to know whether a media session can
// be created at all. Enforcing it here would mean a CLOSED classroom is reported to
// the portal as an error, and the student would see a broken page instead of a card
// that explains itself. (§33 also forbids reading anything into this path: the
// control plane's status is the source of truth, and there is no media plane yet.)
func (s *Service) ListStudentClassrooms(ctx context.Context, studentID uuid.UUID) ([]StudentClassroom, error) {
	if studentID == uuid.Nil {
		// A nil id can never match a grant row. Returning an empty list rather than
		// an error keeps the "a student with no classrooms" contract identical for a
		// programming error and for a real empty roster, and — unlike teacher
		// reads — there is no resource id here to report as invalid.
		return []StudentClassroom{}, nil
	}
	classrooms, err := s.repo.ListForStudent(ctx, studentID)
	if err != nil {
		return nil, err
	}
	// The count is logged, never the classrooms: this is a list endpoint that a
	// whole school calls at the start of a lesson (§59). The ids and names in the
	// response add nothing an operator can act on, while one line per request per
	// student is exactly the volume that makes a log useless.
	logging.FromContext(ctx).Info("student classrooms listed",
		"action", "list_student_classrooms",
		logging.FieldUserID, studentID.String(),
		"classrooms", len(classrooms),
	)
	return classrooms, nil
}

// GetStudentClassroom returns one classroom as the given student sees it.
//
// "Not mine" is ErrStudentNotAssigned, the SAME answer for "no classroom has that
// id" and "the classroom exists but you are not on its roster". WHY one code and not
// a 404/403 pair:
//
//   - The id is the only thing the caller supplied, and it is a UUID. If an
//     unauthorized id answered differently from a nonexistent one, a student could
//     enumerate ids to learn which lessons exist in the school — a timetable they
//     have no business reading (§14/§63).
//   - "You are not in this classroom's list" is a true statement about both cases.
//     The portal renders one message ("这个课堂不在你的名单里"), and it is correct
//     either way.
//   - A 403 here would additionally tell the student that the classroom is real,
//     and would send the frontend into a "request access" flow for a course that
//     may not exist.
//
// A malformed id never reaches this function: the handler answers 400 INVALID_REQUEST
// (§58) before any lookup, so a string that is not an identifier is reported as a bad
// request rather than as a missing classroom.
func (s *Service) GetStudentClassroom(ctx context.Context, studentID, classroomID uuid.UUID) (*StudentClassroom, error) {
	if studentID == uuid.Nil {
		// Same collapse as above: no grant row can match, so there is nothing to look
		// up and the answer is the one this endpoint already gives for "not yours".
		return nil, ErrStudentNotAssigned
	}
	if classroomID == uuid.Nil {
		// Unlike the list, this request named a resource. A nil uuid is not an
		// identifier the client could have meant, so it is a malformed request — and
		// answering INVALID_REQUEST keeps a nil id from ever behaving like a wildcard
		// if a future query loses its WHERE clause.
		return nil, invalidf("classroom id must be a UUID")
	}
	found, err := s.repo.GetForStudent(ctx, studentID, classroomID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		// "nil without an error" is a broken repository contract, not an empty
		// result: answering 404 would hide the bug behind a plausible product state.
		return nil, errors.New("classroom: repository returned no classroom for a student read")
	}
	return found, nil
}

// StudentEntry returns the classroom and the run a student asking to enter would
// join (§43).
//
// The authorization rule is the repository's roster JOIN, so this method adds no
// filter of its own: "may this student see the classroom" and "may this student
// enter it" are the same question asked with the same data, and answering them with
// two different queries is how the two answers eventually disagree (§14/§63).
//
// The classroom's status is returned rather than enforced. WHY: whether a CLOSED
// classroom is a 409 or a page that says "还没开始" is a product decision that belongs
// to the caller that knows the operation; a repository or service that refused would
// leave the join endpoint unable to distinguish "not yours" from "not open", which
// are the two answers the student UI renders differently (§58).
func (s *Service) StudentEntry(ctx context.Context, studentID, classroomID uuid.UUID) (*StudentEntry, error) {
	if studentID == uuid.Nil || classroomID == uuid.Nil {
		// Same collapse as the portal reads: a nil uuid can never match a grant row,
		// and answering "not assigned" keeps a nil id from ever behaving like a
		// wildcard if a future query loses its WHERE clause.
		return nil, ErrStudentNotAssigned
	}
	entry, err := s.repo.GetStudentEntry(ctx, studentID, classroomID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, errors.New("classroom: repository returned no entry for a student join")
	}
	return entry, nil
}

// RunByID returns one run by id.
//
// WHY the session package cannot work this out for itself: a student session stores
// the run id, and leaving a session has to name the media room to remove the
// participant from. Re-deriving `lk_<run_uuid>` outside this package would put a
// second implementation of the naming scheme in the codebase, and the two would
// eventually disagree about a room that is actually live (§8).
func (s *Service) RunByID(ctx context.Context, runID uuid.UUID) (*Run, error) {
	if runID == uuid.Nil {
		return nil, ErrNotFound
	}
	return s.repo.GetRunByID(ctx, runID)
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
