package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/admin"
	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The tests in this file run without PostgreSQL. What they pin is the TRANSPORT
// contract — which middleware chain a route is behind, which status and code a
// rejected request gets, and exactly what the JSON looks like — because those are
// the parts a frontend is built against and the parts a unit test can assert
// precisely. The rules themselves live in internal/admin and are tested there.
// The end-to-end version, with a real database and real sessions, is
// admin_integration_test.go.

// ---------------------------------------------------------------------------
// Fake admin service
// ---------------------------------------------------------------------------

// fakeAdmin records what the handler asked it to do and returns canned results.
type fakeAdmin struct {
	createErr error
	listErr   error
	getErr    error
	updateErr error
	statusErr error
	resetErr  error

	createResult *user.User
	listResult   *user.ListResult
	getResult    *user.User
	updateResult *user.User
	statusResult *user.User
	resetResult  *admin.PasswordResetResult

	listFilter user.ListFilter
	createIn   admin.CreateUserInput
	updateIn   admin.UserUpdateInput
	statusIn   admin.SetStatusInput
	resetIn    admin.ResetPasswordInput

	calls []string
}

func (f *fakeAdmin) ListUsers(_ context.Context, filter user.ListFilter) (*user.ListResult, error) {
	f.calls = append(f.calls, "list")
	f.listFilter = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResult == nil {
		return &user.ListResult{Users: []user.User{}, Total: 0}, nil
	}
	return f.listResult, nil
}

func (f *fakeAdmin) CreateUser(_ context.Context, in admin.CreateUserInput) (*user.User, error) {
	f.calls = append(f.calls, "create")
	f.createIn = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResult, nil
}

func (f *fakeAdmin) GetUser(_ context.Context, id uuid.UUID) (*user.User, error) {
	f.calls = append(f.calls, "get")
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeAdmin) UpdateDisplayName(_ context.Context, in admin.UserUpdateInput) (*user.User, error) {
	f.calls = append(f.calls, "update")
	f.updateIn = in
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateResult, nil
}

func (f *fakeAdmin) SetStatus(_ context.Context, in admin.SetStatusInput) (*user.User, error) {
	f.calls = append(f.calls, "status")
	f.statusIn = in
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.statusResult, nil
}

func (f *fakeAdmin) ResetTeacherPassword(_ context.Context, in admin.ResetPasswordInput) (*admin.PasswordResetResult, error) {
	f.calls = append(f.calls, "reset")
	f.resetIn = in
	if f.resetErr != nil {
		return nil, f.resetErr
	}
	return f.resetResult, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type adminHarness struct {
	*routerHarness
	admin *fakeAdmin
}

func newAdminHarness(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter) *adminHarness {
	t.Helper()
	if cfg == nil {
		cfg = authTestConfig(t)
	}
	fakeAuthSvc := newFakeAuth()
	fakeAdminSvc := &fakeAdmin{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// RespondError logs the internal cause through slog.Default, not through the
	// request-scoped logger. Installing the capturing logger as the process
	// default is what lets a test assert both halves of §58: the client sees a
	// generic INTERNAL, and the operator still sees the cause.
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	router := NewRouter(Deps{
		Logger:  logger,
		Config:  cfg,
		Auth:    fakeAuthSvc,
		Admin:   fakeAdminSvc,
		Limiter: limiter,
	})
	return &adminHarness{
		routerHarness: &routerHarness{router: router, auth: fakeAuthSvc, cfg: cfg, logs: logs},
		admin:         fakeAdminSvc,
	}
}

// adminSession registers an active administrator with a live session and returns
// the account and the two cookies its entry point issues.
func (h *adminHarness) adminSession(t *testing.T) (*user.User, map[string]string) {
	t.Helper()
	u := h.staff(t, user.RoleAdmin, "admin-01", "an-admin-passphrase")
	principal := h.auth.addSession("admin-token-01", u)
	cookies := map[string]string{
		"classwatch_session_admin":      "admin-token-01",
		"classwatch_session_admin_csrf": principal.CSRFToken,
	}
	return u, cookies
}

// call performs an authenticated-or-not request. The CSRF header is added by the
// caller through headers.
func (h *adminHarness) call(method, path, body string, cookies map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	return h.request(method, path, body, cookies, headers, "203.0.113.7:44444")
}

// csrf returns the header map a state-changing request needs.
func csrf(cookies map[string]string) map[string]string {
	for name, value := range cookies {
		if strings.HasSuffix(name, "_csrf") {
			return map[string]string{"X-CSRF-Token": value}
		}
	}
	return nil
}

// adminUser is a ready-made row for the fake service to return.
func adminUser(t *testing.T, role user.Role, account string) *user.User {
	t.Helper()
	var hash *string
	if role != user.RoleStudent {
		stored := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$hashvalue"
		hash = &stored
	}
	return &user.User{
		ID:           uuid.New(),
		Account:      account,
		DisplayName:  string(role) + " " + account,
		Role:         role,
		Status:       user.StatusActive,
		PasswordHash: hash,
		CreatedAt:    time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC),
		UpdatedAt:    time.Date(2025, 2, 4, 5, 6, 7, 0, time.UTC),
	}
}

// decodeUserDTO returns the `user` object of a response.
func decodeUserDTO(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if body.User == nil {
		t.Fatalf("response has no user object: %s", rec.Body.String())
	}
	return body.User
}

// assertNoPasswordMaterial fails when a response body carries a hash or a
// password field. It runs on every admin response below: a DTO that does not have
// the field is the guarantee, and this is the test that keeps it true.
func assertNoPasswordMaterial(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, forbidden := range []string{"passwordHash", "password_hash", "$argon2id$", "createdBy", "created_by"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response contains %q: %s", forbidden, body)
		}
	}
}

