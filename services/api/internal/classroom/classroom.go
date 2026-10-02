// Package classroom implements the classroom domain of §6–§11: the long-lived
// classroom container, the student roster that authorizes access to it, and the
// ClassroomRun that each CLOSED → OPEN transition creates.
//
// The single most important rule in this package is that a Classroom and a
// ClassroomRun are different things (§6). A Classroom is a course that exists for
// months; a Run is one evening of it. Every open creates a NEW run and never
// reuses the previous one (§8), because the run id is what media sessions, events
// and the monitoring wall attach to — reusing it would silently overwrite the
// history of the previous lesson.
//
// Two boundaries this package deliberately respects:
//
//   - Phase 3 allocates a room name but never calls LiveKit (§33/§69). The name is
//     an opaque `lk_<run_uuid>` reserved in the database; the media plane is a
//     later phase's concern. Nothing here decides anything from a LiveKit response,
//     because the Control Plane is the only Source of Truth.
//   - Authorization is an equality test against the stored owner
//     (`classroom.owner_teacher_id == current teacher`), never against a route, a
//     cookie or a role (§4/§37). An ADMIN is not a super-teacher: the admin
//     surface manages accounts, and it cannot open or close somebody's classroom.
package classroom

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Status is the user-visible state of a classroom.
//
// Only two values exist on purpose (§7). WAITING/STARTING/PAUSED/READY/ENDED/
// ARCHIVED are forbidden: they would each need an answer to "can a student enter
// now?" from the student portal, the teacher console and the monitoring wall, and
// the three answers would eventually disagree. Where a lesson *is* in its
// lifecycle is expressed by the ClassroomRun and, later, the StudentSession.
type Status string

const (
	StatusOpen   Status = "OPEN"
	StatusClosed Status = "CLOSED"
)

// Business limits of the HTTP contract. They live here, next to the rules they
// belong to, so the handler and the service cannot drift apart.
const (
	// MaxNameLength bounds a classroom name. It is a presentation limit, not a
	// security boundary: the name is rendered in lists, on the student dashboard
	// and inside log lines, and an unbounded one would blow up all three.
	MaxNameLength = 80
	// MaxDescriptionLength bounds the description for the same reason.
	MaxDescriptionLength = 500
	// MaxAccountsPerRequest bounds one batch-add. It caps how much work a single
	// authenticated request can ask for (each account is a lookup), and it matches
	// what a teacher can realistically paste into an import box in one go.
	MaxAccountsPerRequest = 100
)

// Room-name prefix. The suffix is the run's UUID (see RoomName), which is what
// makes the name opaque (§8): a room name is visible to every participant through
// the media SDK, so it must not carry a student account, a real name or a class
// name. `lk_` is kept only so a human reading a LiveKit log can tell this is one of
// our rooms.
const roomNamePrefix = "lk_"

// RoomName returns the LiveKit room name reserved for a run.
//
// Phase 3 only ALLOCATES and stores this value; it never contacts LiveKit (media
// arrives in Phase 6). The mapping is deterministic (`lk_<run_uuid>`) so it can be
// re-derived from the run alone and never has to be searched for.
func RoomName(runID uuid.UUID) string { return roomNamePrefix + runID.String() }

// Classroom is one classroom row plus the two derived facts the API returns.
type Classroom struct {
	ID             uuid.UUID
	Name           string
	Description    *string
	OwnerTeacherID uuid.UUID
	Status         Status
	CurrentRunID   *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// StudentCount is the size of the roster, loaded with the row so a list of
	// classrooms costs one query instead of one per classroom.
	StudentCount int
	// CurrentRun is set exactly when Status is OPEN (the database keeps the two in
	// lockstep — see classrooms_run_consistency). It carries only what the API
	// returns; the LiveKit room name stays out of it deliberately (see Run).
	CurrentRun *Run
}

// Run is one execution of a classroom (§8).
type Run struct {
	ID          uuid.UUID
	ClassroomID uuid.UUID
	// Status uses the same two-value vocabulary as Classroom: an OPEN run is the
	// one currently attached to an OPEN classroom, and a CLOSED run has closed_at.
	Status Status
	// LiveKitRoomName is `lk_<run_uuid>`. It stays in the domain (Phase 6 needs it)
	// and is deliberately NOT in the HTTP DTO: the room name is a media-plane
	// detail, and publishing it before there is a media plane would hand clients a
	// value they have no legitimate use for.
	LiveKitRoomName string
	OpenedAt        time.Time
	ClosedAt        *time.Time
}

