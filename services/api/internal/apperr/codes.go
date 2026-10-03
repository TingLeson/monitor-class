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
	// CodeInvalidCredentials is returned for BOTH "no such account" and "wrong
	// password" on a password login, with the same code and the same message.
	// WHY: telling them apart turns the login form into an account oracle —
	// an attacker learns which accounts exist (and therefore which ones are worth
	// targeting) before ever guessing a password. The information is deliberately
	// collapsed for the client and kept, with full detail, in the server log.
	CodeInvalidCredentials Code = "INVALID_CREDENTIALS"

	// CodeRateLimited is returned when a client exceeds a rate limit. §2.2 makes
	// rate limiting mandatory because a student account is "know the account and
	// you are in"; the limit is what turns unlimited guessing into an impractical
	// number of attempts.
	CodeRateLimited Code = "RATE_LIMITED"

	// CodeCSRFInvalid is returned when an authenticated unsafe request does not
	// carry the session's CSRF token. 403 — the caller is authenticated, the
	// request is simply not allowed (§63).
	CodeCSRFInvalid Code = "CSRF_INVALID"

	// Classroom domain.
	CodeClassroomNotFound      Code = "CLASSROOM_NOT_FOUND"
	CodeClassroomNotOwner      Code = "CLASSROOM_NOT_OWNER"
	CodeClassroomClosed        Code = "CLASSROOM_CLOSED"
	CodeClassroomAlreadyOpen   Code = "CLASSROOM_ALREADY_OPEN"
	CodeClassroomAlreadyClosed Code = "CLASSROOM_ALREADY_CLOSED"
	CodeStudentNotAssigned     Code = "STUDENT_NOT_ASSIGNED"

	// CodeStudentNotFound and CodeNotAStudent are the per-account refusals of the
	// batch roster import (§11/§69).
	//
	// WHY the batch endpoint needs codes of its own instead of one "some accounts
	// failed": the response is a list of {account, code} pairs, and the teacher's
	// next action is different for each — fix a typo (STUDENT_NOT_FOUND), use a
	// student account instead of a colleague's (NOT_A_STUDENT), or ask an
	// administrator to re-enable the account (ACCOUNT_DISABLED). A single generic
	// code would push the frontend into parsing the message to tell them apart,
	// which is exactly what §58 forbids.
	CodeStudentNotFound Code = "STUDENT_NOT_FOUND"
	CodeNotAStudent     Code = "NOT_A_STUDENT"

	// Student session lifecycle.
	CodeSessionAlreadyActive Code = "SESSION_ALREADY_ACTIVE"
	CodeSessionNotFound      Code = "SESSION_NOT_FOUND"

	// Admin user management (§4/§68).
	//
	// CodeUserNotFound and CodeAccountAlreadyExists are separate from
	// CodeInvalidRequest because the admin UI has a concrete thing to do with
	// each: "this account disappeared, refresh the list" versus "pick another
	// account name" pinned to the account input. Collapsing them into one generic
	// 400 would make the frontend re-implement the distinction by parsing prose.
	CodeUserNotFound            Code = "USER_NOT_FOUND"
	CodeAccountAlreadyExists    Code = "ACCOUNT_ALREADY_EXISTS"
	CodePasswordPolicyViolation Code = "PASSWORD_POLICY_VIOLATION"

	// CodeCannotDisableSelf and CodeLastAdminProtected both answer "why was this
	// refused?" for a request that is perfectly well formed.
	//
	// WHY they are not CodeInvalidRequest: the admin UI has to *explain* the
	// refusal ("you cannot disable the account you are logged in with" versus
	// "at least one administrator must stay active"), and the only alternative
	// would be to render a backend English sentence inside a Chinese interface or
	// to hard-code the guess in the frontend. Codes are the contract clients
	// branch on (§58) — rules users must understand deserve their own code.
	CodeCannotDisableSelf  Code = "CANNOT_DISABLE_SELF"
	CodeLastAdminProtected Code = "LAST_ADMIN_PROTECTED"

	// Media plane.
	CodeMediaTokenFailed Code = "MEDIA_TOKEN_FAILED"

	// Private talk (§31/§76).
	//
	// WHY these two are codes of their own instead of one "PRIVATE_TALK_FAILED": the
	// teacher's next action is different for each, and both are refusals of a request
	// that was perfectly well formed.
	//
	//   - TEACHER_MIC_REQUIRED means the teacher has not published a microphone track.
	//     Nothing is wrong with the student or the lesson; the teacher has to press
	//     "开启麦克风" in their own console. Silently succeeding here would be the worst
	//     outcome of the whole phase: the teacher would believe a student can hear them
	//     while nobody is subscribed to anything (§31).
	//   - PRIVATE_TALK_UNAVAILABLE means the named student cannot be talked to right now
	//     (no active session). The teacher has to pick somebody else or wait.
	CodeTeacherMicRequired     Code = "TEACHER_MIC_REQUIRED"
	CodePrivateTalkUnavailable Code = "PRIVATE_TALK_UNAVAILABLE"

	// Generic transport-level failures.
	CodeInvalidRequest Code = "INVALID_REQUEST"
	CodeInternal       Code = "INTERNAL"

	// CodeNotFound and CodeMethodNotAllowed are the transport-level answers to
	// "there is nothing here".
	//
	// WHY they are separate from CodeInvalidRequest even though the HTTP status
	// already distinguishes them: the frontend branches on the CODE, never on the
	// status (§58). A single generic code would push it into reading the status
	// again — the exact coupling the error contract exists to remove — and the two
	// cases ask for different UX (a 404 is "this screen does not exist", a 405 is
	// "the client and the server disagree about this endpoint").
	//
	// They are also additive: INVALID_REQUEST keeps its meaning for a malformed or
	// refused request to a route that does exist.
	CodeNotFound         Code = "NOT_FOUND"
	CodeMethodNotAllowed Code = "METHOD_NOT_ALLOWED"

	// CodePayloadTooLarge is returned when a request body exceeds
	// HTTP_MAX_BODY_BYTES (§63). It is its own code because the client's action is
	// specific — send less data — and because it must be distinguishable from a
	// malformed body in the logs: one is an over-eager client, the other is a
	// client/server contract mismatch.
	CodePayloadTooLarge Code = "PAYLOAD_TOO_LARGE"

	// CodeServiceUnavailable is returned while the process is draining for
	// shutdown (§62). A request that arrives on a connection the load balancer has
	// not yet retired must get an explicit, retryable answer instead of being
	// silently dropped or served by a process that is about to exit.
	CodeServiceUnavailable Code = "SERVICE_UNAVAILABLE"
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

	// One sentence for "unknown account" and "wrong password" alike: any
	// difference in wording would leak exactly what CodeInvalidCredentials is
	// designed to hide.
	CodeInvalidCredentials: "The account or password is incorrect.",
	CodeRateLimited:        "Too many requests. Please try again later.",
	// Deliberately actionable but non-technical: the frontend can offer "reload
	// the page" without explaining CSRF to a student.
	CodeCSRFInvalid: "Your session could not be verified for this action. Please reload the page and try again.",

	CodeClassroomNotFound:      "Classroom not found.",
	CodeClassroomNotOwner:      "You are not the owner of this classroom.",
	CodeClassroomClosed:        "This classroom is closed.",
	CodeClassroomAlreadyOpen:   "This classroom is already open.",
	CodeClassroomAlreadyClosed: "This classroom is already closed.",
	CodeStudentNotAssigned:     "This student is not assigned to this classroom.",
	// Written for the batch-import result list, where they sit next to the account
	// they describe. They stay factual and short because the frontend renders the
	// code's own localised text; these are the fallbacks.
	CodeStudentNotFound: "No account exists with this account name.",
	CodeNotAStudent:     "This account is not a student account.",

	CodeSessionAlreadyActive: "An active session already exists for this account.",
	CodeSessionNotFound:      "Session not found.",

	// Admin user management. The messages stay generic on purpose: the service
	// attaches a specific, non-sensitive sentence where one exists (a password
	// rule, "you cannot disable your own account"), and these are the fallbacks.
	CodeUserNotFound:            "Account not found.",
	CodeAccountAlreadyExists:    "This account name is already taken.",
	CodePasswordPolicyViolation: "The password does not meet the password policy.",
	CodeCannotDisableSelf:       "You cannot disable the account you are signed in with.",
	CodeLastAdminProtected:      "At least one administrator must stay active.",

	CodeMediaTokenFailed: "Unable to join the media room. Please try again.",

	// Written as instructions, not as diagnoses: the teacher can act on both without
	// knowing anything about LiveKit. The frontend renders its own localised text; these
	// are the fallbacks (§58).
	CodeTeacherMicRequired:     "Turn on your microphone before starting a private talk.",
	CodePrivateTalkUnavailable: "This student is not available for a private talk right now.",

	CodeInvalidRequest: "The request is invalid.",
	CodeInternal:       "An internal error occurred.",

	// Deliberately terse: a 404 that describes what IS there is a free map of the
	// attack surface, and a 405 that lists the allowed methods is a free map of the
	// endpoints. The Allow header carries what HTTP requires; the message does not.
	CodeNotFound:         "This endpoint does not exist.",
	CodeMethodNotAllowed: "This method is not allowed for this endpoint.",

	// Actionable without being technical: the client can retry with a smaller
	// payload (or the operator can raise HTTP_MAX_BODY_BYTES).
	CodePayloadTooLarge: "The request body is too large.",

	// Short and retryable: during a rolling deploy this is a transient answer, and
	// the frontend's reconnect/backoff path is the correct reaction.
	CodeServiceUnavailable: "The service is restarting. Please try again in a moment.",
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
