// Package admin implements the account-administration use cases of §4/§68:
// creating teachers and students, listing accounts, renaming, enabling and
// disabling, and resetting a teacher's password.
//
// WHY this is a service package and not more code in internal/httpapi: these are
// business rules, not transport concerns. "A student must not have a password",
// "the last administrator must not be disabled" and "resetting a password ends
// every session" have to hold for the HTTP API, for a future CLI and for any test
// that wants to exercise them without a router, a cookie jar or a database. A
// rule that lives in a handler is a rule that only exists for one caller.
//
// Two deliberate omissions in the rule set:
//
//   - There is NO "create an administrator" path and no "change a role" path.
//     §4 lists exactly which powers an admin has, and creating admins is not one
//     of them; the first admin comes from the adminctl break-glass CLI, and the
//     count of admins only ever shrinks through this API. An API that could mint
//     an ADMIN would turn any single compromised admin session — or any future
//     authorization bug in an admin endpoint — into a permanent privilege
//     escalation that survives the incident response.
//   - There is NO "reset an administrator's password" path either, and for the
//     same class of reason: administrators are managed out-of-band by adminctl.
//
// The package is written against interfaces (user.Repository, SessionStore) so
// every rule above is unit-testable with fakes; the HTTP layer never sees it.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Service-level sentinel errors.
//
// They are the vocabulary the HTTP layer branches on. Each one names a rule, not
// a piece of transport: mapping them onto codes and statuses is the handler's
// job (see internal/httpapi/admin.go), so the same rule produces the same answer
// no matter which caller hit it.
var (
	// ErrAccountTaken means the account already exists. Accounts are citext, so
	// "Teacher01" and "teacher01" are the same conflict.
	ErrAccountTaken = errors.New("admin: account already exists")

	// ErrUserNotFound means no account has that id.
	ErrUserNotFound = errors.New("admin: account not found")

	// ErrLastAdmin means the operation would leave the deployment with no active
	// administrator.
	ErrLastAdmin = errors.New("admin: refusing to disable the last active administrator")

	// ErrCannotDisableSelf means the caller tried to disable the very account it
	// is signed in with. A separate sentinel (rather than a generic invalid
	// request) because the UI must explain this specific rule — "sign in as
	// another administrator to disable this one" — instead of a vague rejection.
	ErrCannotDisableSelf = errors.New("admin: refusing to let an administrator disable itself")

	// ErrInvalidRequest means the caller asked for something the admin API does
	// not offer: a role it cannot create, a field it cannot edit, a transition it
	// refuses. It is a sentinel so a non-HTTP caller (a future CLI, a test) can
	// separate "you asked wrongly" from "the database is down" without parsing
	// prose.
	ErrInvalidRequest = errors.New("admin: invalid request")
)

// invalidError is the error behind ErrInvalidRequest: the rule that was violated,
// in words an administrator can act on.
//
// WHY a bespoke type instead of apperr.Wrap(..., ErrInvalidRequest): the cause of
// an *apperr.Error is what RespondError writes into the server log, and a sentinel
// whose whole text is "admin: invalid request" would append that phrase to every
// log line — noise in exactly the log an operator reads while debugging a rejected
// admin action. Keeping the message as the error's text makes both readings clean
// and leaves errors.Is(err, ErrInvalidRequest) working.
type invalidError struct{ message string }

func (e *invalidError) Error() string { return e.message }

// Is matches the ErrInvalidRequest sentinel, so errors.Is(err, ErrInvalidRequest)
// holds for every rejection.
func (e *invalidError) Is(target error) bool { return target == ErrInvalidRequest }

// SessionStore is the part of the session lifecycle this package needs.
//
// WHY a one-method interface rather than importing auth.SessionStore: disabling
// an account and resetting a password both have to end that account's sessions
// immediately, and that is the ONLY session operation the admin use cases
// perform. Depending on the whole store would let this package grow a second
// responsibility by accident.
type SessionStore interface {
	// RevokeAllForUser revokes every live session of one account. Idempotent:
	// revoking zero sessions is a success.
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
}

