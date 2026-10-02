package httpapi

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/session"
)

// StudentSessionService is the part of *session.Service the student surface uses.
//
// WHY two methods and not the whole service: the teacher half (media-token,
// monitor) is not something a student route may reach, and a handler wired to this
// interface cannot call it even by mistake. The same pattern as
// StudentClassroomService, for the same reason (§26: one student must never be able
// to learn anything about another, and the smallest interface is how that survives a
// future edit).
type StudentSessionService interface {
	Join(ctx context.Context, in session.JoinInput) (*session.JoinResult, error)
	Leave(ctx context.Context, sessionID, studentID uuid.UUID) (*session.StudentSession, error)
}

// captureRequest is the diagnostics block of §43.
//
// It is decoded, bounded and logged — and never used to decide anything. The type
// comment is the contract: if a later change makes the server branch on
// `displaySurface`, §19 is being violated (the value is the browser's own claim, and a
// modified client can send any string it likes).
type captureRequest struct {
	DisplaySurface string `json:"displaySurface"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
}

// joinRequest is the body of POST /api/v1/student/classrooms/:id/join.
//
// Every member is optional (§43): the screen gate has already run in the browser, and
// a client that sends no diagnostics must still be able to enter the lesson. `capture`
// is a pointer so "absent" and "present but empty" stay distinguishable in the log.
type joinRequest struct {
	Capture *captureRequest `json:"capture"`
}

// joinResponse is the frozen join contract: the session, the media endpoint the
// browser connects to, and a short-lived participant token.
//
// The LIVEKIT ROOM NAME is deliberately NOT here. The token already names the room,
// and publishing it separately would put a media-plane handle into browser history,
// screenshots and frontend error reports for no benefit (§33).
type joinResponse struct {
	SessionID  string `json:"sessionId"`
	LiveKitURL string `json:"livekitUrl"`
	Token      string `json:"token"`
}

// studentSessionHandlers implements the two student session endpoints.
type studentSessionHandlers struct {
	service StudentSessionService
}

func newStudentSessionHandlers(service StudentSessionService) *studentSessionHandlers {
	return &studentSessionHandlers{service: service}
}

// join handles POST /api/v1/student/classrooms/:id/join (§43).
//
// The order of the checks is the one §43 prescribes, and none of them can be skipped
// by anything the client sends:
//
//	authenticated STUDENT, ACTIVE   ← RequireSession + RequireRole (middleware)
//	the client is really that browser ← CSRF (middleware)
//	assigned to this classroom      ← the roster JOIN, in the service
//	classroom OPEN + current run    ← the service
//
// The body's `capture` block is recorded in the log as diagnostics. It is NOT part of
// that list and must never become part of it: `displaySurface === "monitor"` is a
// browser-side assertion (§19), and the server cannot verify it.
//
// 409 CLASSROOM_CLOSED for a classroom that is not running — not 404 — because the
// student is authorized and the resource exists; the lesson is simply not open, and
// the portal renders exactly that.
func (h *studentSessionHandlers) join() gin.HandlerFunc {
	return func(c *gin.Context) {
		studentID, ok := studentActorFrom(c)
		if !ok {
			return
		}
		classroomID, ok := parseClassroomID(c)
		if !ok {
			return
		}
		// The body is optional (an empty one is valid), but a member we do not know is
		// refused with the field's name. WHY strict here even though the payload is
		// diagnostics: the join contract is frozen and shared with a frontend built in
		// parallel, and a silently dropped `capture` block would mean the diagnostics
		// this endpoint exists to collect are missing exactly when they are wanted.
		var req joinRequest
		if !bindJSONLimit(c, &req, maxClassroomBodyBytes, true) {
			return
		}
		capture, ok := parseCapture(c, req.Capture)
		if !ok {
			return
		}

		result, err := h.service.Join(c.Request.Context(), session.JoinInput{
			StudentID:   studentID,
			ClassroomID: classroomID,
			Capture:     capture,
		})
		if err != nil {
			RespondError(c, sessionError(err))
			return
		}
		if result == nil || result.Session == nil {
			// A successful join always produces a session; nil-without-error is a bug,
			// and an INTERNAL error here is discoverable where a nil dereference inside
			// the recovery middleware is only an opaque 500.
			RespondError(c, apperr.New(apperr.CodeInternal))
			return
		}
		RespondJSON(c, http.StatusOK, joinResponse{
			SessionID:  result.Session.ID.String(),
			LiveKitURL: result.LiveKitURL,
			// The token is a credential. It is written to this response and nowhere
			// else: no log line, no error message, no header (§59).
			Token: result.Token,
		})
	}
}

// leave handles POST /api/v1/student/sessions/:id/leave (§43/§50).
//
// 204 with no body, and the answer does not reveal whether the session existed before
// this call: a session that was already LEFT is left alone (the first left_at is kept),
// and a session belonging to another student is answered exactly like one that does
// not exist (§58 — no probing other students' session ids).
func (h *studentSessionHandlers) leave() gin.HandlerFunc {
	return func(c *gin.Context) {
		studentID, ok := studentActorFrom(c)
		if !ok {
			return
		}
		sessionID, ok := parseUUIDParam(c, "id")
		if !ok {
			return
		}
		if _, err := h.service.Leave(c.Request.Context(), sessionID, studentID); err != nil {
			RespondError(c, sessionError(err))
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// parseCapture validates and normalises the optional diagnostics block.
//
// WHAT is checked and what is not: the shape is bounded (a short surface name, sane
// pixel counts) so a hostile client cannot use this endpoint to write megabytes of
// nonsense into the log stream, but the VALUE of displaySurface is never compared
// against "monitor". Rejecting a join because the browser said "window" would turn
// diagnostics into a gate, which §43 forbids — and it would not stop a modified client
// either.
func parseCapture(c *gin.Context, raw *captureRequest) (*session.Capture, bool) {
	if raw == nil {
		return nil, true
	}
	if utf8.RuneCountInString(raw.DisplaySurface) > maxDisplaySurfaceLength {
		RespondError(c, invalidClassroomRequest("capture.displaySurface is too long"))
		return nil, false
	}
	if raw.Width < 0 || raw.Height < 0 || raw.Width > maxCaptureDimension || raw.Height > maxCaptureDimension {
		RespondError(c, invalidClassroomRequest("capture dimensions are out of range"))
		return nil, false
	}
	return &session.Capture{
		DisplaySurface: raw.DisplaySurface,
		Width:          raw.Width,
		Height:         raw.Height,
	}, true
}

// Bounds for the diagnostics block. They exist to keep a client from turning a log
// line into a payload; they are not a security boundary (§19).
const (
	maxDisplaySurfaceLength = 64
	maxCaptureDimension     = 100000
)

// sessionError maps a session service error onto the API error contract of §58.
//
// It lives here, next to the handlers, for the same reason classroomError does: one
// function decides the code, so the same rule cannot produce two different answers
// depending on which endpoint reached it, and the service stays free of HTTP.
//
// The classroom sentinels travel through unchanged (the roster and ownership rules are
// the classroom domain's), and the session's own three collapse as follows:
//
//	classroom.ErrStudentNotAssigned → 404 STUDENT_NOT_ASSIGNED  (join: not on the roster)
//	classroom.ErrNotFound           → 404 CLASSROOM_NOT_FOUND
//	classroom.ErrNotOwner           → 403 CLASSROOM_NOT_OWNER
//	session.ErrClassroomClosed      → 409 CLASSROOM_CLOSED
//	session.ErrSessionNotFound      → 404 SESSION_NOT_FOUND
//	session.ErrMediaUnavailable     → 502 MEDIA_TOKEN_FAILED
func sessionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrClassroomClosed):
		return apperr.Wrap(apperr.CodeClassroomClosed, err)
	case errors.Is(err, session.ErrSessionNotFound):
		return apperr.Wrap(apperr.CodeSessionNotFound, err)
	case errors.Is(err, session.ErrMediaUnavailable):
		// 502: the request was fine and retrying can genuinely succeed. The cause (a
		// LiveKit endpoint, an SDK error) is logged and never returned — it would leak
		// the media plane's internals to a student (§58/§63).
		return apperr.Wrap(apperr.CodeMediaTokenFailed, err)
	case errors.Is(err, classroom.ErrStudentNotAssigned):
		return apperr.Wrap(apperr.CodeStudentNotAssigned, err)
	case errors.Is(err, classroom.ErrNotFound):
		return apperr.Wrap(apperr.CodeClassroomNotFound, err)
	case errors.Is(err, classroom.ErrNotOwner):
		return apperr.Wrap(apperr.CodeClassroomNotOwner, err)
	default:
		// Anything else — including an error that already is an *apperr.Error — is
		// passed through the same collapse the classroom surface uses, so a driver
		// error can never reach a browser.
		return classroomError(err)
	}
}
