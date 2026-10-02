// Package apperr defines ClassWatch's machine-readable API error contract.
//
// WHY this exists as its own package instead of ad-hoc HTTP status codes:
// the frontend is a UX layer, not a security boundary. It decides what to render
// (a "this classroom is closed" page, a redirect to login, a retry button) from
// the stable `error.code` string, while the server keeps deciding what is
// actually allowed. Any permission or state rule enforced only in the browser
// can be bypassed with curl, so the two sides agree on codes, and the backend
// remains the only authority (§37/§58/§63).
package apperr

// Code is the stable, machine-readable identifier of a business error.
//
// Codes are part of the public API contract: the moment a frontend branch
// depends on one, renaming it is a breaking change. Messages are not stable —
// they are localisable prose for humans.
type Code string

// Control Plane error codes. The set mirrors the business rules enumerated in
// the task book §58; anything not listed here is not a contract and should not
// be invented ad hoc.
const (
	// Authentication and authorization.
	CodeAuthRequired    Code = "AUTH_REQUIRED"    // no valid session
	CodeAccountDisabled Code = "ACCOUNT_DISABLED" // session valid, account switched off
	CodeRoleForbidden   Code = "ROLE_FORBIDDEN"   // wrong role for this endpoint

	// Classroom domain.
	CodeClassroomNotFound      Code = "CLASSROOM_NOT_FOUND"
	CodeClassroomNotOwner      Code = "CLASSROOM_NOT_OWNER"
	CodeClassroomClosed        Code = "CLASSROOM_CLOSED"
	CodeClassroomAlreadyOpen   Code = "CLASSROOM_ALREADY_OPEN"
	CodeClassroomAlreadyClosed Code = "CLASSROOM_ALREADY_CLOSED"
	CodeStudentNotAssigned     Code = "STUDENT_NOT_ASSIGNED"

	// Student session lifecycle.
	CodeSessionAlreadyActive Code = "SESSION_ALREADY_ACTIVE"
	CodeSessionNotFound      Code = "SESSION_NOT_FOUND"

	// Media plane.
	CodeMediaTokenFailed Code = "MEDIA_TOKEN_FAILED"

	// Generic transport-level failures.
	CodeInvalidRequest Code = "INVALID_REQUEST"
	CodeInternal       Code = "INTERNAL"
)

// defaultMessages maps every code to a short, non-technical sentence.
//
// WHY they are deliberately vague about causes: an error message is attacker
// visible. "Classroom does not belong to you" and "classroom not found" must
// look different only where the product needs them to; neither may leak database
// errors, SQL, LiveKit room names, stack traces or whether an account exists.
var defaultMessages = map[Code]string{
	CodeAuthRequired:    "Authentication is required.",
	CodeAccountDisabled: "This account is disabled.",
	CodeRoleForbidden:   "Your role is not allowed to perform this action.",

	CodeClassroomNotFound:      "Classroom not found.",
	CodeClassroomNotOwner:      "You are not the owner of this classroom.",
	CodeClassroomClosed:        "This classroom is closed.",
	CodeClassroomAlreadyOpen:   "This classroom is already open.",
	CodeClassroomAlreadyClosed: "This classroom is already closed.",
	CodeStudentNotAssigned:     "You are not assigned to this classroom.",

	CodeSessionAlreadyActive: "An active session already exists for this account.",
	CodeSessionNotFound:      "Session not found.",

	CodeMediaTokenFailed: "Unable to join the media room. Please try again.",

	CodeInvalidRequest: "The request is invalid.",
	CodeInternal:       "An internal error occurred.",
}

// DefaultMessage returns the user-facing message for a code.
//
// It never returns an empty string: an unknown code would otherwise produce a
// response with no `message` field, which is a frontend crash waiting to happen.
func DefaultMessage(code Code) string {
	if msg, ok := defaultMessages[code]; ok {
		return msg
	}
	return defaultMessages[CodeInternal]
}