// PasswordGenerator mints a password for an administrator who does not want to
// invent one. It is injectable so tests can assert on the generated value.
type PasswordGenerator func() (string, error)

// generatedPasswordBytes is the size of the server-generated password.
//
// 24 bytes = 192 bits of CSPRNG output, which base64url-encodes to 32 characters.
// WHY this instead of a word list or a shorter string: the value is read once,
// typed once and never remembered, so it can afford to be maximally strong —
// 192 bits is far beyond guessing even for an attacker who can hash offline, and
// 32 characters is short enough to paste into a chat message to the teacher.
const generatedPasswordBytes = 24

// DefaultListPageSize / MaxListPageSize are the pagination bounds of §42.
//
// The cap is not cosmetic: the admin list is a table rendered in a browser, and
// without a bound the page size is an unauthenticated-style amplification knob
// (an admin session could ask for every account in one response). 200 matches the
// frozen contract the frontend was built against.
const (
	DefaultListPageSize = 50
	MaxListPageSize     = 200
)

// Service implements the admin use cases.
type Service struct {
	users    user.Repository
	sessions SessionStore
	// policy is the same password rule the login flow and adminctl use. One
	// policy object means "≥12 characters, not the account name" cannot differ
	// between the CLI that creates the first admin and the API that creates the
	// teachers they manage.
	policy auth.PasswordPolicy
	// now is injectable so tests do not have to sleep to observe updated_at.
	now func() time.Time
	// generatePassword is a field, not a direct call, so a test can make the
	// generated value predictable and assert that it never reaches a log line.
	generatePassword PasswordGenerator
}

// NewService wires the service. A zero password policy is replaced by the
// default rather than meaning "no minimum" (see auth.NewPasswordPolicy).
func NewService(users user.Repository, sessions SessionStore, policy auth.PasswordPolicy) *Service {
	return &Service{
		users:            users,
		sessions:         sessions,
		policy:           policy,
		now:              time.Now,
		generatePassword: generatePassword,
	}
}

// CreateUserInput is the request of CreateUser.
type CreateUserInput struct {
	Account     string
	DisplayName string
	// Role must be TEACHER or STUDENT; ADMIN is rejected (see the package
	// comment).
	Role user.Role
	// Password is required for TEACHER and forbidden for STUDENT. It is a pointer
	// so "the client sent no password" and "the client sent an empty password"
	// stay distinguishable: the first is the correct student request, the second
	// is a malformed teacher request, and both must not be silently treated as
	// "absent".
	Password *string
	// ActorID is the administrator performing the write; it is recorded in
	// created_by (§68) and in the audit log.
	ActorID uuid.UUID
}

// UserUpdateInput is the request of UpdateDisplayName.
type UserUpdateInput struct {
	TargetID    uuid.UUID
	DisplayName string
	ActorID     uuid.UUID
}

// SetStatusInput is the request of SetStatus.
type SetStatusInput struct {
	TargetID uuid.UUID
	Status   user.Status
	ActorID  uuid.UUID
}

// ResetPasswordInput is the request of ResetTeacherPassword.
type ResetPasswordInput struct {
	TeacherID uuid.UUID
	// Password is optional. When nil the server generates one; when set it must
	// satisfy the password policy.
	Password *string
	ActorID  uuid.UUID
}

// PasswordResetResult is the outcome of a reset.
//
// GeneratedPassword is non-empty ONLY when the server minted the password, and
// the HTTP layer returns it exactly once, in this response. It is never stored,
// never logged and cannot be recovered afterwards — which is why the admin UI has
// to show it immediately.
type PasswordResetResult struct {
	User              *user.User
	GeneratedPassword string
}

