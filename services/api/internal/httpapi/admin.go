package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/admin"
	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// AdminService is the part of *admin.Service the HTTP layer uses.
//
// WHY an interface: the behaviour that most needs testing here is the transport
// contract — which middleware runs, which status and code a rejected request
// gets, what the JSON looks like, and what happens when a forbidden field is
// smuggled into a PATCH body. None of that needs PostgreSQL, and every test that
// needs it anyway is not written.
//
// The methods return *user.User (the domain row, which carries PasswordHash) and
// never a DTO. Serialisation is the handler's job, and the DTO types below have
// no password field at all, so a leak would have to be written deliberately.
type AdminService interface {
	ListUsers(ctx context.Context, filter user.ListFilter) (*user.ListResult, error)
	CreateUser(ctx context.Context, in admin.CreateUserInput) (*user.User, error)
	GetUser(ctx context.Context, id uuid.UUID) (*user.User, error)
	UpdateDisplayName(ctx context.Context, in admin.UserUpdateInput) (*user.User, error)
	SetStatus(ctx context.Context, in admin.SetStatusInput) (*user.User, error)
	ResetTeacherPassword(ctx context.Context, in admin.ResetPasswordInput) (*admin.PasswordResetResult, error)
}

// maxAdminBodyBytes bounds an admin request body.
//
// These endpoints are authenticated, so this is not the hostile-input bound the
// login handler needs; it is the bound that keeps a buggy client from streaming
// a file into an Argon2id call. The largest legitimate body is a password (128
// bytes) plus a display name (64 characters, up to 256 bytes as UTF-8): 16 KiB is
// two orders of magnitude of headroom.
const maxAdminBodyBytes = 16 << 10

// maxSearchQueryLength bounds `q`. The value goes into an ILIKE pattern, so an
// unbounded one is a free way to make the database scan-and-compare megabytes per
// row; no admin searching for a person types more than a name.
const maxSearchQueryLength = 128

// adminUserDTO is the account shape the admin API returns (§42).
//
// WHY not the userDTO that the auth endpoints return: the two are different
// contracts, and the frontend types them separately (packages/shared-types
// deliberately keeps AuthUser and the admin list user apart). This one adds
// updatedAt, which the admin list needs to show "last modified", and omits
// nothing else. It carries no password hash, no createdBy and no session data:
// a DTO that does not have a field cannot leak it, whatever a later refactor of
// user.User does (§9/§58).
type adminUserDTO struct {
	ID          string  `json:"id"`
	Account     string  `json:"account"`
	DisplayName string  `json:"displayName"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	LastLoginAt *string `json:"lastLoginAt"`
}

// newAdminUserDTO renders one account.
//
// It refuses a nil row instead of dereferencing it. The service contract says a
// successful call returns an account, so nil-without-error is a bug — and the
// right place to discover a bug is a returned error in the request path, not a
// panic that the recovery middleware turns into an opaque 500.
func newAdminUserDTO(u *user.User) (adminUserDTO, error) {
	if u == nil {
		return adminUserDTO{}, errors.New("httpapi: admin service returned no user")
	}
	dto := adminUserDTO{
		ID:          u.ID.String(),
		Account:     u.Account,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		Status:      string(u.Status),
		CreatedAt:   formatTimestamp(u.CreatedAt),
		UpdatedAt:   formatTimestamp(u.UpdatedAt),
	}
	if u.LastLoginAt != nil {
		last := formatTimestamp(*u.LastLoginAt)
		dto.LastLoginAt = &last
	}
	return dto, nil
}

// listUsersResponse is the frozen list contract: the page, its size and the total
// the filter matched, so the frontend can render "1-50 of 123" without a second
// request.
type listUsersResponse struct {
	Users    []adminUserDTO `json:"users"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
}

// createUserRequest is the body of POST /admin/users.
//
// Password is a pointer so "absent" and `""` are distinguishable: an absent
// password is correct for a student and an error for a teacher, while an empty
// string is a policy violation for a teacher — and neither may be confused with
// the other (see admin.CreateUserInput).
type createUserRequest struct {
	Account     string  `json:"account"`
	DisplayName string  `json:"displayName"`
	Role        string  `json:"role"`
	Password    *string `json:"password"`
}

// updateUserRequest is the body of PATCH /admin/users/:id.
//
// DisplayName is a POINTER so `{}` — a body that carries no field at all — is a
// rejected request rather than a silent "rename to the empty string". Without it,
// the missing field would arrive as "", the service would reject it as blank, and
// the client would be told "display name must not be empty" for a request that
// never mentioned a display name. The distinction costs one nil check and buys an
// error message that describes what actually happened.
type updateUserRequest struct {
	DisplayName *string `json:"displayName"`
}