// ---------------------------------------------------------------------------
// The three middleware layers, on every endpoint
// ---------------------------------------------------------------------------

// TestAdminRoutesRequireAllThreeLayers is the §37/§63 test for the whole group:
// for every route, no session is 401, a non-admin session is 403, and a write
// without the CSRF token is 403.
func TestAdminRoutesRequireAllThreeLayers(t *testing.T) {
	type route struct {
		method string
		path   string
		body   string
		write  bool
	}
	id := uuid.New().String()
	routes := []route{
		{http.MethodGet, "/api/v1/admin/users", "", false},
		{http.MethodPost, "/api/v1/admin/users", `{"account":"t001","displayName":"T","role":"TEACHER","password":"a-passphrase-x"}`, true},
		{http.MethodGet, "/api/v1/admin/users/" + id, "", false},
		{http.MethodPatch, "/api/v1/admin/users/" + id, `{"displayName":"New"}`, true},
		{http.MethodPatch, "/api/v1/admin/users/" + id + "/status", `{"status":"DISABLED"}`, true},
		{http.MethodPost, "/api/v1/admin/teachers/" + id + "/reset-password", ``, true},
	}

	for _, tc := range routes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// 1. No session at all → 401, and no service call.
			anonymous := newAdminHarness(t, nil, nil)
			rec := anonymous.call(tc.method, tc.path, tc.body, nil, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("without a session: status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != "AUTH_REQUIRED" {
				t.Errorf("without a session: code = %q, want AUTH_REQUIRED", code)
			}
			if len(anonymous.admin.calls) != 0 {
				t.Errorf("an unauthenticated request reached the service: %v", anonymous.admin.calls)
			}

			// 2. A live session for the wrong role → 403 ROLE_FORBIDDEN.
			for _, other := range []struct {
				role    user.Role
				account string
				cookie  string
			}{
				{user.RoleTeacher, "teacher-99", "classwatch_session_teacher"},
				{user.RoleStudent, "S99001", "classwatch_session_student"},
			} {
				otherHarness := newAdminHarness(t, nil, nil)
				var otherUser *user.User
				if other.role == user.RoleStudent {
					// The student helper is used because a student account has no
					// password at all; the middleware test only needs the role.
					otherUser = otherHarness.student(t, other.account)
				} else {
					otherUser = otherHarness.staff(t, other.role, other.account, "")
				}
				raw := strings.ToLower(string(other.role)) + "-token"
				principal := otherHarness.auth.addSession(raw, otherUser)
				otherCookies := map[string]string{other.cookie: raw, other.cookie + "_csrf": principal.CSRFToken}
				otherRec := otherHarness.call(tc.method, tc.path, tc.body, otherCookies, csrf(otherCookies))
				if otherRec.Code != http.StatusForbidden {
					t.Fatalf("%s session: status = %d, want 403 (%s)", other.role, otherRec.Code, otherRec.Body.String())
				}
				if code := decodeErrorCode(t, otherRec); code != "ROLE_FORBIDDEN" {
					t.Errorf("%s session: code = %q, want ROLE_FORBIDDEN", other.role, code)
				}
				if len(otherHarness.admin.calls) != 0 {
					t.Errorf("%s session reached the service: %v", other.role, otherHarness.admin.calls)
				}
			}

			if !tc.write {
				// A read route is exempt from the CSRF check by design, so the
				// matrix ends here for GET.
				return
			}

			// 3. Admin session, write method, no CSRF token → 403 CSRF_INVALID.
			//
			// A fresh harness, so "the request was rejected before the service ran"
			// is asserted against a call log that the earlier cases cannot pollute.
			adminSide := newAdminHarness(t, nil, nil)
			_, adminCookies := adminSide.adminSession(t)
			noCSRF := adminSide.call(tc.method, tc.path, tc.body, adminCookies, nil)
			if noCSRF.Code != http.StatusForbidden {
				t.Fatalf("without CSRF: status = %d, want 403 (%s)", noCSRF.Code, noCSRF.Body.String())
			}
			if code := decodeErrorCode(t, noCSRF); code != "CSRF_INVALID" {
				t.Errorf("without CSRF: code = %q, want CSRF_INVALID", code)
			}
			if len(adminSide.admin.calls) != 0 {
				t.Errorf("a CSRF-less request reached the service: %v", adminSide.admin.calls)
			}
		})
	}
}

