package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
)

// ClassroomService is the part of *classroom.Service the HTTP layer uses.
//
// WHY an interface: what needs pinning here is the transport contract — the three
// middleware layers, the exact JSON, which status and code a rejected request gets,
// and that an unknown field in a PATCH body is refused rather than ignored. None of
// that needs PostgreSQL, and a test that needs it anyway is a test nobody runs.
// The rules themselves are tested in internal/classroom.
//
// The service returns domain types, never DTOs: serialisation is the handler's job,
// and the DTOs below have no field for the LiveKit room name or the owner's account,
// so publishing either would have to be done deliberately.
//
// The two student reads are part of this interface because there is ONE classroom
// service in the process (cmd/api wires a single *classroom.Service, which serves
// both surfaces). student_classrooms.go declares them again as the narrow
// StudentClassroomService that the student handlers accept, so a student route can
// only ever reach the read-only part of the domain.
type ClassroomService interface {
	List(ctx context.Context, teacherID uuid.UUID) ([]classroom.Classroom, error)
	Create(ctx context.Context, in classroom.CreateInput) (*classroom.Classroom, error)
	Get(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error)
	Update(ctx context.Context, in classroom.UpdateInput) (*classroom.Classroom, error)
	ListStudents(ctx context.Context, classroomID, teacherID uuid.UUID) ([]classroom.Student, error)
	AddStudents(ctx context.Context, in classroom.AddStudentsInput) (*classroom.AddStudentsResult, error)
	RemoveStudent(ctx context.Context, classroomID, studentID, teacherID uuid.UUID) error
	Open(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error)
	Close(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error)
	ListStudentClassrooms(ctx context.Context, studentID uuid.UUID) ([]classroom.StudentClassroom, error)
	GetStudentClassroom(ctx context.Context, studentID, classroomID uuid.UUID) (*classroom.StudentClassroom, error)
}

// maxClassroomBodyBytes bounds a classroom request body.
//
// The batch-add endpoint is the reason this is larger than the admin bound: 100
// accounts of at most 64 characters are ~7 KiB of JSON, and the limit must not be
// what rejects a teacher's oversized paste before the service can answer with the
// precise "at most 100 accounts" message. 256 KiB is ~3800 maximum-length accounts,
// comfortably past every legitimate use and still small enough that a buggy client
// cannot stream a file into the process.
const maxClassroomBodyBytes = 256 << 10

// classroomDTO is the classroom shape of §42.
//
// It deliberately has no `currentRunId`, no `livekitRoomName` and no owner account:
// the client gets the current run as an object (id + openedAt, everything the
// console needs to show "opened at 19:00"), and the room name is a media-plane
// detail that Phase 6 will hand out with the media token, to the participants who
// are entitled to join — not to every list response.
type classroomDTO struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    *string `json:"description"`
	Status         string  `json:"status"`
	OwnerTeacherID string  `json:"ownerTeacherId"`
	StudentCount   int     `json:"studentCount"`
	// CurrentRun is null exactly when the classroom is CLOSED, which is the same
	// invariant the database enforces (classrooms_run_consistency).
	CurrentRun *classroomRunRefDTO `json:"currentRun"`
	CreatedAt  string              `json:"createdAt"`
	UpdatedAt  string              `json:"updatedAt"`
}

// classroomRunRefDTO is the subset of a run embedded in a classroom.
type classroomRunRefDTO struct {
	ID       string `json:"id"`
	OpenedAt string `json:"openedAt"`
}

// classroomRunDTO is the ClassroomRun shape returned by open and close.
//
// It has no livekitRoomName field for the same reason classroomDTO has none: the
// room name is what a participant needs to join, and that is issued with the media
// token in Phase 6. Returning it here would leak it into logs, screenshots and
// browser history for no benefit.
type classroomRunDTO struct {
	ID          string  `json:"id"`
	ClassroomID string  `json:"classroomId"`
	Status      string  `json:"status"`
	OpenedAt    string  `json:"openedAt"`
	ClosedAt    *string `json:"closedAt"`
}