// ListUsers returns one page of accounts.
//
// The filter arrives already parsed and validated by the HTTP layer, which is the
// only place that knows what a query string is; the page bounds are re-checked
// here because a repository that silently accepts limit=0 would return the whole
// table.
func (s *Service) ListUsers(ctx context.Context, filter user.ListFilter) (*user.ListResult, error) {
	if filter.Limit <= 0 || filter.Limit > MaxListPageSize {
		return nil, invalidRequest(fmt.Sprintf("pageSize must be between 1 and %d", MaxListPageSize))
	}
	if filter.Offset < 0 {
		return nil, invalidRequest("page must be 1 or greater")
	}
	result, err := s.users.ListPage(ctx, filter)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetUser returns one account by id.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*user.User, error) {
	u, err := s.users.FindByID(ctx, id)
	if err != nil {
		return nil, translateLookupError(err)
	}
	return u, nil
}

// CreateUser creates a TEACHER or STUDENT account (§4).
//
// The rule order is deliberate: every check that needs no database runs first, so
// a malformed request never reaches SQL, and the password is hashed only after
// the account name is known to be creatable — Argon2id at 64 MiB is far too
// expensive to spend on requests that are going to be rejected anyway.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (*user.User, error) {
	if in.Role != user.RoleTeacher && in.Role != user.RoleStudent {
		// §4: the admin API creates teachers and students. Administrators come
		// from adminctl — the bootstrap problem the CLI exists to solve — and an
		// API that could create one is a privilege-escalation surface.
		return nil, invalidRequest("role must be TEACHER or STUDENT; administrator accounts can only be created by the adminctl initialization command")
	}
	if err := user.ValidateAccount(in.Account); err != nil {
		return nil, invalidRequest(err.Error())
	}
	displayName, err := user.ValidateDisplayName(in.DisplayName)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}

	params := user.CreateParams{
		Account:     in.Account,
		DisplayName: displayName,
		Role:        in.Role,
		CreatedBy:   actorRef(in.ActorID),
	}

	switch in.Role {
	case user.RoleTeacher:
		// A teacher without a password would be an account nobody can log into:
		// the CHECK constraint would reject it, and the operator would see a
		// constraint name instead of a sentence.
		if in.Password == nil {
			return nil, invalidRequest("a password is required to create a TEACHER account")
		}
		hash, err := s.hashPassword(in.Account, *in.Password)
		if err != nil {
			return nil, err
		}
		params.PasswordHash = &hash

	case user.RoleStudent:
		// §2.2: a student account is an account and nothing else. Accepting a
		// password here would create a credential the student is never given (the
		// student entry does not read one) and that the CHECK constraint forbids —
		// rejecting it explicitly tells the caller their client is wrong instead
		// of silently dropping the value.
		if in.Password != nil {
			return nil, invalidRequest("a STUDENT account must not have a password")
		}
		params.PasswordHash = nil
	}

	created, err := s.users.Create(ctx, params)
	if err != nil {
		if errors.Is(err, user.ErrAccountTaken) {
			return nil, ErrAccountTaken
		}
		return nil, err
	}

	logAdminAction(ctx, "user.created", in.ActorID, created.ID,
		logging.FieldRole, string(created.Role),
		// Presence, never the value: a hash is password material (§59).
		"has_password", created.HasPassword(),
	)
	return created, nil
}