// TestAdminReadRoutesDoNotRequireCSRF states the other half of the rule: a GET is
// not a state change, so requiring a token there would break plain navigation and
// buy nothing.
func TestAdminReadRoutesDoNotRequireCSRF(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	h.admin.getResult = adminUser(t, user.RoleStudent, "S1")
	_, cookies := h.adminSession(t)

	rec := h.call(http.MethodGet, "/api/v1/admin/users/"+uuid.New().String(), "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAdminRoutesAreAbsentWithoutAService pins the degraded-mode behaviour: no
// admin service means no admin surface at all (404), rather than a route that
// exists with nothing behind it.
func TestAdminRoutesAreAbsentWithoutAService(t *testing.T) {
	cfg := authTestConfig(t)
	fakeAuthSvc := newFakeAuth()
	router := NewRouter(Deps{Logger: discardLogger(), Config: cfg, Auth: fakeAuthSvc})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no admin service is wired", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Path and query validation
// ---------------------------------------------------------------------------

func TestAdminInvalidUUIDIs400NotA500(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/admin/users/not-a-uuid", ""},
		{http.MethodPatch, "/api/v1/admin/users/not-a-uuid", `{"displayName":"x"}`},
		{http.MethodPatch, "/api/v1/admin/users/not-a-uuid/status", `{"status":"ACTIVE"}`},
		{http.MethodPost, "/api/v1/admin/teachers/not-a-uuid/reset-password", ""},
		{http.MethodGet, "/api/v1/admin/users/42", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := h.call(tc.method, tc.path, tc.body, cookies, csrf(cookies))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}
			if len(h.admin.calls) != 0 {
				t.Errorf("a malformed id reached the service: %v", h.admin.calls)
			}
		})
	}
}

func TestAdminListQueryValidation(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"no parameters", "", http.StatusOK},
		{"valid filters", "?role=TEACHER&status=ACTIVE&q=li&page=2&pageSize=10", http.StatusOK},
		{"lower-case filter values", "?role=teacher&status=active", http.StatusOK},
		{"unknown role", "?role=PRINCIPAL", http.StatusBadRequest},
		{"unknown status", "?status=PAUSED", http.StatusBadRequest},
		{"empty role", "?role=", http.StatusBadRequest},
		{"page zero", "?page=0", http.StatusBadRequest},
		{"negative page", "?page=-1", http.StatusBadRequest},
		{"non-numeric page", "?page=abc", http.StatusBadRequest},
		{"empty page", "?page=", http.StatusBadRequest},
		{"pageSize zero", "?pageSize=0", http.StatusBadRequest},
		{"pageSize above the cap", "?pageSize=201", http.StatusBadRequest},
		{"pageSize at the cap", "?pageSize=200", http.StatusOK},
		{"unknown parameter", "?pagesize=10", http.StatusBadRequest},
		{"over-long search", "?q=" + strings.Repeat("a", maxSearchQueryLength+1), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdminHarness(t, nil, nil)
			_, cookies := h.adminSession(t)
			rec := h.call(http.MethodGet, "/api/v1/admin/users"+tc.query, "", cookies, nil)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusBadRequest {
				if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
					t.Errorf("code = %q, want INVALID_REQUEST", code)
				}
				if len(h.admin.calls) != 0 {
					t.Errorf("an invalid query reached the service: %v", h.admin.calls)
				}
			}
		})
	}
}