type setStatusRequest struct {
	Status string `json:"status"`
}

type resetPasswordRequest struct {
	Password *string `json:"password"`
}

type resetPasswordResponse struct {
	User adminUserDTO `json:"user"`
	// Password appears ONLY when the server generated it, and only in this one
	// response. `omitempty` makes "the admin chose it" and "the server chose it"
	// distinguishable by the presence of the field, so the UI never has to guess
	// whether it should display something.
	Password string `json:"password,omitempty"`
}

// adminHandlers implements the six admin endpoints.
type adminHandlers struct {
	service AdminService
}

func newAdminHandlers(service AdminService) *adminHandlers {
	return &adminHandlers{service: service}
}

// listUsers handles GET /api/v1/admin/users.
func (h *adminHandlers) listUsers() gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, page, pageSize, ok := parseListQuery(c)
		if !ok {
			return
		}
		result, err := h.service.ListUsers(c.Request.Context(), filter)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		// The DTO slice is built through make() so an empty page serialises as
		// `"users": []` — a null here would make every frontend add a null check
		// before .map().
		users := make([]adminUserDTO, 0, len(result.Users))
		for i := range result.Users {
			dto, err := newAdminUserDTO(&result.Users[i])
			if err != nil {
				RespondError(c, adminError(err))
				return
			}
			users = append(users, dto)
		}
		RespondJSON(c, http.StatusOK, listUsersResponse{
			Users:    users,
			Total:    result.Total,
			Page:     page,
			PageSize: pageSize,
		})
	}
}

// createUser handles POST /api/v1/admin/users.
//
// Success is 201 with a Location header pointing at the new account: the frontend
// can follow it, and an operator reading an HTTP trace can see what was created
// without decoding the body.
func (h *adminHandlers) createUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		actorID, ok := adminActorFrom(c)
		if !ok {
			return
		}
		var req createUserRequest
		if !bindStrictJSON(c, &req) {
			return
		}
		role, ok := parseRole(c, req.Role)
		if !ok {
			return
		}
		created, err := h.service.CreateUser(c.Request.Context(), admin.CreateUserInput{
			Account:     strings.TrimSpace(req.Account),
			DisplayName: req.DisplayName,
			Role:        role,
			Password:    req.Password,
			ActorID:     actorID,
		})
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		dto, err := newAdminUserDTO(created)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		c.Header("Location", adminUserPath(created.ID))
		RespondJSON(c, http.StatusCreated, gin.H{"user": dto})
	}
}

// getUser handles GET /api/v1/admin/users/:id.
func (h *adminHandlers) getUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUserID(c)
		if !ok {
			return
		}
		u, err := h.service.GetUser(c.Request.Context(), id)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		dto, err := newAdminUserDTO(u)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"user": dto})
	}
}

// updateUser handles PATCH /api/v1/admin/users/:id.
//
// The body accepts exactly one field. Unknown or forbidden fields — `role`,
// `status`, `password`, a typo like `displayname` — are rejected with 400 by the
// strict binder below instead of being dropped on the floor. WHY that matters
// more than it looks: a silently-ignored `role` would make the API answer 200 to
// a request that did not do what it said, and the caller (an admin console, a
// migration script) would carry on believing the role had changed. A rejection is
// the only answer that keeps the client's model of the server true.
func (h *adminHandlers) updateUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		actorID, ok := adminActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseUserID(c)
		if !ok {
			return
		}
		var req updateUserRequest
		if !bindStrictJSON(c, &req) {
			return
		}
		if req.DisplayName == nil {
			RespondError(c, invalidAdminRequest("displayName is required"))
			return
		}
		updated, err := h.service.UpdateDisplayName(c.Request.Context(), admin.UserUpdateInput{
			TargetID:    id,
			DisplayName: *req.DisplayName,
			ActorID:     actorID,
		})
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		dto, err := newAdminUserDTO(updated)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"user": dto})
	}
}

// setStatus handles PATCH /api/v1/admin/users/:id/status.
func (h *adminHandlers) setStatus() gin.HandlerFunc {
	return func(c *gin.Context) {
		actorID, ok := adminActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseUserID(c)
		if !ok {
			return
		}
		var req setStatusRequest
		if !bindStrictJSON(c, &req) {
			return
		}
		status := user.Status(strings.ToUpper(strings.TrimSpace(req.Status)))
		if !status.Valid() {
			RespondError(c, invalidAdminRequest("status must be ACTIVE or DISABLED"))
			return
		}
		updated, err := h.service.SetStatus(c.Request.Context(), admin.SetStatusInput{
			TargetID: id,
			Status:   status,
			ActorID:  actorID,
		})
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		dto, err := newAdminUserDTO(updated)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		RespondJSON(c, http.StatusOK, gin.H{"user": dto})
	}
}