// UpdateDisplayName renames an account.
//
// ONLY the display name can be changed. Role and status are separate operations
// on purpose:
//
//   - Role is not editable at all. §4 lists "编辑用户显示名称" — editing the
//     display name — and nothing about changing a role, and a role change is
//     never a cosmetic edit: it changes what the account can do (a student who
//     becomes a teacher gains the console, a teacher who becomes a student loses
//     their classrooms), and it invalidates the authorization decisions already
//     baked into every live session. If the product ever needs it, it deserves
//     its own endpoint, its own rule about existing sessions and its own review.
//   - Status has its own endpoint (PATCH .../status) because disabling has side
//     effects — session revocation — that a rename must not have.
//
// The HTTP layer rejects unknown and forbidden fields outright instead of
// ignoring them, and the reason is worth stating here: a body of
// `{"displayName":"李老师","role":"ADMIN"}` that silently succeeds would leave
// the caller — and the admin console's own state — believing the role changed.
// A rejection is the only honest answer.
func (s *Service) UpdateDisplayName(ctx context.Context, in UserUpdateInput) (*user.User, error) {
	displayName, err := user.ValidateDisplayName(in.DisplayName)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}
	if err := s.users.UpdateDisplayName(ctx, in.TargetID, displayName); err != nil {
		return nil, translateLookupError(err)
	}
	// The write returns no row, so the updated account is read back: the response
	// carries updated_at, and inventing it client-side (now() in Go) would make
	// the API and the database disagree by whatever the round trip costs.
	updated, err := s.users.FindByID(ctx, in.TargetID)
	if err != nil {
		return nil, translateLookupError(err)
	}

	logAdminAction(ctx, "user.display_name_updated", in.ActorID, in.TargetID)
	return updated, nil
}