// StudentClassroom is one classroom as a STUDENT sees it (§14/§70).
//
// WHY this is a type of its own instead of a Classroom with fields blanked out: the
// student view and the teacher view are not the same resource with different
// visibility — they are two different questions. The teacher asks "how is my course
// doing?" and needs the roster size; the student asks "which lesson can I enter
// now?" and must never be told who else is in the room. A shared type would make
// every future teacher field an accidental student field, and the leak would be
// discovered by a student rather than by a test. The DTO in internal/httpapi mirrors
// this type field for field, so a field that is not here cannot be published there.
//
// Two things this type deliberately does NOT carry:
//
//   - Any roster information (no StudentCount, no names, no accounts). §26 forbids
//     one student learning anything about another, and "how many people are in this
//     lesson?" is exactly such a fact. Omitting the field is the only version of
//     this rule that cannot regress: there is nothing to forget to filter.
//   - The owner teacher's id or account. The card shows a teacher's display name and
//     nothing more, so the id would be an internal identifier published for no
//     product reason.
type StudentClassroom struct {
	ID          uuid.UUID
	Name        string
	Description *string
	// Status is OPEN or CLOSED and is reported, never enforced here: a CLOSED
	// classroom is still returned (§14 renders it as "暂不可进入") and whether the
	// student may actually enter is decided by Phase 5's screen gate, not by this
	// read path.
	Status Status
	// TeacherDisplayName is users.display_name of the classroom owner. It is the one
	// piece of another account this DTO may contain, because the student card shows
	// whose lesson it is; no id, account or email travels with it.
	TeacherDisplayName string
	// CurrentRun is the run of the OPEN period, and nil while CLOSED. The database
	// keeps the two in lockstep (classrooms_run_consistency), so the zero value here
	// is not "unknown" but "there is none".
	CurrentRun *StudentCurrentRun
	CreatedAt  time.Time
}

// StudentCurrentRun is the run reference embedded in a StudentClassroom.
//
// It carries id and openedAt and nothing else: the LiveKit room name is a
// media-plane detail that Phase 6 hands out with the media token to a participant
// who is entitled to join, never to a list endpoint (§33).
type StudentCurrentRun struct {
	ID       uuid.UUID
	OpenedAt time.Time
}

// Student is one roster entry joined with the account it authorizes.
type Student struct {
	// ID is the account id (users.id), which is also classroom_students.student_id.
	ID          uuid.UUID
	Account     string
	DisplayName string
	// Status is the ACCOUNT status, not a roster status. A DISABLED student stays
	// on the roster — they were authorized for this course and may be re-enabled —
	// and the console shows them greyed out. Dropping them from the list instead
	// would make "why is this student missing?" unanswerable.
	Status  user.Status
	AddedAt time.Time
}

// Rejection is one account a batch add refused, with the stable code that says
// why. It exists because batch import is a partial-success operation (see
// Service.AddStudents).
type Rejection struct {
	// Account is the account as the teacher submitted it (trimmed), so a typo can
	// be spotted next to the code that rejected it.
	Account string
	// Code is one of the API error codes: STUDENT_NOT_FOUND, NOT_A_STUDENT,
	// ACCOUNT_DISABLED or INVALID_REQUEST. It is an apperr.Code rather than a
	// private enum so the response cannot invent a code the frontend does not know.
	Code apperr.Code
}

// Sentinel errors. They are the vocabulary the HTTP layer maps onto error codes
// (internal/httpapi/teacher_classrooms.go), which keeps HTTP semantics out of the
// domain and makes every rule testable without a router.
var (
	// ErrNotFound means no classroom has that id.
	ErrNotFound = errors.New("classroom: not found")

	// ErrNotOwner means the classroom exists but belongs to another teacher.
	//
	// It is a separate sentinel from ErrNotFound because §58 makes
	// CLASSROOM_NOT_OWNER a distinct product answer: the frontend shows "only the
	// teacher who created this classroom can do that", not "it was deleted". The
	// classroom id is an unguessable UUID and the caller is an authenticated
	// teacher either way, so distinguishing the two leaks nothing.
	ErrNotOwner = errors.New("classroom: caller is not the owner")

	// ErrAlreadyOpen means an open was attempted on a classroom that is OPEN. It is
	// the expected outcome of a double click or a retried request, not a failure.
	ErrAlreadyOpen = errors.New("classroom: already open")

	// ErrAlreadyClosed means a close was attempted on a classroom that is CLOSED.
	ErrAlreadyClosed = errors.New("classroom: already closed")

	// ErrStudentNotAssigned means the student is not on this classroom's roster.
	ErrStudentNotAssigned = errors.New("classroom: student is not assigned to this classroom")

	// ErrInvalidRequest is the sentinel behind every "the request itself is
	// unusable" rejection. The concrete error also carries a human-readable
	// sentence (see invalidError).
	ErrInvalidRequest = errors.New("classroom: invalid request")
)