// resetTeacherPassword handles POST /api/v1/admin/teachers/:id/reset-password.
func (h *adminHandlers) resetTeacherPassword() gin.HandlerFunc {
	return func(c *gin.Context) {
		actorID, ok := adminActorFrom(c)
		if !ok {
			return
		}
		id, ok := parseUserID(c)
		if !ok {
			return
		}
		// A reset with no body at all is legitimate (the server generates the
		// password), so an empty body is treated as `{}` rather than as a
		// malformed request. Anything present must still be strict JSON.
		var req resetPasswordRequest
		if !bindStrictJSONAllowEmpty(c, &req) {
			return
		}
		result, err := h.service.ResetTeacherPassword(c.Request.Context(), admin.ResetPasswordInput{
			TeacherID: id,
			Password:  req.Password,
			ActorID:   actorID,
		})
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		dto, err := newAdminUserDTO(result.User)
		if err != nil {
			RespondError(c, adminError(err))
			return
		}
		RespondJSON(c, http.StatusOK, resetPasswordResponse{
			User:     dto,
			Password: result.GeneratedPassword,
		})
	}
}

// adminActorFrom returns the authenticated administrator performing this request.
//
// The id comes from the Principal that RequireSession loaded from the database,
// NEVER from the request body or a header: the audit trail and created_by must
// name the account that actually authenticated, otherwise every "who created this
// student?" question can be answered with a lie by anyone with a session.
//
// A missing Principal is unreachable while the middleware order is correct (the
// route group applies RequireSession and RequireRole before any handler). It is
// still handled explicitly, and answered as 401 rather than by inventing an actor:
// a write with no attributable actor must not happen at all.
func adminActorFrom(c *gin.Context) (uuid.UUID, bool) {
	principal, ok := PrincipalFrom(c)
	if !ok || principal == nil {
		RespondError(c, apperr.New(apperr.CodeAuthRequired))
		return uuid.Nil, false
	}
	return principal.UserID, true
}

// adminError maps a service error onto the API error contract.
//
// The mapping lives here, in one function, so a rule cannot produce two
// different answers depending on which endpoint reached it — and so the service
// stays free of HTTP semantics entirely.
//
// WHY the invalid-request messages are forwarded verbatim: they are written for
// the admin ("role must be TEACHER or STUDENT", "you cannot disable your own
// account") and contain no internal detail. They are already *apperr.Error values
// from the service where a specific sentence exists, and the fallback below only
// fires for the sentinels that carry no message of their own.
//
// Anything unrecognised becomes INTERNAL, which is what keeps a driver error from
// reaching a browser: the cause is attached for the log (RespondError logs it) and
// never for the response body (§58).
func adminError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, admin.ErrUserNotFound):
		return apperr.Wrap(apperr.CodeUserNotFound, err)
	case errors.Is(err, admin.ErrAccountTaken):
		return apperr.Wrap(apperr.CodeAccountAlreadyExists, err)
	case errors.Is(err, admin.ErrLastAdmin):
		// Its own code (409, see apperr.HTTPStatus): the admin UI must say
		// "at least one administrator must stay active", which is a different
		// message from every other refusal.
		return apperr.New(apperr.CodeLastAdminProtected)
	case errors.Is(err, admin.ErrCannotDisableSelf):
		return apperr.New(apperr.CodeCannotDisableSelf)
	default:
		// An error that already IS an *apperr.Error travels unchanged. The service
		// builds those itself — an INVALID_REQUEST with the rule that was violated,
		// or a PASSWORD_POLICY_VIOLATION with the message the frontend pins to the
		// password field — and re-deriving the envelope here would replace a
		// specific, user-facing sentence with a generic one (and, for a wrapped
		// sentinel, repeat the code inside the message). Checking this BEFORE the
		// sentinel cases is what keeps the service's wording authoritative.
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			return appErr
		}
		// Everything else collapses to INTERNAL with the original error kept as
		// the log-only cause (§58).
		return apperr.Wrap(apperr.CodeInternal, err)
	}
}

// adminUserPath is the canonical URL of one account, used for Location.
func adminUserPath(id uuid.UUID) string { return "/api/v1/admin/users/" + id.String() }

