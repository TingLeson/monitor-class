package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/session"
)

// This file is the HTTP surface of §31: the three private-talk endpoints.
//
//	POST   /api/v1/teacher/classrooms/:id/private-talk   body {"studentId": uuid}
//	  → 200 {"target": {"studentId","displayName","sessionId"}}
//	DELETE /api/v1/teacher/classrooms/:id/private-talk
//	  → 204
//	GET    /api/v1/teacher/classrooms/:id/private-talk
//	  → 200 {"target": {...} | null}
//
// The shapes are frozen because the frontend is written against them in parallel. Every
// one of them is a teacher-only route: it lives on the teacher group, behind
// RequireSession(TEACHER) + RequireRole(TEACHER) + CSRF for the two writes, exactly like
// the media-token endpoint.

// PrivateTalkService is the part of *session.Service the private-talk surface uses.
//
// WHY a third narrow interface rather than widening TeacherSessionService: the state
// machine of §31 is a different subject from the media token and the monitor read, and a
// handler wired to this interface cannot mint a token or advance a session by mistake. The
// same pattern as StudentSessionService and TeacherSessionService.
type PrivateTalkService interface {
	StartPrivateTalk(ctx context.Context, in session.StartPrivateTalkInput) (*session.PrivateTalkView, error)
	StopPrivateTalk(ctx context.Context, in session.StopPrivateTalkInput) error
	PrivateTalk(ctx context.Context, classroomID, teacherID uuid.UUID) (*session.PrivateTalkView, error)
}

// privateTalkRequest is the body of the POST: one student, and nothing else.
//
// No teacher id, no session id, no classroom id, and no display name: the caller is the
// authenticated teacher (their id and login session come from the Principal) and the
// classroom comes from the path. Accepting any of those from the body would let a request
// name somebody else's student, or start a talk under another teacher's identity.
type privateTalkRequest struct {
	StudentID string `json:"studentId"`
}

// privateTalkResponse is the frozen read/start contract: `{"target": … | null}`.
//
// WHY the target is always a wrapper object and never a bare student: the IDLE state of
// §31 has to be expressible, and `null` is how it is expressed. A client can therefore
// write `if (body.target) { … }` for both the read and the start, and a future field on the
// target (a `startedAt`, say) does not change the shape.
type privateTalkResponse struct {
	Target *privateTalkTargetDTO `json:"target"`
}

// privateTalkTargetDTO is one active private talk as the teacher's console renders it.
//
// It carries exactly the three values the console needs: which student (to mark the card),
// which session (the media identity the console subscribes to in order to hear the student
// — §44), and the display name (so the console does not have to join two responses to say
// who is being talked to). The teacher's own media identity is deliberately absent: the
// console already has it, and publishing it would put a media-plane handle into a response
// body (§33/§44).
type privateTalkTargetDTO struct {
	StudentID   string `json:"studentId"`
	DisplayName string `json:"displayName"`
	SessionID   string `json:"sessionId"`
}

// privateTalkHandlers implements the three endpoints.
type privateTalkHandlers struct {
	service PrivateTalkService
}

func newPrivateTalkHandlers(service PrivateTalkService) *privateTalkHandlers {
	return &privateTalkHandlers{service: service}
}

// start handles POST /api/v1/teacher/classrooms/:id/private-talk (§31).
//
// # What the refusals mean, and why each is its own code
//
//	409 TEACHER_MIC_REQUIRED      the teacher has not published a microphone. The fix is in
//	                              the teacher's own console ("请先开启你的麦克风"), and the
//	                              request must NOT appear to succeed: a teacher who thinks
//	                              they are talking while nobody is subscribed has been lied
//	                              to by the system (§31).
//	409 PRIVATE_TALK_UNAVAILABLE  the student cannot be talked to right now (no active
//	                              session, or not in the media room).
//	409 CLASSROOM_CLOSED          the lesson is not running.
//	403 CLASSROOM_NOT_OWNER       somebody else's classroom.
//	404 STUDENT_NOT_ASSIGNED      the student is not on this classroom's roster.
//	502 MEDIA_TOKEN_FAILED        the media plane could not be reached or refused the
//	                              subscription. A start is a PROMISE, so it fails loudly.
//
// The body is one field, and a body that names no student is a 400 — not an empty string
// student id, which would be a lookup that can never match and a confusing 404.
func (h *privateTalkHandlers) start() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		var body privateTalkRequest
		if !bindClassroomJSON(c, &body) {
			return
		}
		studentID, err := uuid.Parse(body.StudentID)
		if err != nil {
			RespondError(c, invalidClassroomRequest("studentId must be a UUID"))
			return
		}

		result, err := h.service.StartPrivateTalk(c.Request.Context(), session.StartPrivateTalkInput{
			ClassroomID: classroomID,
			TeacherID:   principal.UserID,
			// §44: the teacher's participant identity is their LOGIN session id, taken from
			// the Principal and never from the body.
			TeacherSessionID: principal.SessionID,
			// §25: the student is told WHOSE lesson it is, so the name comes from the
			// authenticated account and not from the request.
			TeacherDisplayName: principal.DisplayName,
			StudentID:          studentID,
		})
		if err != nil {
			RespondError(c, sessionError(err))
			return
		}
		RespondJSON(c, http.StatusOK, newPrivateTalkResponse(result))
	}
}

// stop handles DELETE /api/v1/teacher/classrooms/:id/private-talk (§31).
//
// 204 with no body, and idempotent: a classroom that is not talking to anybody is an
// end state, not an error. This is the endpoint the console calls when the teacher
// presses "结束语音沟通" and, defensively, when the console is being torn down — a teardown
// call that answered 4xx would leave the frontend showing an error for something that is
// already true.
func (h *privateTalkHandlers) stop() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		if err := h.service.StopPrivateTalk(c.Request.Context(), session.StopPrivateTalkInput{
			ClassroomID:      classroomID,
			TeacherID:        principal.UserID,
			TeacherSessionID: principal.SessionID,
		}); err != nil {
			RespondError(c, sessionError(err))
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// show handles GET /api/v1/teacher/classrooms/:id/private-talk (§31).
//
// It is mounted on the READ side of the teacher group: it changes no state from the
// client's point of view (the reconciliation happens on the monitor poll), and the console
// calls it on load to render the button's current state. `target: null` is the IDLE answer.
func (h *privateTalkHandlers) show() gin.HandlerFunc {
	return func(c *gin.Context) {
		teacherID, ok := teacherActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		view, err := h.service.PrivateTalk(c.Request.Context(), classroomID, teacherID)
		if err != nil {
			RespondError(c, sessionError(err))
			return
		}
		RespondJSON(c, http.StatusOK, newPrivateTalkResponse(view))
	}
}

// newPrivateTalkResponse renders the frozen wrapper, always as an object.
//
// A nil view or a nil target becomes `{"target": null}` — never `null` and never a missing
// member: the frontend reads `body.target` unconditionally.
func newPrivateTalkResponse(view *session.PrivateTalkView) privateTalkResponse {
	if view == nil || view.Target == nil {
		return privateTalkResponse{}
	}
	return privateTalkResponse{Target: &privateTalkTargetDTO{
		StudentID:   view.Target.StudentID.String(),
		DisplayName: view.Target.DisplayName,
		SessionID:   view.Target.SessionID.String(),
	}}
}
