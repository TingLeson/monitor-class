package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
)

// StudentClassroomService is the part of *classroom.Service the student portal uses.
//
// WHY a second, two-method interface instead of the full ClassroomService: the
// student surface is read-only in Phase 4 (§70), and the smallest interface that
// describes it is the one that keeps it that way. A handler wired to the full
// service could call Open or AddStudents from a student route without anything
// objecting; wired to this one it cannot compile.
//
// The methods are named after the STUDENT's question, not after the teacher's, so
// the signature itself records that the caller is a student (there is an id in the
// first position and no owner id to check) rather than an owner.
type StudentClassroomService interface {
	ListStudentClassrooms(ctx context.Context, studentID uuid.UUID) ([]classroom.StudentClassroom, error)
	GetStudentClassroom(ctx context.Context, studentID, classroomID uuid.UUID) (*classroom.StudentClassroom, error)
}

// studentClassroomDTO is the classroom shape a STUDENT receives (§14/§70).
//
// It is NOT the teacher's classroomDTO, and the differences are the contract:
//
//   - No studentCount, no roster, no other student's id, account or name. §26 makes
//     student-to-student isolation a product requirement; a count is information
//     about other students ("there are 30 people in this lesson"), so it is absent
//     rather than filtered. A field that does not exist cannot be leaked by a later
//     query change, and this is cheaper than a redaction test that has to be
//     remembered for every new field.
//   - No ownerTeacherId, only the owner's display name: the card shows "王老师" and
//     has no use for an account identifier.
//   - No livekitRoomName. The room name belongs to the media plane and is issued
//     with the media token of Phase 6 to a participant who is entitled to join
//     (§33); publishing it in a list would hand every browser the handle to a room
//     it is not yet allowed to enter.
type studentClassroomDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Status      string  `json:"status"`
	// Teacher carries the display name as an object rather than as a flat
	// teacherDisplayName field because the card renders a person, and a nested object
	// is where the rest of that person (should a later phase ever need one) belongs
	// without renaming a member of the student DTO.
	Teacher    studentTeacherDTO     `json:"teacher"`
	CurrentRun *studentCurrentRunDTO `json:"currentRun"`
	CreatedAt  string                `json:"createdAt"`
}

// studentTeacherDTO is the whole of what a student learns about the teacher.
type studentTeacherDTO struct {
	DisplayName string `json:"displayName"`
}

// studentCurrentRunDTO is the run a classroom is currently in.
//
// `openedAt` is what the student card renders ("19:00 开始"); the run's status and
// closedAt are omitted because a current run is OPEN by definition — the database
// keeps classroom.status and the run in lockstep (classrooms_run_consistency) — so
// publishing them would invite a client to branch on a value that can only have one
// answer.
type studentCurrentRunDTO struct {
	ID       string `json:"id"`
	OpenedAt string `json:"openedAt"`
}

// studentClassroomHandlers implements the two student classroom endpoints.
type studentClassroomHandlers struct {
	service StudentClassroomService
}

func newStudentClassroomHandlers(service StudentClassroomService) *studentClassroomHandlers {
	return &studentClassroomHandlers{service: service}
}

// listClassrooms handles GET /api/v1/student/classrooms.
//
// Every classroom the student is authorized for, OPEN and CLOSED alike (§14), with
// OPEN ordered first by the repository. No paging envelope: a student is on a
// handful of rosters, and the response shape is frozen for the frontend that is
// being built against it in parallel.
func (h *studentClassroomHandlers) listClassrooms() gin.HandlerFunc {
	return func(c *gin.Context) {
		studentID, ok := studentActorFrom(c)
		if !ok {
			return
		}
		classrooms, err := h.service.ListStudentClassrooms(c.Request.Context(), studentID)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		// Built with make() so an empty list serialises as `[]` and not null: the
		// portal's empty state is a real product state ("你还没有被加入任何课堂"), and
		// a null would make every client special-case it.
		dtos := make([]studentClassroomDTO, 0, len(classrooms))
		for i := range classrooms {
			dto, err := newStudentClassroomDTO(&classrooms[i])
			if err != nil {
				RespondError(c, classroomError(err))
				return
			}
			dtos = append(dtos, dto)
		}
		RespondJSON(c, http.StatusOK, gin.H{"classrooms": dtos})
	}
}

// getClassroom handles GET /api/v1/student/classrooms/:id.
//
// 200 for a CLOSED classroom too. The status is data, not an error: §14 requires the
// card to say "暂不可进入", and refusing the read would leave the portal unable to
// render the very state it is supposed to explain. Whether the student may ENTER is
// the screen gate's question (Phase 5), and it is answered by a different endpoint.
//
// 404 STUDENT_NOT_ASSIGNED is the single failure answer here; see
// classroom.Service.GetStudentClassroom for why "no such classroom" and "not on its
// roster" deliberately share it.
func (h *studentClassroomHandlers) getClassroom() gin.HandlerFunc {
	return func(c *gin.Context) {
		studentID, ok := studentActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseClassroomID(c)
		if !ok {
			return
		}
		found, err := h.service.GetStudentClassroom(c.Request.Context(), studentID, id)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		dto, err := newStudentClassroomDTO(found)
		if err != nil {
			RespondError(c, classroomError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"classroom": dto})
	}
}

// newStudentClassroomDTO renders one classroom for a student.
//
// A nil row is refused instead of dereferenced: the service contract says a
// successful call returns a classroom, so nil-without-error is a bug, and a
// returned error is discoverable where a panic inside the recovery middleware is
// only an opaque 500.
func newStudentClassroomDTO(c *classroom.StudentClassroom) (studentClassroomDTO, error) {
	if c == nil {
		return studentClassroomDTO{}, errors.New("httpapi: classroom service returned no classroom")
	}
	dto := studentClassroomDTO{
		ID:   c.ID.String(),
		Name: c.Name,
		// Description is passed through as-is: the domain already stores nothing for
		// "no description" (classroom.NormalizeDescription turns a blank string into
		// nil), so a description here is either meaningful text or null. Mapping ""
		// to null in the handler would paper over a service that stopped normalising
		// and would make the same classroom render differently on the two surfaces.
		Description: c.Description,
		Status:      string(c.Status),
		Teacher:     studentTeacherDTO{DisplayName: c.TeacherDisplayName},
		CreatedAt:   formatTimestamp(c.CreatedAt),
	}
	if c.CurrentRun != nil {
		dto.CurrentRun = &studentCurrentRunDTO{
			ID:       c.CurrentRun.ID.String(),
			OpenedAt: formatTimestamp(c.CurrentRun.OpenedAt),
		}
	}
	return dto, nil
}

// studentActorFrom returns the authenticated student performing this request.
//
// The id comes from the Principal that RequireSession loaded from the database,
// never from a query parameter, a header or the body. That is the whole of the
// student-side authorization: a student id supplied by the client would let anyone
// read anyone's timetable, and the roster JOIN would faithfully return it.
//
// A missing Principal is unreachable while the middleware order is correct; it is
// still answered as 401 rather than by inventing an actor, because a roster decision
// with no attributable actor must not happen at all.
func studentActorFrom(c *gin.Context) (uuid.UUID, bool) {
	principal, ok := PrincipalFrom(c)
	if !ok || principal == nil {
		RespondError(c, apperr.New(apperr.CodeAuthRequired))
		return uuid.Nil, false
	}
	return principal.UserID, true
}