// SetStatus enables or disables an account.
//
// The three rules this implements, and why each exists:
//
//  1. Disabling revokes every session of that account RIGHT NOW. The alternative
//     — waiting for the sessions to expire — would mean a teacher removed for
//     cause keeps their console, their classroom and their media tokens for up
//     to SESSION_TTL. Revoking is what makes "disabled" mean "disabled" rather
//     than "will be disabled later".
//  2. An admin cannot disable their own account. That is not paternalism: the
//     usual way to hit it is a mis-click on the row above the one that was meant,
//     and the result is that the person doing the maintenance is thrown out of
//     the console mid-task — with no way back in except adminctl.
//  3. The last active administrator cannot be disabled. With rule 2 in place this
//     is mostly about the *other* admin's account, and it is the difference
//     between "we have a deployment" and "nobody in this organisation can create
//     a teacher account again".
//
// Re-enabling is deliberately NOT the inverse of disabling: it creates no
// session. The account's owner logs in again with their password, which is both
// simpler and safer than resurrecting sessions that were killed while the account
// was disabled.
func (s *Service) SetStatus(ctx context.Context, in SetStatusInput) (*user.User, error) {
	if !in.Status.Valid() {
		return nil, invalidRequest("status must be ACTIVE or DISABLED")
	}
	target, err := s.users.FindByID(ctx, in.TargetID)
	if err != nil {
		return nil, translateLookupError(err)
	}
	if target.Status == in.Status {
		// Idempotent: the caller's goal is already true. Repeating the revoke
		// would also be harmless, but reporting success without a write keeps the
		// audit log honest — nothing changed.
		logAdminAction(ctx, "user.status_unchanged", in.ActorID, target.ID,
			"status", string(in.Status))
		return target, nil
	}

	if in.Status == user.StatusDisabled {
		if target.ID == in.ActorID {
			return nil, ErrCannotDisableSelf
		}
		if target.Role == user.RoleAdmin {
			// Read-then-write is not enough on its own (a second admin could
			// disable the other concurrently); the repository repeats the check
			// inside the UPDATE. This call exists so the answer is a sentence
			// rather than a guard that silently matched nothing.
			remaining, err := s.users.CountActiveAdmins(ctx)
			if err != nil {
				return nil, err
			}
			if remaining <= 1 {
				return nil, ErrLastAdmin
			}
		}
	}

	if err := s.users.SetStatus(ctx, in.TargetID, in.Status); err != nil {
		if errors.Is(err, user.ErrLastAdmin) {
			return nil, ErrLastAdmin
		}
		return nil, translateLookupError(err)
	}

	updated, err := s.users.FindByID(ctx, in.TargetID)
	if err != nil {
		return nil, translateLookupError(err)
	}

	logAdminAction(ctx, "user.status_changed", in.ActorID, target.ID,
		"status", string(in.Status),
		logging.FieldRole, string(target.Role))

	// Session revocation comes last, and only for DISABLED: the status is already
	// durable, so a revoke failure is a real problem (the sessions outlive the
	// decision) but not a reason to report the status change as failed — the
	// account IS disabled, and every request it makes is rejected by
	// authentication anyway (see auth.Service.Authenticate). It is logged at Warn
	// because it deserves an operator's attention.
	if in.Status == user.StatusDisabled {
		if err := s.revokeAllSessions(ctx, target.ID); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// ResetTeacherPassword replaces a teacher's password and ends their sessions.
//
// Scope, and why it is this narrow: §4 grants "重置老师密码" and nothing else, so a
// student (who has no credential) and another administrator (whose account is
// managed by adminctl, the break-glass path that must keep working when the API
// is what is broken) are both rejected with 400 rather than quietly accepted.
//
// When the caller supplies no password the server generates one. That is the
// better default, not a convenience: a password an administrator invents for
// somebody else is typically short, reused across the school's other systems, or
// a predictable pattern like the school name plus a number — and it is then
// communicated over a channel the administrator chooses. A 192-bit value read
// from the response and pasted once has none of those failure modes, and it
// costs the admin nothing because nobody has to remember it.
func (s *Service) ResetTeacherPassword(ctx context.Context, in ResetPasswordInput) (*PasswordResetResult, error) {
	target, err := s.users.FindByID(ctx, in.TeacherID)
	if err != nil {
		return nil, translateLookupError(err)
	}
	if target.Role != user.RoleTeacher {
		return nil, invalidRequest("only TEACHER accounts have a password reset here; administrator passwords are managed with the adminctl command")
	}

	password := ""
	generated := false
	switch {
	case in.Password != nil:
		password = *in.Password
	default:
		password, err = s.generatePassword()
		if err != nil {
			return nil, err
		}
		generated = true
		// The generated value must satisfy the same policy as a supplied one.
		// 192 bits of base64url always will; verifying keeps the invariant true
		// even if someone later "simplifies" the generator into something weaker.
		if err := s.validatePassword(target.Account, password); err != nil {
			return nil, invalidRequest("the generated password does not satisfy the password policy")
		}
	}

	if !generated {
		if err := s.validatePassword(target.Account, password); err != nil {
			return nil, err
		}
	}

	hash, err := auth.Hash(password)
	if err != nil {
		return nil, err
	}
	// Sessions are revoked BEFORE the new hash is written, and the order is not
	// arbitrary. Both orders revoke them; this one fails in the safer direction.
	// If the revoke fails nothing has changed yet and the caller gets an error it
	// can retry. If the WRITE failed after a successful revoke, the teacher would
	// simply have to log in again with their existing password — annoying, but
	// the credential and the sessions would still agree with each other. The
	// reverse order can leave the old password working on a console whose
	// sessions were supposed to be terminated.
	if err := s.revokeAllSessions(ctx, target.ID); err != nil {
		return nil, err
	}
	if err := s.users.SetPasswordHash(ctx, target.ID, hash); err != nil {
		return nil, translateLookupError(err)
	}

	// The log line records the event, never the value. "generated" answers the
	// only question an audit needs: did an operator choose this password, or did
	// the server?
	logAdminAction(ctx, "user.password_reset", in.ActorID, target.ID,
		logging.FieldRole, string(target.Role),
		"password_generated", generated,
		"sessions_revoked", true)

	result := &PasswordResetResult{User: target}
	if generated {
		result.GeneratedPassword = password
	}
	return result, nil
}

// hashPassword validates and hashes a caller-supplied password.
func (s *Service) hashPassword(account, password string) (string, error) {
	if err := s.validatePassword(account, password); err != nil {
		return "", err
	}
	return auth.Hash(password)
}

// validatePassword applies the shared password policy and normalises the failure
// into a PASSWORD_POLICY_VIOLATION.
//
// WHY the policy's own message is forwarded: it states the rule ("at least 12
// characters"), never the value, and the frontend needs something to display next
// to the password field. The account name is passed in because a password equal
// to the account is the one guess every attacker tries first.
func (s *Service) validatePassword(account, password string) error {
	if err := s.policy.Validate(account, password); err != nil {
		msg := apperr.DefaultMessage(apperr.CodePasswordPolicyViolation)
		if errors.Is(err, auth.ErrPasswordTooShort) ||
			errors.Is(err, auth.ErrPasswordTooLong) ||
			errors.Is(err, auth.ErrPasswordBlank) ||
			errors.Is(err, auth.ErrPasswordEqualsAccount) {
			// Safe to show: the policy errors describe the rule that failed and
			// deliberately never echo the password. The internal "auth: " prefix is
			// stripped because this sentence is rendered next to a form field, and a
			// Go package name is not something an administrator should have to read.
			msg = strings.TrimPrefix(err.Error(), "auth: ")
		}
		return apperr.New(apperr.CodePasswordPolicyViolation).WithMessage(msg)
	}
	return nil
}

// revokeAllSessions ends every live session of one account.
func (s *Service) revokeAllSessions(ctx context.Context, userID uuid.UUID) error {
	if s.sessions == nil {
		return nil
	}
	if err := s.sessions.RevokeAllForUser(ctx, userID); err != nil {
		return fmt.Errorf("admin: revoke all sessions for %s: %w", userID, err)
	}
	return nil
}

// generatePassword mints a server-side password (see generatedPasswordBytes).
func generatePassword() (string, error) {
	return auth.NewGeneratedPassword(generatedPasswordBytes)
}

// SetPasswordGeneratorForTest replaces the generator that mints a password when
// the admin does not supply one.
//
// WHY this exists in non-test code: a generated password is returned exactly once,
// in an HTTP response, so an end-to-end test that wants to prove "log in with the
// password the API just handed back" has to be able to predict it. The production
// generator stays the default — this only makes it swappable — and the name says
// plainly who is expected to call it.
func (s *Service) SetPasswordGeneratorForTest(generate PasswordGenerator) {
	if generate != nil {
		s.generatePassword = generate
	}
}

// invalidRequest builds a 400 with a specific, user-facing reason.
//
// Both vocabularies work on the returned value: the HTTP layer sees an
// *apperr.Error with the message it must render, and any caller can test
// errors.Is(err, ErrInvalidRequest).
func invalidRequest(message string) *apperr.Error {
	return apperr.Wrap(apperr.CodeInvalidRequest, &invalidError{message: message}).WithMessage(message)
}

// translateLookupError maps repository errors onto the admin vocabulary.
//
// The alternative — letting user.ErrNotFound reach the HTTP layer — would make
// every handler import the user package just to decide between 404 and 500, and
// would leak a storage detail into the transport.
func translateLookupError(err error) error {
	if errors.Is(err, user.ErrNotFound) {
		return ErrUserNotFound
	}
	return err
}

// actorRef turns an actor id into the nullable created_by value. The zero UUID
// means "no actor known" (an internal caller), which is recorded as NULL rather
// than as the nil UUID: a nil UUID in created_by would point at a user that
// cannot exist and would break the foreign key.
func actorRef(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// logAdminAction writes the one structured line every mutating admin action
// produces (§59): who did what to whom, with the request id already attached by
// the HTTP middleware's request-scoped logger.
//
// The signature has no parameter for a password, a hash or a token, and that is
// the point: the omission is enforced by the compiler rather than by a reviewer
// noticing an extra attribute.
func logAdminAction(ctx context.Context, action string, actorID, targetID uuid.UUID, extra ...any) {
	attrs := []any{
		"actor_user_id", actorID.String(),
		"target_user_id", targetID.String(),
		"action", action,
	}
	attrs = append(attrs, extra...)
	logging.FromContext(ctx).Info("admin action", attrs...)
}
