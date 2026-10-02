package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/session"
)

// TeacherSessionService is the part of *session.Service the teacher surface uses.
//
// The two student methods are absent on purpose: a teacher route has no business
// joining a lesson as a student, and an interface that cannot express it is a better
// guarantee than a review comment.
type TeacherSessionService interface {
	TeacherToken(ctx context.Context, in session.TeacherTokenInput) (*session.TeacherTokenResult, error)
	Monitor(ctx context.Context, classroomID, teacherID uuid.UUID) (*session.MonitorView, error)
}

// teacherMediaTokenResponse is the frozen contract of the teacher media token:
// the endpoint and a participant token, nothing else.
//
// No sessionId, no room name, no permissions echo. The teacher's participant identity
// is their LOGIN session id (§44) and stays server-side; the token already carries it,
// and returning it would publish an internal identifier the console has no use for.
type teacherMediaTokenResponse struct {
	LiveKitURL string `json:"livekitUrl"`
	Token      string `json:"token"`
}

// monitorResponse is the body of the monitoring endpoint (§51): a list of tiles.
//
// No envelope, no counts, no run id: the teacher console polls this per tile grid, and
// everything the wall renders is already inside each student object.
type monitorResponse struct {
	Students []monitorStudentDTO `json:"students"`
}

// monitorStudentDTO is one tile of §51, extended in Phase 7 to cover the students who
// have not entered yet.
//
// WHY sessionId is exposed even though the tile is keyed by studentId: the wall has to
// be able to leave a session (and, from Phase 8, to subscribe to one session's events),
// and the session is the thing that has an identity in the media plane. The display
// name is the only piece of account data here — no account, no email (§26 is about
// students seeing each other, but a teacher's wall has no use for an account name
// either).
//
// WHY sessionId and sessionStatus are nullable, and why that is a contract and not a
// convenience: the list is the classroom ROSTER, which includes students who were
// authorized for the lesson and never pressed "进入课堂" (§29's "18 / 25" needs the
// denominator). Such a student has no session, so both members are null — a real,
// renderable state ("未进入"), not missing data. There is deliberately NO seventh
// status for it: a client that reads sessionStatus == null as "not in this lesson" is
// reading the whole truth, and a "NOT_JOINED" value would have to be filtered out of
// every state machine that already handles the six states of §12.
type monitorStudentDTO struct {
	StudentID     string          `json:"studentId"`
	DisplayName   string          `json:"displayName"`
	SessionID     *string         `json:"sessionId"`
	SessionStatus *string         `json:"sessionStatus"`
	Screen        monitorTrackDTO `json:"screen"`
	Camera        monitorTrackDTO `json:"camera"`
	Microphone    monitorTrackDTO `json:"microphone"`
	Connection    string          `json:"connection"`
	JoinedAt      *string         `json:"joinedAt"`
	LastEventAt   *string         `json:"lastEventAt"`
}

// monitorTrackDTO is the per-media block of §51.
//
// An object with one boolean rather than a bare boolean, because the shape is where
// Phase 9/10 will add what the wall needs next (a muted flag, a resolution) without
// renaming a member the frontend already reads.
type monitorTrackDTO struct {
	Active bool `json:"active"`
}

// teacherSessionHandlers implements the two teacher media endpoints.
type teacherSessionHandlers struct {
	service TeacherSessionService
}

func newTeacherSessionHandlers(service TeacherSessionService) *teacherSessionHandlers {
	return &teacherSessionHandlers{service: service}
}

// mediaToken handles POST /api/v1/teacher/classrooms/:id/media-token (§27/§43).
//
// The caller's identity in the room is their login session id, taken from the
// Principal — never from the body, and never the teacher's account or name (§44). The
// grants are the narrowest set §27 allows: subscribe to the students, publish only a
// microphone.
//
// 409 CLASSROOM_CLOSED for a classroom that is not running and 403
// CLASSROOM_NOT_OWNER for one belonging to another teacher — two different product
// answers, so they are two different codes (§58).
func (h *teacherSessionHandlers) mediaToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacher, ok := principalActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		result, err := h.service.TeacherToken(c.Request.Context(), session.TeacherTokenInput{
			ClassroomID: classroomID,
			TeacherID:   teacher.UserID,
			// The login session id is the media identity (§44). It is a UUID that
			// nobody can map back to a person without the database, which is the same
			// property the students' identities have — a teacher is not more
			// identifiable in the room than the students are.
			SessionID: teacher.SessionID,
		})
		if err != nil {
			RespondError(c, sessionError(err))
			return
		}
		if result == nil {
			RespondError(c, apperr.New(apperr.CodeInternal))
			return
		}
		// The token is written to the response and never logged (§59).
		RespondJSON(c, http.StatusOK, teacherMediaTokenResponse{
			LiveKitURL: result.LiveKitURL,
			Token:      result.Token,
		})
	}
}