// parseListQuery reads and validates the list filters.
//
// Everything is validated before the service is called, and every rejection is a
// 400 rather than a silent fallback. WHY no lenient parsing here: `?page=abc`
// silently becoming page 1 is how a frontend ships a broken "next page" button
// that nobody notices until an admin with more than one page of accounts uses it.
func parseListQuery(c *gin.Context) (user.ListFilter, int, int, bool) {
	filter := user.ListFilter{}
	query := c.Request.URL.Query()

	if !onlyQueryKeys(c, "role", "status", "q", "page", "pageSize") {
		return filter, 0, 0, false
	}

	if raw, present := query["role"]; present {
		role, ok := parseEnumParam(c, "role", raw, roleNames())
		if !ok {
			return filter, 0, 0, false
		}
		parsed := user.Role(role)
		filter.Role = &parsed
	}
	if raw, present := query["status"]; present {
		status, ok := parseEnumParam(c, "status", raw, statusNames())
		if !ok {
			return filter, 0, 0, false
		}
		parsed := user.Status(status)
		filter.Status = &parsed
	}
	if raw, present := query["q"]; present {
		term := strings.TrimSpace(firstValue(raw))
		if len(term) > maxSearchQueryLength {
			RespondError(c, invalidAdminRequest("q must be at most 128 characters"))
			return filter, 0, 0, false
		}
		filter.Query = term
	}

	page, ok := parseBoundedIntParam(c, "page", query["page"], 1, 1, maxPageNumber)
	if !ok {
		return filter, 0, 0, false
	}
	pageSize, ok := parseBoundedIntParam(c, "pageSize", query["pageSize"], admin.DefaultListPageSize, 1, admin.MaxListPageSize)
	if !ok {
		return filter, 0, 0, false
	}

	filter.Limit = pageSize
	filter.Offset = (page - 1) * pageSize
	return filter, page, pageSize, true
}

// maxPageNumber bounds `page`.
//
// WHY a page number has an upper bound at all: the offset is sent to PostgreSQL
// as a parameter, and a page number large enough to overflow a 32-bit offset
// would turn "you asked for a page past the end" into a driver error — a 500 for
// what is really a silly request. One million pages of 200 accounts is far beyond
// any school, and past it the honest answer is 400.
const maxPageNumber = 1_000_000

// onlyQueryKeys rejects unknown query parameters.
//
// Same reasoning as the strict JSON body: `?PageSize=10` (a typo or a client
// written against a different contract) would otherwise be accepted and silently
// ignored — the client would page with 50 rows while believing it fetched 10.
// The set of accepted parameters is small, frozen and documented, so refusing the
// rest costs nothing and keeps the contract honest.
func onlyQueryKeys(c *gin.Context, allowed ...string) bool {
	for key := range c.Request.URL.Query() {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			RespondError(c, invalidAdminRequest("unknown query parameter: "+key))
			return false
		}
	}
	return true
}

// roleNames and statusNames render the domain enums as the exact tokens the API
// accepts. They are derived from the domain constants rather than typed out, so a
// role added to internal/user cannot be silently missing from the error message
// that tells a client what is allowed.
func roleNames() []string {
	names := make([]string, 0, len(user.Roles))
	for _, role := range user.Roles {
		names = append(names, string(role))
	}
	return names
}

func statusNames() []string {
	return []string{string(user.StatusActive), string(user.StatusDisabled)}
}

// parseEnumParam validates a single-valued enum query parameter.
//
// The value is taken from the LAST occurrence so that Go's standard
// `values.Get(key)` — which is what every client-side query builder mimics —
// means the same thing here.
func parseEnumParam(c *gin.Context, name string, raw []string, valid []string) (string, bool) {
	value := strings.ToUpper(strings.TrimSpace(lastValue(raw)))
	for _, candidate := range valid {
		if value == candidate {
			return value, true
		}
	}
	RespondError(c, invalidAdminRequest(name+" must be one of "+strings.Join(valid, ", ")))
	return "", false
}

// parseBoundedIntParam parses an optional integer query parameter.
func parseBoundedIntParam(c *gin.Context, name string, raw []string, fallback, minValue, maxValue int) (int, bool) {
	if len(raw) == 0 {
		return fallback, true
	}
	value := strings.TrimSpace(lastValue(raw))
	// An empty value is rejected rather than defaulted: `?page=` comes from a
	// frontend bug (an uninitialised field bound to a query param), and answering
	// it with page 1 hides that bug behind working-looking behaviour.
	parsed, err := strconv.Atoi(value)
	if err != nil {
		RespondError(c, invalidAdminRequest(name+" must be an integer"))
		return 0, false
	}
	if parsed < minValue || parsed > maxValue {
		if name == "pageSize" {
			RespondError(c, invalidAdminRequest("pageSize must be between 1 and 200"))
			return 0, false
		}
		RespondError(c, invalidAdminRequest(name+" must be between 1 and 1000000"))
		return 0, false
	}
	return parsed, true
}

