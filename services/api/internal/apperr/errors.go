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
	case CodeAuthRequired:
		return http.StatusUnauthorized // 401 — a session could fix this
	case CodeAccountDisabled, CodeRoleForbidden, CodeClassroomNotOwner:
		return http.StatusForbidden // 403 — authenticated but not permitted
	case CodeClassroomNotFound, CodeSessionNotFound:
		// 404 rather than 403 for "exists but is not yours" only where the
		// resource id is already unguessable (UUID) and the caller has a
		// legitimate reason to distinguish "typo" from "denied".
		return http.StatusNotFound
	case CodeClassroomClosed, CodeClassroomAlreadyOpen, CodeClassroomAlreadyClosed,
		CodeStudentNotAssigned, CodeSessionAlreadyActive:
		return http.StatusConflict // 409 — the request was valid for another state
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeMediaTokenFailed:
		// 502 because the failure is in the media plane, not in the request: the
		// client's input was fine and retrying can genuinely succeed.
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