// monitor handles GET /api/v1/teacher/classrooms/:id/monitor (§51).
//
// # What this endpoint is, and what it is not
//
// It is a READ that also ADVANCES state: the business fields come from PostgreSQL, the
// media fields from a server-side observation of the LiveKit room, and the observation
// is folded back into `student_sessions` before the response is built. That is the
// Phase 6 answer to §45/§46 ("the server decides who is online"), and it is a polling
// design on purpose: Phase 8 replaces the poll with signed LiveKit webhooks, at which
// point this endpoint becomes a pure read and the transition function moves to the
// webhook handler. The DTO does not change either way.
//
// # Why a media-plane failure is a 200
//
// When the room cannot be observed, the response says so per student
// (connection=UNKNOWN) and NOTHING is advanced. Returning 502 instead would make the
// teacher's wall an error page during a LiveKit hiccup — while the control plane is
// perfectly able to answer "who is in this lesson?" from its own rows. And writing
// DISCONNECTED for everyone would turn a media outage into a permanent business fact,
// which is the one thing §33 forbids. The distinction is visible in the response, so a
// frontend can render a "连接状态未知" hint rather than an empty wall.
func (h *teacherSessionHandlers) monitor() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		view, err := h.service.Monitor(c.Request.Context(), classroomID, teacherID)
		if err != nil {
			RespondError(c, sessionError(err))
			return
		}
		if view == nil {
			RespondError(c, apperr.New(apperr.CodeInternal))
			return
		}
		RespondJSON(c, http.StatusOK, monitorResponse{Students: newMonitorStudentDTOs(view.Students)})
	}
}

// newMonitorStudentDTOs renders the wall, always as a non-nil slice.
//
// An empty roster answers `"students": []` and not null: "this classroom authorizes
// nobody yet" is a real state of the wall (the teacher opened the lesson before adding
// students), and a null would make every client special-case it before rendering its own
// empty state.
//
// The nullable members are rendered from the nullable domain fields, and the mapping is
// one-to-one on purpose: the DTO has no fallback of its own, so "no session" cannot
// become a zero UUID or a status nobody stored (§29).
func newMonitorStudentDTOs(students []session.MonitorStudent) []monitorStudentDTO {
	dtos := make([]monitorStudentDTO, 0, len(students))
	for _, student := range students {
		dtos = append(dtos, monitorStudentDTO{
			StudentID:     student.StudentID.String(),
			DisplayName:   student.DisplayName,
			SessionID:     formatOptionalUUID(student.SessionID),
			SessionStatus: formatOptionalStatus(student.Status),
			Screen:        monitorTrackDTO{Active: student.ScreenActive},
			Camera:        monitorTrackDTO{Active: student.CameraActive},
			Microphone:    monitorTrackDTO{Active: student.MicrophoneActive},
			Connection:    string(student.Connection),
			JoinedAt:      formatOptionalTimestamp(student.JoinedAt),
			LastEventAt:   formatOptionalTimestamp(student.LastEventAt),
		})
	}
	return dtos
}

// formatOptionalUUID renders a nullable identifier as its string form or JSON null.
func formatOptionalUUID(value *uuid.UUID) *string {
	if value == nil {
		return nil
	}
	formatted := value.String()
	return &formatted
}

// formatOptionalStatus renders a nullable session status as its wire value or JSON null.
//
// The status is a *session.Status rather than a string so an invented value cannot be
// written here: whatever reaches the response was produced by the state machine of §12.
func formatOptionalStatus(value *session.Status) *string {
	if value == nil {
		return nil
	}
	formatted := string(*value)
	return &formatted
}

// formatOptionalTimestamp renders a nullable timestamp as RFC3339 or JSON null.
func formatOptionalTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatTimestamp(*value)
	return &formatted
}

// principalActorFrom returns the whole authenticated principal.
//
// WHY this exists next to teacherActorFrom/studentActorFrom: the teacher media token
// needs the LOGIN SESSION id, not only the user id (§44). Those two are different
// values with different lifetimes — the user outlives the session — and a handler that
// reached for the user id as the media identity would give a teacher a stable identity
// across every lesson, which is both a correlation handle and unnecessary.
func principalActorFrom(c *gin.Context) (*auth.Principal, bool) {
	principal, ok := PrincipalFrom(c)
	if !ok || principal == nil {
		RespondError(c, apperr.New(apperr.CodeAuthRequired))
		return nil, false
	}
	return principal, true
}