// lastValue returns the last value of a repeated query parameter.
func lastValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// firstValue returns the first value of a repeated query parameter.
func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// parseRole validates the role field of a create request.
func parseRole(c *gin.Context, raw string) (user.Role, bool) {
	role := user.Role(strings.ToUpper(strings.TrimSpace(raw)))
	if !role.Valid() {
		RespondError(c, invalidAdminRequest("role must be ADMIN, TEACHER or STUDENT"))
		return "", false
	}
	return role, true
}

// parseUserID reads and validates the :id path parameter.
//
// A malformed id is 400 INVALID_REQUEST and not 404: the caller sent something
// that is not an identifier at all, and saying "not found" would suggest a valid
// id that happens to be missing. The check happens here so the service and the
// database never see a value that cannot match a row.
func parseUserID(c *gin.Context) (uuid.UUID, bool) {
	raw := strings.TrimSpace(c.Param("id"))
	id, err := uuid.Parse(raw)
	if err != nil {
		RespondError(c, invalidAdminRequest("id must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// bindStrictJSON decodes a request body that must be present and must contain
// exactly the fields the endpoint declares.
func bindStrictJSON(c *gin.Context, target any) bool {
	return bindJSONLimit(c, target, maxAdminBodyBytes, false)
}

// bindStrictJSONAllowEmpty is bindStrictJSON for endpoints where an absent body
// is a valid request (the teacher password reset).
func bindStrictJSONAllowEmpty(c *gin.Context, target any) bool {
	return bindJSONLimit(c, target, maxAdminBodyBytes, true)
}

// bindJSONLimit is the strict request-body decoder.
//
// WHY not gin's ShouldBindJSON: it uses encoding/json's default object handling,
// which IGNORES unknown fields. For an admin API that is a correctness problem
// rather than a style one — `PATCH /users/:id {"role":"ADMIN"}` or
// `{"displayname":"..."}` would return 200 having changed nothing, and the caller
// would believe otherwise. DisallowUnknownFields turns every such request into a
// 400 with a message naming the offending field.
//
// The decoder error is logged server-side but never echoed: it can quote a
// fragment of the body, and password reset bodies contain a password (§59). The
// message the client gets names the field name from the JSON token, which is not
// secret material.
//
// The body limit is a parameter because the largest legitimate body differs per
// endpoint (an admin password versus a 100-account import); the bound that matters
// is that one exists at all and that it is chosen per contract, not guessed.
func bindJSONLimit(c *gin.Context, target any, limit int64, allowEmpty bool) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)

	// Peek first so an empty body can be distinguished from a malformed one: an
	// empty reset-password body means "generate a password for me".
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		LoggerFrom(c).Info("request body could not be read", "error", err.Error())
		RespondError(c, invalidAdminRequest("the request body could not be read"))
		return false
	}
	if len(raw) == 0 {
		if allowEmpty {
			return true
		}
		RespondError(c, invalidAdminRequest("a JSON request body is required"))
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		LoggerFrom(c).Info("request body rejected", "error", err.Error())
		RespondError(c, invalidAdminRequest(describeJSONError(err)))
		return false
	}
	// A second value in the same body (`{}{}`) is a malformed request; the
	// decoder would silently ignore everything after the first object.
	if err := decoder.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		RespondError(c, invalidAdminRequest("the request body must contain a single JSON object"))
		return false
	}
	return true
}

// describeJSONError turns a decoder error into a message that names the field
// without quoting the body.
//
// The two errors an admin client can realistically cause are handled explicitly,
// because they are the ones the frontend has to act on: an unknown field (the
// client and the server disagree about the contract) and a type mismatch (a
// number where a string was expected). Everything else collapses to one safe
// sentence, since a raw decoder message can quote body content.
func describeJSONError(err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "json: unknown field "):
		field := strings.TrimPrefix(message, "json: unknown field ")
		return "unknown field " + field + " in the request body"
	case strings.Contains(message, "cannot unmarshal"):
		return "a field in the request body has the wrong type"
	default:
		return "the request body is not valid JSON for this endpoint"
	}
}

// invalidAdminRequest builds a 400 with a specific, safe message.
func invalidAdminRequest(message string) *apperr.Error {
	return apperr.New(apperr.CodeInvalidRequest).WithMessage(message)
}