func TestAdminListDefaultsAndPaging(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	rows := []user.User{*adminUser(t, user.RoleTeacher, "teacher_1"), *adminUser(t, user.RoleStudent, "S1")}
	h.admin.listResult = &user.ListResult{Users: rows, Total: 123}

	// Defaults: page 1, 50 rows.
	rec := h.call(http.MethodGet, "/api/v1/admin/users", "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Users    []map[string]any `json:"users"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"pageSize"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Page != 1 || body.PageSize != admin.DefaultListPageSize || body.Total != 123 {
		t.Errorf("page/pageSize/total = %d/%d/%d, want 1/%d/123", body.Page, body.PageSize, body.Total, admin.DefaultListPageSize)
	}
	if len(body.Users) != 2 {
		t.Fatalf("users = %d, want 2", len(body.Users))
	}
	if h.admin.listFilter.Limit != admin.DefaultListPageSize || h.admin.listFilter.Offset != 0 {
		t.Errorf("filter = %+v, want limit 50 offset 0", h.admin.listFilter)
	}
	// The DTO is the frozen contract, field for field.
	for _, field := range []string{"id", "account", "displayName", "role", "status", "createdAt", "updatedAt", "lastLoginAt"} {
		if _, ok := body.Users[0][field]; !ok {
			t.Errorf("user DTO is missing %q: %v", field, body.Users[0])
		}
	}
	if body.Users[0]["lastLoginAt"] != nil {
		t.Errorf("lastLoginAt = %v, want null for an account that never logged in", body.Users[0]["lastLoginAt"])
	}
	assertNoPasswordMaterial(t, rec)

	// Page 3 of 10 rows: offset 20.
	rec = h.call(http.MethodGet, "/api/v1/admin/users?page=3&pageSize=10", "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if h.admin.listFilter.Offset != 20 || h.admin.listFilter.Limit != 10 {
		t.Errorf("filter = %+v, want limit 10 offset 20", h.admin.listFilter)
	}

	// Filters travel through unchanged.
	rec = h.call(http.MethodGet, "/api/v1/admin/users?role=TEACHER&status=DISABLED&q=%20li%20", "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := h.admin.listFilter
	if got.Role == nil || *got.Role != user.RoleTeacher {
		t.Errorf("role filter = %v, want TEACHER", got.Role)
	}
	if got.Status == nil || *got.Status != user.StatusDisabled {
		t.Errorf("status filter = %v, want DISABLED", got.Status)
	}
	if got.Query != "li" {
		t.Errorf("q = %q, want the trimmed value", got.Query)
	}
}

func TestAdminListEmptyPageIsAnEmptyArray(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.listResult = &user.ListResult{Users: []user.User{}, Total: 0}

	rec := h.call(http.MethodGet, "/api/v1/admin/users?role=ADMIN", "", cookies, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// `"users": null` would force a null check on every frontend .map().
	if !strings.Contains(rec.Body.String(), `"users":[]`) {
		t.Errorf("empty page = %s, want an empty array", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Strict body binding
// ---------------------------------------------------------------------------

// TestAdminRejectsUnknownAndForbiddenFields is the test for the decision recorded
// in UpdateDisplayName: an unsupported field is refused, never ignored, so a
// caller can never believe it changed something it did not.
// TestAdminRejectsUnknownAndForbiddenFields pins the decision recorded in
// UpdateDisplayName: an unsupported field is refused, never ignored.
//
// One caveat is asserted here rather than left implicit: encoding/json matches
// field names case-insensitively, so `{"displayname":...}` and
// `{"DisplayName":...}` are accepted and mean displayName. That leniency is the
// standard library's and is harmless — the field still maps to the ONE thing this
// endpoint may change. What must never be accepted is a field that maps to
// nothing (`display_name`) or to something forbidden (`role`, `status`,
// `password`), and those are the cases below.
func TestAdminRejectsUnknownAndForbiddenFields(t *testing.T) {
	id := uuid.New().String()
	cases := []struct {
		name string
		path string
		body string
	}{
		{"role on PATCH", "/api/v1/admin/users/" + id, `{"displayName":"New","role":"ADMIN"}`},
		{"status on PATCH", "/api/v1/admin/users/" + id, `{"displayName":"New","status":"DISABLED"}`},
		{"password on PATCH", "/api/v1/admin/users/" + id, `{"displayName":"New","password":"hunter2hunter2"}`},
		{"underscored field name", "/api/v1/admin/users/" + id, `{"display_name":"New"}`},
		{"empty PATCH body", "/api/v1/admin/users/" + id, `{}`},
		{"extra field on create", "/api/v1/admin/users", `{"account":"t1","displayName":"T","role":"TEACHER","password":"a-passphrase-x","status":"DISABLED"}`},
		{"role on status", "/api/v1/admin/users/" + id + "/status", `{"status":"ACTIVE","role":"ADMIN"}`},
		{"unknown field on reset", "/api/v1/admin/teachers/" + id + "/reset-password", `{"password":null,"account":"x"}`},
		{"wrong type on create", "/api/v1/admin/users", `{"account":42,"displayName":"T","role":"TEACHER","password":"a-passphrase-x"}`},
		{"two objects", "/api/v1/admin/users", `{"account":"t1","displayName":"T","role":"STUDENT"}{}`},
		{"not JSON at all", "/api/v1/admin/users", `account=t1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdminHarness(t, nil, nil)
			_, cookies := h.adminSession(t)
			// A PATCH that somehow passed validation would reach a fake with a nil
			// result; giving it a row keeps the failure a 400-shaped assertion
			// instead of a 500 from the nil guard.
			h.admin.updateResult = adminUser(t, user.RoleStudent, "S1")
			method := http.MethodPatch
			if strings.Contains(tc.path, "/admin/users") && !strings.Contains(tc.path, id) {
				method = http.MethodPost
			}
			if strings.HasSuffix(tc.path, "reset-password") {
				method = http.MethodPost
			}
			rec := h.call(method, tc.path, tc.body, cookies, csrf(cookies))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}
			if len(h.admin.calls) != 0 {
				t.Errorf("a rejected body reached the service: %v", h.admin.calls)
			}
		})
	}
}

// TestAdminErrorMessagesNameTheFieldWithoutEchoingTheBody keeps the 400s
// actionable while making sure password material never travels back out.
func TestAdminErrorMessagesNameTheFieldWithoutEchoingTheBody(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	const secret = "super-secret-passphrase"

	rec := h.call(http.MethodPatch, "/api/v1/admin/users/"+uuid.New().String(),
		`{"displayName":"New","password":"`+secret+`"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	message := errorMessage(t, rec)
	if !strings.Contains(message, "password") {
		t.Errorf("message = %q, want it to name the offending field", message)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the error response echoes the request body: %s", rec.Body.String())
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Errorf("the password reached the log: %s", h.logs.String())
	}
}

func TestAdminRoleValidationOnCreate(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    int
		wantSub string
	}{
		{"admin is refused by the service", `{"account":"admin_2","displayName":"A","role":"ADMIN","password":"a-passphrase-x"}`, 0, ""},
		{"unknown role", `{"account":"t1","displayName":"T","role":"PRINCIPAL"}`, http.StatusBadRequest, "role"},
		{"missing role", `{"account":"t1","displayName":"T"}`, http.StatusBadRequest, "role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdminHarness(t, nil, nil)
			_, cookies := h.adminSession(t)
			if tc.want == 0 {
				// ADMIN passes the transport layer (the field is well formed) and is
				// refused by the service, which is where the rule lives. The reply is
				// the service's 400.
				h.admin.createErr = apperr.New(apperr.CodeInvalidRequest).
					WithMessage("role must be TEACHER or STUDENT; administrator accounts can only be created by the adminctl initialization command")
				rec := h.call(http.MethodPost, "/api/v1/admin/users", tc.body, cookies, csrf(cookies))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
				}
				if !strings.Contains(errorMessage(t, rec), "adminctl") {
					t.Errorf("message = %q, want the adminctl explanation", errorMessage(t, rec))
				}
				if h.admin.createIn.Role != user.RoleAdmin {
					t.Errorf("service received role %q, want ADMIN passed through for the rule to reject", h.admin.createIn.Role)
				}
				return
			}
			rec := h.call(http.MethodPost, "/api/v1/admin/users", tc.body, cookies, csrf(cookies))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(errorMessage(t, rec), tc.wantSub) {
				t.Errorf("message = %q, want it to mention %q", errorMessage(t, rec), tc.wantSub)
			}
			if len(h.admin.calls) != 0 {
				t.Errorf("a malformed role reached the service: %v", h.admin.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Transport behaviour of each endpoint
// ---------------------------------------------------------------------------

func TestAdminCreateReturns201WithLocationAndNoPasswordHash(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	actor, cookies := h.adminSession(t)
	created := adminUser(t, user.RoleTeacher, "teacher_01")
	h.admin.createResult = created

	rec := h.call(http.MethodPost, "/api/v1/admin/users",
		`{"account":"teacher_01","displayName":"李老师","role":"TEACHER","password":"a-teacher-passphrase"}`,
		cookies, csrf(cookies))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/admin/users/"+created.ID.String() {
		t.Errorf("Location = %q, want the new account's URL", got)
	}
	if !strings.Contains(rec.Body.String(), `"user"`) {
		t.Errorf("body = %s, want a user object", rec.Body.String())
	}
	assertNoPasswordMaterial(t, rec)
	if dto := decodeUserDTO(t, rec); dto["updatedAt"] == nil {
		t.Error("the DTO has no updatedAt")
	}
	// The actor comes from the session, never from the body.
	if h.admin.createIn.ActorID != actor.ID {
		t.Errorf("actor = %s, want the authenticated administrator %s", h.admin.createIn.ActorID, actor.ID)
	}
	if h.admin.createIn.Password == nil || *h.admin.createIn.Password != "a-teacher-passphrase" {
		t.Errorf("password passed through as %v", h.admin.createIn.Password)
	}
}

func TestAdminCreateStudentWithoutPasswordField(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.createResult = adminUser(t, user.RoleStudent, "S10001")

	rec := h.call(http.MethodPost, "/api/v1/admin/users",
		`{"account":"S10001","displayName":"张三","role":"STUDENT"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if h.admin.createIn.Password != nil {
		t.Errorf("password = %v, want nil for a student (§2.2)", h.admin.createIn.Password)
	}
}

func TestAdminCreateDuplicateAccountIsAConflict(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.createErr = admin.ErrAccountTaken

	rec := h.call(http.MethodPost, "/api/v1/admin/users",
		`{"account":"S10001","displayName":"张三","role":"STUDENT"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "ACCOUNT_ALREADY_EXISTS" {
		t.Errorf("code = %q, want ACCOUNT_ALREADY_EXISTS", code)
	}
}

func TestAdminPasswordPolicyViolationIsItsOwnCode(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.createErr = apperr.New(apperr.CodePasswordPolicyViolation).
		WithMessage("auth: password is too short: at least 12 characters")

	rec := h.call(http.MethodPost, "/api/v1/admin/users",
		`{"account":"teacher_01","displayName":"李老师","role":"TEACHER","password":"short"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	// The dedicated code is what lets the frontend attach the message to the
	// password input instead of showing a generic banner.
	if code := decodeErrorCode(t, rec); code != "PASSWORD_POLICY_VIOLATION" {
		t.Errorf("code = %q, want PASSWORD_POLICY_VIOLATION", code)
	}
}

func TestAdminGetUnknownUserIs404(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.getErr = admin.ErrUserNotFound

	rec := h.call(http.MethodGet, "/api/v1/admin/users/"+uuid.New().String(), "", cookies, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "USER_NOT_FOUND" {
		t.Errorf("code = %q, want USER_NOT_FOUND", code)
	}
}

func TestAdminPatchReturnsTheUpdatedDTO(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	actor, cookies := h.adminSession(t)
	updated := adminUser(t, user.RoleStudent, "S10002")
	updated.DisplayName = "李四"
	h.admin.updateResult = updated

	rec := h.call(http.MethodPatch, "/api/v1/admin/users/"+updated.ID.String(),
		`{"displayName":"李四"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	dto := decodeUserDTO(t, rec)
	if dto["displayName"] != "李四" {
		t.Errorf("displayName = %v", dto["displayName"])
	}
	for _, field := range []string{"id", "account", "displayName", "role", "status", "createdAt", "updatedAt", "lastLoginAt"} {
		if _, ok := dto[field]; !ok {
			t.Errorf("user DTO is missing %q", field)
		}
	}
	assertNoPasswordMaterial(t, rec)
	if h.admin.updateIn.TargetID != updated.ID || h.admin.updateIn.ActorID != actor.ID {
		t.Errorf("service input = %+v, want the path id and the session actor", h.admin.updateIn)
	}
}

func TestAdminSetStatusEndpoint(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	actor, cookies := h.adminSession(t)
	target := adminUser(t, user.RoleTeacher, "teacher_02")
	h.admin.statusResult = target

	rec := h.call(http.MethodPatch, "/api/v1/admin/users/"+target.ID.String()+"/status",
		`{"status":"DISABLED"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if h.admin.statusIn.Status != user.StatusDisabled {
		t.Errorf("status passed = %q, want DISABLED", h.admin.statusIn.Status)
	}
	if h.admin.statusIn.ActorID != actor.ID {
		t.Errorf("actor = %s, want %s", h.admin.statusIn.ActorID, actor.ID)
	}
	assertNoPasswordMaterial(t, rec)

	// An invalid status is caught at the transport layer.
	rec = h.call(http.MethodPatch, "/api/v1/admin/users/"+target.ID.String()+"/status",
		`{"status":"PAUSED"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	// The "last administrator" refusal has its own code and a 409: the body was
	// well formed, the action just conflicts with the system's current state.
	// The UI needs to tell the administrator *which* rule stopped it, so it
	// branches on the code instead of parsing prose (§58).
	h.admin.statusErr = admin.ErrLastAdmin
	rec = h.call(http.MethodPatch, "/api/v1/admin/users/"+target.ID.String()+"/status",
		`{"status":"DISABLED"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "LAST_ADMIN_PROTECTED" {
		t.Errorf("code = %q, want LAST_ADMIN_PROTECTED", code)
	}
	message := errorMessage(t, rec)
	if message == "" {
		t.Error("message is empty, want a fallback sentence for the code")
	}
	// The sentence is rendered in an admin console: it must not carry an internal
	// package name, and it must not repeat the error code inside the message.
	for _, internal := range []string{"admin:", "httpapi:", "LAST_ADMIN_PROTECTED"} {
		if strings.Contains(message, internal) {
			t.Errorf("the message leaks an internal token %q: %q", internal, message)
		}
	}

	// Disabling yourself is refused with its own code as well: the console shows
	// a different sentence, and the frontend must not have to guess which rule
	// fired from a generic INVALID_REQUEST.
	h.admin.statusErr = admin.ErrCannotDisableSelf
	rec = h.call(http.MethodPatch, "/api/v1/admin/users/"+target.ID.String()+"/status",
		`{"status":"DISABLED"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "CANNOT_DISABLE_SELF" {
		t.Errorf("code = %q, want CANNOT_DISABLE_SELF", code)
	}
}

func TestAdminResetPasswordResponseShape(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	teacher := adminUser(t, user.RoleTeacher, "teacher_03")

	// 1. No body at all: the server generates the password and returns it once.
	const generated = "generated-value-abcdefghij"
	h.admin.resetResult = &admin.PasswordResetResult{User: teacher, GeneratedPassword: generated}
	rec := h.call(http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password", "", cookies, csrf(cookies))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		User     map[string]any `json:"user"`
		Password string         `json:"password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Password != generated {
		t.Errorf("password = %q, want the generated value", body.Password)
	}
	if body.User == nil {
		t.Fatal("the response has no user object")
	}
	assertNoPasswordMaterial(t, rec)
	if h.admin.resetIn.Password != nil {
		t.Errorf("service received a password (%v) although the body was empty", *h.admin.resetIn.Password)
	}

	// 2. The admin supplies a password: the field is omitted, so the UI knows it
	//    has nothing to display.
	h.admin.resetResult = &admin.PasswordResetResult{User: teacher}
	rec = h.call(http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password",
		`{"password":"a-new-passphrase-1"}`, cookies, csrf(cookies))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password\"") && strings.Contains(rec.Body.String(), `"password":`) {
		// `password` may only appear as a JSON key of the generated-password field.
		var probe map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &probe); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if _, present := probe["password"]; present {
			t.Errorf("the response echoed a password field: %s", rec.Body.String())
		}
	}
	if h.admin.resetIn.Password == nil || *h.admin.resetIn.Password != "a-new-passphrase-1" {
		t.Errorf("the supplied password did not reach the service")
	}

	// 3. A non-teacher target is a 400 from the service.
	h.admin.resetErr = apperr.New(apperr.CodeInvalidRequest).
		WithMessage("only TEACHER accounts have a password reset here; administrator passwords are managed with the adminctl command")
	rec = h.call(http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password", "", cookies, csrf(cookies))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorMessage(t, rec), "adminctl") {
		t.Errorf("message = %q, want a pointer to adminctl", errorMessage(t, rec))
	}
}

// TestAdminInternalErrorIsNotLeaked is the §58 test at the transport layer: an
// unrecognised error becomes a generic INTERNAL, and the driver detail stays in
// the log.
func TestAdminInternalErrorIsNotLeaked(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	h.admin.listErr = errors.New(`ERROR: relation "users" does not exist (SQLSTATE 42P01)`)

	rec := h.call(http.MethodGet, "/api/v1/admin/users", "", cookies, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", code)
	}
	if strings.Contains(rec.Body.String(), "42P01") || strings.Contains(rec.Body.String(), "relation") {
		t.Errorf("the response leaks database detail: %s", rec.Body.String())
	}
	if !strings.Contains(h.logs.String(), "42P01") {
		t.Error("the cause was not logged, so the incident would be undebuggable")
	}
}

// ---------------------------------------------------------------------------
// Method routing
// ---------------------------------------------------------------------------

// TestAdminWrongMethodIs405 keeps the admin group's method set exact: a DELETE
// (which no phase implements) must answer 405 with the standard envelope and an
// Allow header, not 404 and not an accidental handler.
func TestAdminWrongMethodIs405(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	id := uuid.New().String()

	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodDelete, "/api/v1/admin/users/" + id, "GET"},
		{http.MethodPut, "/api/v1/admin/users/" + id, "GET"},
		{http.MethodGet, "/api/v1/admin/users/" + id + "/status", "PATCH"},
		{http.MethodPost, "/api/v1/admin/users/" + id + "/status", "PATCH"},
		{http.MethodGet, "/api/v1/admin/teachers/" + id + "/reset-password", "POST"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := h.call(tc.method, tc.path, "", cookies, csrf(cookies))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}
			if allow := rec.Header().Get("Allow"); !strings.Contains(allow, tc.allow) {
				t.Errorf("Allow = %q, want it to contain %q", allow, tc.allow)
			}
		})
	}
}

// TestAdminUnknownPathIs404 uses the same envelope as every other failure, so a
// client never has to parse an HTML error page.
func TestAdminUnknownPathIs404(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)

	rec := h.call(http.MethodGet, "/api/v1/admin/students", "", cookies, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

// TestAdminActionsAreLoggedWithIdentity is the §59 logging test for the admin
// surface: the access log must attribute the request to the administrator, and no
// credential may appear anywhere in it.
func TestAdminActionsAreLoggedWithIdentity(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	actor, cookies := h.adminSession(t)
	created := adminUser(t, user.RoleTeacher, "teacher_04")
	h.admin.createResult = created
	const password = "a-very-secret-passphrase"

	h.call(http.MethodPost, "/api/v1/admin/users",
		`{"account":"teacher_04","displayName":"李老师","role":"TEACHER","password":"`+password+`"}`,
		cookies, csrf(cookies))

	logged := h.logs.String()
	if strings.Contains(logged, password) {
		t.Fatalf("the password reached the access log: %s", logged)
	}
	if strings.Contains(logged, "$argon2id$") {
		t.Fatalf("a password hash reached the access log: %s", logged)
	}
	for _, cookie := range cookies {
		if strings.Contains(logged, cookie) {
			t.Fatalf("a session or CSRF token reached the log")
		}
	}
	if !strings.Contains(logged, actor.ID.String()) {
		t.Errorf("the access log does not name the administrator: %s", logged)
	}
	if !strings.Contains(logged, "POST") || !strings.Contains(logged, "/api/v1/admin/users") {
		t.Errorf("the access log does not record the action")
	}
}

// TestAdminRequestBodySizeIsBounded documents the amplification bound: an admin
// session must not be able to make the server read an unbounded body.
func TestAdminRequestBodySizeIsBounded(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	huge := `{"displayName":"` + strings.Repeat("a", maxAdminBodyBytes+1024) + `"}`

	rec := h.call(http.MethodPatch, "/api/v1/admin/users/"+uuid.New().String(), huge, cookies, csrf(cookies))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an over-sized body", rec.Code)
	}
}

// TestAdminServiceFailureDoesNotPanicOnNilResult guards the handler against a
// service that reports success with nothing in it: the alternative is a nil
// dereference in the request path, which is a 500 at best.
func TestAdminServiceFailureDoesNotPanicOnNilResult(t *testing.T) {
	h := newAdminHarness(t, nil, nil)
	_, cookies := h.adminSession(t)
	// createResult stays nil and no error is set: the handler must not dereference
	// it blindly. It is a programming error on the service side, so a 500 (not a
	// panic, and not a 200 with a bogus body) is the acceptable outcome.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the handler panicked on a nil result: %v", r)
			}
		}()
		rec := h.call(http.MethodPost, "/api/v1/admin/users",
			`{"account":"S1","displayName":"S","role":"STUDENT"}`, cookies, csrf(cookies))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	}()
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// TestAdminRoutesAreRegisteredOnce guards against a route being mounted twice,
// which gin panics on at boot — a failure that would only show up when the binary
// starts.
func TestAdminRoutesAreRegisteredOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newAdminHarness(t, nil, nil)
	found := map[string]int{}
	for _, route := range h.router.Routes() {
		found[route.Method+" "+route.Path]++
	}
	want := []string{
		"GET /api/v1/admin/users",
		"POST /api/v1/admin/users",
		"GET /api/v1/admin/users/:id",
		"PATCH /api/v1/admin/users/:id",
		"PATCH /api/v1/admin/users/:id/status",
		"POST /api/v1/admin/teachers/:id/reset-password",
	}
	for _, route := range want {
		if found[route] != 1 {
			t.Errorf("route %q registered %d times, want exactly 1", route, found[route])
		}
	}
	if len(found) == 0 {
		t.Fatal("no routes registered")
	}
}
