package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is the single error type that may cross the HTTP boundary.
//
// It carries four things and nothing more:
//
//	Code    – the stable contract the frontend switches on
//	Message – user-facing prose; safe to display verbatim
//	Status  – the HTTP status that matches the code
//	Err     – the internal cause, logged but NEVER serialised
//
// WHY the internal cause is separated from the message: the cause is where SQL
// errors, DSN fragments and LiveKit responses live. Keeping them in a distinct
// field makes leaking them a deliberate act instead of an accident, and lets the
// HTTP layer render a safe body while the log keeps full detail (§58/§59).
type Error struct {
	Code    Code
	Message string
	Status  int
	Err     error
}

// Error implements the error interface. The rendering intentionally includes the
// internal cause so server-side logs stay debuggable; callers that hand an error
// to a client must go through the HTTP layer, which only ever emits Code and
// Message.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to errors.Is / errors.As so infrastructure
// code can test for context.Canceled, pgx.ErrNoRows and friends.
func (e *Error) Unwrap() error { return e.Err }

// New builds an error from a code, using the code's default message and status.
func New(code Code) *Error {
	return &Error{
		Code:    code,
		Message: DefaultMessage(code),
		Status:  HTTPStatus(code),
	}
}

// Wrap builds an error from a code while retaining the internal cause for logs.
//
// The cause is deliberately NOT appended to Message: `Wrap(CodeInternal, err)`
// must produce the same client-visible sentence as `New(CodeInternal)`, so a
// future refactor cannot accidentally turn a database error into a response body.
func Wrap(code Code, err error) *Error {
	e := New(code)
	e.Err = err
	return e
}

// WithMessage overrides the user-facing message in place and returns the same
// pointer, which keeps construction chainable:
//
//	return apperr.Wrap(apperr.CodeClassroomNotFound, err).WithMessage("...")
//
// The status is intentionally left untouched — a message must never be able to
// change the HTTP semantics of a code.
func (e *Error) WithMessage(message string) *Error {
	e.Message = message
	return e
}

// From converts any error into an *Error.
//
// Unknown errors collapse to CodeInternal, and the original error is retained
// for logging only. This is the safety net that guarantees the API never returns
// a raw driver error to a browser, no matter where in the stack the error
// originated.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Wrap(CodeInternal, err)
}

// HTTPStatus maps a business code to its HTTP status.
//
// The split matters: several distinct codes share a status (403 for "not owner"
// and "wrong role") because HTTP only describes the class of failure, while the
// code describes the exact rule that was violated. Clients must branch on Code.
func HTTPStatus(code Code) int {
	switch code {
	case CodeAuthRequired, CodeInvalidCredentials:
		// 401 — a different credential could fix this. Note that
		// INVALID_CREDENTIALS covers "no such account" as well; the status must
		// not differ between the two cases either.
		return http.StatusUnauthorized
	case CodeAccountDisabled, CodeRoleForbidden, CodeCSRFInvalid, CodeClassroomNotOwner:
		return http.StatusForbidden // 403 — authenticated but not permitted
	case CodeRateLimited:
		// 429 with a Retry-After header set by the middleware; the code alone
		// tells the frontend to back off instead of retrying in a loop.
		return http.StatusTooManyRequests
	case CodeClassroomNotFound, CodeSessionNotFound, CodeUserNotFound, CodeStudentNotFound,
		CodeStudentNotAssigned, CodeNotFound:
		// 404 rather than 403 for "exists but is not yours" only where the
		// resource id is already unguessable (UUID) and the caller has a
		// legitimate reason to distinguish "typo" from "denied".
		//
		// CodeStudentNotFound (no such account) and CodeStudentNotAssigned (the
		// account exists but is not on this roster) are both 404: in each case the
		// caller named a student who is not there, and the fix is to correct the
		// name or add them — not to log in differently.
		//
		// CodeNotFound is the transport-level member of this group: the URL named
		// something this API does not serve.
		return http.StatusNotFound
	case CodeAccountAlreadyExists:
		// 409 — the request was well formed and would have succeeded against a
		// different account name; the conflict is with existing state.
		return http.StatusConflict
	case CodeClassroomClosed, CodeClassroomAlreadyOpen, CodeClassroomAlreadyClosed,
		CodeSessionAlreadyActive, CodeTeacherMicRequired, CodePrivateTalkUnavailable:
		// 409 — the request was valid for another state. The private-talk pair belongs
		// here for the same reason CLASSROOM_CLOSED does: the body was well formed and the
		// action is legal in general, it just conflicts with what the media plane reports
		// right now (the teacher has no microphone track, or the student is not connected).
		// Retrying the identical request can genuinely succeed once that changes, which is
		// exactly what separates 409 from 400.
		return http.StatusConflict
	case CodeCannotDisableSelf, CodeLastAdminProtected:
		// 409 and not 400: the body was well formed and the action is legal in
		// general — it conflicts with the *system's* current state (which account
		// is calling, how many administrators remain). Retrying the identical
		// request cannot succeed until that state changes.
		return http.StatusConflict
	case CodeInvalidRequest, CodePasswordPolicyViolation, CodeNotAStudent:
		// PASSWORD_POLICY_VIOLATION is a 400 and not a 409: nothing conflicts with
		// existing state, the submitted value itself is unusable. The distinct
		// code exists so the frontend can attach the message to the password
		// field instead of showing a generic banner.
		//
		// NOT_A_STUDENT is a 400 for the same reason: the account named in the
		// request exists but is not usable here, and no amount of retrying or
		// waiting changes that — the teacher has to pick a different account. It is
		// reported per account inside the batch-add response, so this status is
		// only what the code would mean if it ever stood alone.
		return http.StatusBadRequest
	case CodeMediaTokenFailed:
		// 502 because the failure is in the media plane, not in the request: the
		// client's input was fine and retrying can genuinely succeed.
		return http.StatusBadGateway
	case CodeMethodNotAllowed:
		// 405 with gin's own Allow header. The code and the status agree, which is
		// the whole point of having both: a client that branches on the code sees
		// the same classification a human reading the status does.
		return http.StatusMethodNotAllowed
	case CodePayloadTooLarge:
		// 413 — the body was rejected before it was interpreted, so no business code
		// applies.
		return http.StatusRequestEntityTooLarge
	case CodeServiceUnavailable:
		// 503 is what makes a load balancer and a retrying client do the right
		// thing during a rolling deploy: this process is alive, but not for you
		// right now.
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