// invalidError is the error behind ErrInvalidRequest.
//
// WHY a bespoke type instead of apperr.Wrap(CodeInvalidRequest, ErrInvalidRequest):
// the HTTP layer forwards this sentence to the teacher ("name must be 1-80
// characters"), so it has to be the error's own text. Keeping the sentinel
// matchable through Is() means a caller can still separate "your request was
// wrong" from "the database is down" without parsing prose.
type invalidError struct{ message string }

func (e *invalidError) Error() string { return e.message }

// Is matches ErrInvalidRequest so errors.Is(err, ErrInvalidRequest) holds.
func (e *invalidError) Is(target error) bool { return target == ErrInvalidRequest }

// InvalidMessage returns the human-readable sentence of an invalid-request error,
// or "" for any other error.
func InvalidMessage(err error) string {
	var invalid *invalidError
	if errors.As(err, &invalid) {
		return invalid.message
	}
	return ""
}

// invalidf builds an invalid-request error with a specific, safe message.
func invalidf(format string, args ...any) error {
	return &invalidError{message: fmt.Sprintf(format, args...)}
}

// InvalidRequestf builds an invalid-request error from outside this package.
//
// It exists so a caller that owns part of the request contract — the HTTP layer
// rejecting a member that is present but null, or a test fake standing in for the
// service — can produce a rejection that is indistinguishable from one this
// package makes. Hand-rolling an error that merely *looks* similar would break
// errors.Is(err, ErrInvalidRequest), which is what the error mapping branches on.
func InvalidRequestf(format string, args ...any) error {
	return invalidf(format, args...)
}

// CreateParams is the input of Repository.Create.
type CreateParams struct {
	OwnerTeacherID uuid.UUID
	Name           string
	// Description is nil for "no description". The service has already turned a
	// blank string into nil, so the store never has to decide what "" means.
	Description *string
}

// UpdateParams is the input of Repository.UpdateDetails.
type UpdateParams struct {
	ClassroomID uuid.UUID
	// Name is nil when the request did not mention it.
	Name *string
	// DescriptionSet distinguishes "not mentioned" (false, leave the stored value
	// alone) from "explicitly cleared" (true with a nil Description). A plain
	// *string cannot express that, and the difference is visible to the user: with
	// one field, renaming a classroom would silently wipe its description.
	DescriptionSet bool
	Description    *string
}

// OpenParams is the input of Repository.Open. The id and the room name are minted
// by the service, not by the store: the room name is a domain decision
// (lk_<run_uuid>) and keeping it out of SQL makes it unit-testable.
type OpenParams struct {
	ClassroomID uuid.UUID
	// TeacherID is re-checked inside the locked transaction. The service already
	// checked it, but that check happened before this transaction started, and the
	// check that is protected by the row lock is the one that decides.
	TeacherID uuid.UUID
	RunID     uuid.UUID
	RoomName  string
}