// classroomStudentDTO is one roster entry.
//
// It carries the account status (ACTIVE/DISABLED) rather than a roster status,
// because a DISABLED student stays on the roster and the console must be able to
// show them greyed out instead of silently dropping them from the list.
type classroomStudentDTO struct {
	ID          string `json:"id"`
	Account     string `json:"account"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
	AddedAt     string `json:"addedAt"`
}

// rejectedAccountDTO is one refused account of a batch add.
//
// The `code` is the contract; the account is echoed so the frontend can attach the
// code to the exact line the teacher pasted without matching strings itself.
type rejectedAccountDTO struct {
	Account string `json:"account"`
	Code    string `json:"code"`
}

// createClassroomRequest is the body of POST /teacher/classrooms.
//
// Name is a pointer so "the client sent no name" is distinguishable from "the
// client sent an empty name" — the first is a missing member, the second is a
// blank value, and a single string field would report the same thing for both.
type createClassroomRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// updateClassroomRequest is the body of PATCH /teacher/classrooms/:id.
//
// Both members are presence-tracking (see patchString): `{"name":"x"}` renames the
// classroom and leaves the description alone, `{"description":null}` clears the
// description. With plain pointers those two requests would be indistinguishable
// from `{}` and from each other, and every rename would wipe the description.
type updateClassroomRequest struct {
	Name        patchString `json:"name"`
	Description patchString `json:"description"`
}

// patchString is one optional string member of a PATCH body.
type patchString struct {
	// Set records that the member was present in the JSON at all.
	Set bool
	// Value is the string, or nil when the member was explicitly `null`.
	Value *string
}

// UnmarshalJSON records presence in addition to the value.
func (f *patchString) UnmarshalJSON(data []byte) error {
	f.Set = true
	if string(data) == "null" {
		f.Value = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		// Returned unmodified so bindJSON can classify it ("a field in the request
		// body has the wrong type") without echoing the body.
		return err
	}
	f.Value = &value
	return nil
}

// addStudentsRequest is the body of POST /teacher/classrooms/:id/students.
type addStudentsRequest struct {
	Accounts []string `json:"accounts"`
}

// classroomHandlers implements the eight teacher classroom endpoints.
type classroomHandlers struct {
	service ClassroomService
}

func newClassroomHandlers(service ClassroomService) *classroomHandlers {
	return &classroomHandlers{service: service}
}

// listClassrooms handles GET /api/v1/teacher/classrooms.
//
// The response is `{"classrooms":[...]}` with no paging metadata: a teacher owns a
// handful of courses, and a page/total envelope would advertise a pagination
// contract the endpoint does not implement (see classroom.Repository.ListByOwner).
func (h *classroomHandlers) listClassrooms() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		classrooms, err := h.service.List(c.Request.Context(), teacherID)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		// Built with make() so an empty list serialises as `[]` and not null.
		dtos := make([]classroomDTO, 0, len(classrooms))
		for i := range classrooms {
			dto, err := newClassroomDTO(&classrooms[i])
			if err != nil {
				RespondError(c, classroomError(err))
				return
			}
			dtos = append(dtos, dto)
		}
		RespondJSON(c, http.StatusOK, gin.H{"classrooms": dtos})
	}
}

// createClassroom handles POST /api/v1/teacher/classrooms.
//
// 201 with a Location header: the client can follow it, and an operator reading an
// HTTP trace sees what was created without decoding the body.
//
// The owner is taken from the session, never from the body — a client-supplied
// ownerTeacherId would let one teacher create classrooms in another teacher's name,
// which is precisely the authorization the rest of this file depends on.
func (h *classroomHandlers) createClassroom() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		var req createClassroomRequest
		if !bindClassroomJSON(c, &req) {
			return
		}
		created, err := h.service.Create(c.Request.Context(), classroom.CreateInput{
			TeacherID:   teacherID,
			Name:        stringValue(req.Name),
			Description: req.Description,
		})
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		dto, err := newClassroomDTO(created)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		c.Header("Location", classroomPath(created.ID))
		RespondJSON(c, http.StatusCreated, gin.H{"classroom": dto})
	}
}

// getClassroom handles GET /api/v1/teacher/classrooms/:id.
func (h *classroomHandlers) getClassroom() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		found, err := h.service.Get(c.Request.Context(), id, teacherID)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		dto, err := newClassroomDTO(found)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"classroom": dto})
	}
}

// updateClassroom handles PATCH /api/v1/teacher/classrooms/:id.
//
// Unknown members — `status`, `currentRunId`, `ownerTeacherId`, a typo like
// `Name` — are rejected with 400 by the strict binder instead of being dropped.
// WHY that matters more than it looks: a silently ignored `status` would let the
// API answer 200 to a request that did not do what it said, and a client that
// believed it had opened a classroom would show students an OPEN badge for a
// classroom the database still has CLOSED. The status transition has its own
// endpoint (/open, /close) precisely so it can be audited and locked.
func (h *classroomHandlers) updateClassroom() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		var req updateClassroomRequest
		if !bindClassroomJSON(c, &req) {
			return
		}
		if req.Name.Set && req.Name.Value == nil {
			RespondError(c, invalidClassroomRequest("name must be a string, not null"))
			return
		}
		in := classroom.UpdateInput{
			ClassroomID:    id,
			TeacherID:      teacherID,
			Name:           req.Name.Value,
			DescriptionSet: req.Description.Set,
			Description:    req.Description.Value,
		}
		updated, err := h.service.Update(c.Request.Context(), in)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		dto, err := newClassroomDTO(updated)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"classroom": dto})
	}
}

// listStudents handles GET /api/v1/teacher/classrooms/:id/students.
func (h *classroomHandlers) listStudents() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		students, err := h.service.ListStudents(c.Request.Context(), id, teacherID)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"students": newClassroomStudentDTOs(students)})
	}
}

// addStudents handles POST /api/v1/teacher/classrooms/:id/students.
//
// 200 with a partial-success body, never 207 and never a failure when only some
// accounts were unusable: the import either granted everything it could (with the
// refusals explained per account) or it failed for a reason that has nothing to do
// with the content of the list. See classroom.Service.AddStudents for why.
func (h *classroomHandlers) addStudents() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		var req addStudentsRequest
		if !bindClassroomJSON(c, &req) {
			return
		}
		result, err := h.service.AddStudents(c.Request.Context(), classroom.AddStudentsInput{
			ClassroomID: id,
			TeacherID:   teacherID,
			Accounts:    req.Accounts,
		})
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		rejected := make([]rejectedAccountDTO, 0, len(result.Rejected))
		for _, r := range result.Rejected {
			rejected = append(rejected, rejectedAccountDTO{Account: r.Account, Code: string(r.Code)})
		}
		RespondJSON(c, http.StatusOK, gin.H{
			"students": newClassroomStudentDTOs(result.Students),
			"rejected": rejected,
		})
	}
}

// removeStudent handles DELETE /api/v1/teacher/classrooms/:id/students/:studentId.
//
// 204 and no body: there is nothing meaningful to return about a row that no longer
// exists, and a body would only invite clients to depend on its shape.
func (h *classroomHandlers) removeStudent() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		studentID, ok := parseUUIDParam(c, "studentId")
		if !ok {
			return
		}
		if err := h.service.RemoveStudent(c.Request.Context(), id, studentID, teacherID); err != nil {
			RespondError(c, classroomError(err))
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// openClassroom handles POST /api/v1/teacher/classrooms/:id/open.
func (h *classroomHandlers) openClassroom() gin.HandlerFunc {
	return h.transition(func(ctx context.Context, id, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error) {
		return h.service.Open(ctx, id, teacherID)
	})
}

// closeClassroom handles POST /api/v1/teacher/classrooms/:id/close.
func (h *classroomHandlers) closeClassroom() gin.HandlerFunc {
	return h.transition(func(ctx context.Context, id, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error) {
		return h.service.Close(ctx, id, teacherID)
	})
}

// transition builds the handlers of the two state-changing endpoints.
//
// They differ in exactly one call, and the response shape is identical by contract:
// both answer `{"classroom": {...}, "run": {...}}`, so the frontend renders the new
// state from one place instead of branching on the endpoint. The run is the one
// that was just opened or just closed — after a close the classroom's own
// currentRun is null again, which is why the run is returned separately.
func (h *classroomHandlers) transition(apply func(ctx context.Context, id, teacherID uuid.UUID) (*classroom.Classroom, *classroom.Run, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}

		updated, run, err := apply(c.Request.Context(), id, teacherID)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		classroomDTOValue, err := newClassroomDTO(updated)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		runDTO, err := newClassroomRunDTO(run)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"classroom": classroomDTOValue, "run": runDTO})
	}
}

// newClassroomDTO renders one classroom.
//
// A nil row is refused instead of dereferenced: the service contract says a
// successful call returns a classroom, so nil-without-error is a bug, and the right
// place to discover a bug is a returned error in the request path rather than a
// panic the recovery middleware turns into an opaque 500.
func newClassroomDTO(c *classroom.Classroom) (classroomDTO, error) {
	if c == nil {
		return classroomDTO{}, errors.New("httpapi: classroom service returned no classroom")
	}
	dto := classroomDTO{
		ID:             c.ID.String(),
		Name:           c.Name,
		Description:    c.Description,
		Status:         string(c.Status),
		OwnerTeacherID: c.OwnerTeacherID.String(),
		StudentCount:   c.StudentCount,
		CreatedAt:      formatTimestamp(c.CreatedAt),
		UpdatedAt:      formatTimestamp(c.UpdatedAt),
	}
	if c.CurrentRun != nil {
		dto.CurrentRun = &classroomRunRefDTO{
			ID:       c.CurrentRun.ID.String(),
			OpenedAt: formatTimestamp(c.CurrentRun.OpenedAt),
		}
	}
	return dto, nil
}

// newClassroomRunDTO renders one run.
func newClassroomRunDTO(r *classroom.Run) (classroomRunDTO, error) {
	if r == nil {
		return classroomRunDTO{}, errors.New("httpapi: classroom service returned no run")
	}
	dto := classroomRunDTO{
		ID:          r.ID.String(),
		ClassroomID: r.ClassroomID.String(),
		Status:      string(r.Status),
		OpenedAt:    formatTimestamp(r.OpenedAt),
	}
	if r.ClosedAt != nil {
		closed := formatTimestamp(*r.ClosedAt)
		dto.ClosedAt = &closed
	}
	return dto, nil
}

// newClassroomStudentDTOs renders a roster, always as a non-nil slice.
func newClassroomStudentDTOs(students []classroom.Student) []classroomStudentDTO {
	dtos := make([]classroomStudentDTO, 0, len(students))
	for _, s := range students {
		dtos = append(dtos, classroomStudentDTO{
			ID:          s.ID.String(),
			Account:     s.Account,
			DisplayName: s.DisplayName,
			Status:      string(s.Status),
			AddedAt:     formatTimestamp(s.AddedAt),
		})
	}
	return dtos
}

// classroomError maps a service error onto the API error contract.
//
// The mapping lives in one function, exactly like adminError, so a rule cannot
// produce two different answers depending on which endpoint reached it and the
// service stays free of HTTP semantics.
//
// The interesting pair is NOT_FOUND versus NOT_OWNER (§58): "this classroom does
// not exist" and "this classroom is not yours" are different product answers with
// different UI, so they must not collapse. Distinguishing them leaks nothing — the
// ids are unguessable UUIDs and the caller is an authenticated teacher.
func classroomError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, classroom.ErrNotFound):
		return apperr.Wrap(apperr.CodeClassroomNotFound, err)
	case errors.Is(err, classroom.ErrNotOwner):
		return apperr.Wrap(apperr.CodeClassroomNotOwner, err)
	case errors.Is(err, classroom.ErrAlreadyOpen):
		// A lost race or a double click, not an incident: 409 with a code the UI can
		// answer by simply re-reading the classroom.
		return apperr.Wrap(apperr.CodeClassroomAlreadyOpen, err)
	case errors.Is(err, classroom.ErrAlreadyClosed):
		return apperr.Wrap(apperr.CodeClassroomAlreadyClosed, err)
	case errors.Is(err, classroom.ErrStudentNotAssigned):
		return apperr.Wrap(apperr.CodeStudentNotAssigned, err)
	case errors.Is(err, classroom.ErrInvalidRequest):
		// The service's own sentence is written for the teacher ("name must be at
		// most 80 characters") and describes the field that was wrong, so it is
		// forwarded verbatim; the fallback only fires for a sentinel with no message.
		message := classroom.InvalidMessage(err)
		if message == "" {
			message = apperr.DefaultMessage(apperr.CodeInvalidRequest)
		}
		return apperr.Wrap(apperr.CodeInvalidRequest, err).WithMessage(message)
	default:
		// An error that already IS an *apperr.Error travels unchanged (the DB layer
		// and the middleware build those); everything else collapses to INTERNAL with
		// the original kept as the log-only cause, which is what stops a driver error
		// from reaching a browser (§58).
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			return appErr
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
}

// teacherActorFrom returns the authenticated teacher performing this request.
//
// The id comes from the Principal that RequireSession loaded from the database,
// never from the body, a query parameter or a header. That is what makes ownership
// checking meaningful: if the caller could name itself, every teacher could act as
// every other one.
//
// A missing Principal is unreachable while the middleware order is correct; it is
// still handled explicitly and answered as 401 rather than by inventing an actor,
// because an ownership decision with no attributable actor must not happen at all.
func teacherActorFrom(c *gin.Context) (uuid.UUID, bool) {
	principal, ok := PrincipalFrom(c)
	if !ok || principal == nil {
		RespondError(c, apperr.New(apperr.CodeAuthRequired))
		return uuid.Nil, false
	}
	return principal.UserID, true
}

// classroomPath is the canonical URL of one classroom, used for Location.
func classroomPath(id uuid.UUID) string { return "/api/v1/teacher/classrooms/" + id.String() }

// parseClassroomID reads the :id path parameter.
//
// A malformed id is 400 and not 404: the caller sent something that is not an
// identifier at all, and answering "not found" would suggest a valid classroom that
// happens to be missing. Rejecting it here also keeps a value that cannot match a
// row away from the database.
func parseClassroomID(c *gin.Context) (uuid.UUID, bool) { return parseUUIDParam(c, "id") }

// parseUUIDParam reads and validates one UUID path parameter.
func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := strings.TrimSpace(c.Param(name))
	id, err := uuid.Parse(raw)
	if err != nil {
		RespondError(c, invalidClassroomRequest(name+" must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// bindClassroomJSON decodes a classroom request body strictly.
func bindClassroomJSON(c *gin.Context, target any) bool {
	return bindJSONLimit(c, target, maxClassroomBodyBytes, false)
}

// invalidClassroomRequest builds a 400 with a specific, safe message.
func invalidClassroomRequest(message string) *apperr.Error {
	return apperr.New(apperr.CodeInvalidRequest).WithMessage(message)
}

// stringValue dereferences an optional string, mapping nil to "".
//
// Used only where the service re-validates the value anyway (a missing name is
// rejected as a blank name), so the empty string never reaches the database.
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