// Repository is the persistence contract of the classroom domain.
//
// Open and Close are on this interface — SQL and locking cannot be separated —
// while every business rule that can be expressed without a lock lives in the
// Service. The interface exists so the rules can be unit tested against a fake.
type Repository interface {
	// Create inserts a CLOSED classroom and returns the stored row.
	Create(ctx context.Context, params CreateParams) (*Classroom, error)
	// GetByID returns one classroom with its roster size and current run.
	GetByID(ctx context.Context, id uuid.UUID) (*Classroom, error)
	// ListByOwner returns a teacher's classrooms, newest first. No pagination: see
	// the note on Service.List.
	ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Classroom, error)
	// ListForStudent returns the classrooms the student is authorized for, OPEN
	// first. The authorization filter is part of the SQL (`classroom_students`
	// JOINed in), never a filter applied after reading more rows than the caller may
	// see (§14).
	ListForStudent(ctx context.Context, studentID uuid.UUID) ([]StudentClassroom, error)
	// GetForStudent returns one classroom the student is authorized for, or
	// ErrStudentNotAssigned when there is no such row — the same answer for "no
	// classroom with that id" and "not on its roster" (see Service.GetStudentClassroom).
	GetForStudent(ctx context.Context, studentID, classroomID uuid.UUID) (*StudentClassroom, error)
	// UpdateDetails rewrites name and/or description and bumps updated_at.
	UpdateDetails(ctx context.Context, params UpdateParams) (*Classroom, error)
	// ListStudents returns the whole roster (including DISABLED accounts), newest
	// addition first.
	ListStudents(ctx context.Context, classroomID uuid.UUID) ([]Student, error)
	// AddStudents grants access to a set of accounts. It is idempotent: a student
	// already on the roster is a success, not a conflict, so a retried request or a
	// re-pasted import list cannot fail.
	AddStudents(ctx context.Context, classroomID uuid.UUID, studentIDs []uuid.UUID, addedBy uuid.UUID) error
	// RemoveStudent deletes one grant, returning ErrStudentNotAssigned when there
	// was none.
	RemoveStudent(ctx context.Context, classroomID, studentID uuid.UUID) error
	// Open creates the run and flips the classroom to OPEN in ONE transaction that
	// holds a row lock on the classroom. It re-checks ownership and state under that
	// lock, because between the service's read and this write another request may
	// have changed them.
	Open(ctx context.Context, params OpenParams) (*Classroom, *Run, error)
	// Close ends the current run and flips the classroom to CLOSED in one locked
	// transaction, returning the run it just closed. It re-checks ownership under
	// the lock for the same reason Open does.
	Close(ctx context.Context, classroomID, teacherID uuid.UUID) (*Classroom, *Run, error)
}

// AccountDirectory is the slice of the account store the classroom service needs
// to resolve submitted accounts.
//
// WHY a one-method interface instead of depending on user.Repository: the batch
// add resolves accounts one by one (≤100 per request) so each rejection can name
// its own reason, and that is the ONLY account operation this package performs.
// Depending on the whole repository would let a classroom feature grow an account
// responsibility by accident — and would force every test fake to implement a
// dozen unrelated methods.
type AccountDirectory interface {
	// FindByAccount looks an account up case-insensitively (the column is citext),
	// returning user.ErrNotFound when there is no such account.
	FindByAccount(ctx context.Context, account string) (*user.User, error)
}

// NormalizeName validates and normalises a classroom name.
//
// The stored value is trimmed: leading/trailing whitespace is invisible in every
// UI that renders the name, so "C++ 晚自习 " and "C++ 晚自习" must not be able to
// become two different-looking-but-different classrooms.
//
// Control characters are rejected rather than stripped. They render as nothing (or
// as a line break) in a list, and inside a log line they can forge structure — a
// name is a label for humans, and a label nobody can see is a bug that surfaces as
// "the classroom list looks broken".
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", invalidf("name is required and must not be blank")
	}
	if utf8.RuneCountInString(name) > MaxNameLength {
		return "", invalidf("name must be at most %d characters", MaxNameLength)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", invalidf("name must not contain control characters")
		}
	}
	return name, nil
}

// NormalizeDescription validates and normalises a description.
//
// nil and a blank string both mean "no description": an empty textarea is the most
// common way a user says "nothing here", and storing "" would make the API return
// a description the UI renders as an empty line while the null case renders
// nothing at all — two spellings of the same state, which eventually diverge.
//
// Unlike a name, a description may contain newlines and tabs: it is free text shown
// in a paragraph. Only NUL is refused, because PostgreSQL cannot store it in a text
// column at all — accepting it would turn a bad request into a driver error (500).
func NormalizeDescription(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	description := strings.TrimSpace(*raw)
	if description == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(description) > MaxDescriptionLength {
		return nil, invalidf("description must be at most %d characters", MaxDescriptionLength)
	}
	if strings.ContainsRune(description, 0) {
		return nil, invalidf("description must not contain NUL characters")
	}
	return &description, nil
}

// DedupeAccounts trims and de-duplicates a submitted account list, preserving the
// order of first appearance.
//
// De-duplication is case-INSENSITIVE, because accounts are stored in a citext
// column: `S10086` and `s10086` are the same student. Treating them as two would
// resolve the same row twice and report nothing useful about it, and the roster
// primary key would swallow the second insert in silence.
//
// A blank entry is kept (not dropped): the caller turns it into an explicit
// INVALID_REQUEST rejection, so a pasted list with a trailing comma shows the
// teacher exactly which line was empty instead of quietly importing one student
// fewer.
func DedupeAccounts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, entry := range raw {
		account := strings.TrimSpace(entry)
		key := strings.ToLower(account)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, account)
	}
	return out
}
