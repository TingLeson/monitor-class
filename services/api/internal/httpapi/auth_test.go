package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The tests in this file run without PostgreSQL: the auth service is an
// interface precisely so the middleware behaviour (status codes, cookies, CSRF,
// cross-entry rejection) can be asserted exactly, with no database in the loop.
// The end-to-end version with a real database lives in auth_integration_test.go.

// ---------------------------------------------------------------------------
// Fake auth service
// ---------------------------------------------------------------------------

type fakeAuth struct {
	// principals is keyed by raw session token.
	principals map[string]*auth.Principal
	// passwords is keyed by "<role>|<lowercase account>".
	passwords map[string]string
	// users is keyed by "<role>|<lowercase account>"; used to build the DTO.
	users map[string]*user.User

	studentLoginErr error
	passwordErr     error
	authenticateErr error
	logoutErr       error

	logoutCalls []string
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{
		principals: make(map[string]*auth.Principal),
		passwords:  make(map[string]string),
		users:      make(map[string]*user.User),
	}
}

func accountKey(role user.Role, account string) string {
	return string(role) + "|" + strings.ToLower(account)
}

// addUser registers an account that can log in through the entry of its role.
func (f *fakeAuth) addUser(u *user.User, password string) {
	f.users[accountKey(u.Role, u.Account)] = u
	if password != "" {
		f.passwords[accountKey(u.Role, u.Account)] = password
	}
}

// addSession registers a raw token that authenticates as the given account.
func (f *fakeAuth) addSession(raw string, u *user.User) *auth.Principal {
	principal := &auth.Principal{
		UserID:      u.ID,
		Account:     u.Account,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Status:      u.Status,
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
		SessionID:   uuid.New(),
		CSRFToken:   "csrf-" + raw,
	}
	f.principals[raw] = principal
	return principal
}

func (f *fakeAuth) loginResult(role user.Role, u *user.User, raw string) *auth.LoginResult {
	principal := f.addSession(raw, u)
	return &auth.LoginResult{
		Principal:    *principal,
		User:         u,
		SessionToken: raw,
		CSRFToken:    principal.CSRFToken,
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

func (f *fakeAuth) LoginStudent(_ context.Context, account string, _ auth.LoginMeta) (*auth.LoginResult, error) {
	if f.studentLoginErr != nil {
		return nil, f.studentLoginErr
	}
	u, ok := f.users[accountKey(user.RoleStudent, account)]
	if !ok {
		return nil, auth.ErrInvalidCredentials
	}
	if u.Status != user.StatusActive {
		return nil, auth.ErrAccountDisabled
	}
	return f.loginResult(user.RoleStudent, u, "student-token-"+strings.ToLower(account)), nil
}

func (f *fakeAuth) LoginWithPassword(_ context.Context, role user.Role, account, password string, _ auth.LoginMeta) (*auth.LoginResult, error) {
	if f.passwordErr != nil {
		return nil, f.passwordErr
	}
	u, ok := f.users[accountKey(role, account)]
	if !ok {
		return nil, auth.ErrInvalidCredentials
	}
	if f.passwords[accountKey(role, account)] != password {
		return nil, auth.ErrInvalidCredentials
	}
	if u.Status != user.StatusActive {
		return nil, auth.ErrAccountDisabled
	}
	return f.loginResult(role, u, strings.ToLower(string(role))+"-token-"+strings.ToLower(account)), nil
}

func (f *fakeAuth) Authenticate(_ context.Context, rawToken string) (*auth.Principal, error) {
	if f.authenticateErr != nil {
		return nil, f.authenticateErr
	}
	if p, ok := f.principals[rawToken]; ok {
		return p, nil
	}
	return nil, auth.ErrSessionInvalid
}

func (f *fakeAuth) Logout(_ context.Context, rawToken string) error {
	f.logoutCalls = append(f.logoutCalls, rawToken)
	return f.logoutErr
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// authTestConfig is the full configuration the router needs in Phase 1.
func authTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		AppEnv:                           config.EnvTest,
		SessionCookieName:                "classwatch_session",
		SessionTTL:                       time.Hour,
		SessionCookieSecure:              false,
		RateLimitLoginPerMinute:          100,
		RateLimitLoginPerAccountPer10Min: 100,
		RateLimitAPIPerMinute:            1000,
		PasswordMinLength:                12,
	}
}

type routerHarness struct {
	router *gin.Engine
	auth   *fakeAuth
	logs   *bytes.Buffer
	cfg    *config.Config
}

func newHarness(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter) *routerHarness {
	t.Helper()
	if cfg == nil {
		cfg = authTestConfig(t)
	}
	fake := newFakeAuth()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := NewRouter(Deps{Logger: logger, Config: cfg, Auth: fake, Limiter: limiter})
	return &routerHarness{router: router, auth: fake, logs: logs, cfg: cfg}
}

// student is a convenience account with an active session.
func (h *routerHarness) student(t *testing.T, account string) *user.User {
	t.Helper()
	u := &user.User{
		ID:          uuid.New(),
		Account:     account,
		DisplayName: "Student " + account,
		Role:        user.RoleStudent,
		Status:      user.StatusActive,
		CreatedAt:   time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	h.auth.addUser(u, "")
	return u
}

func (h *routerHarness) staff(t *testing.T, role user.Role, account, password string) *user.User {
	t.Helper()
	hash := "hash-not-used-by-the-fake"
	u := &user.User{
		ID:           uuid.New(),
		Account:      account,
		DisplayName:  string(role) + " " + account,
		Role:         role,
		Status:       user.StatusActive,
		PasswordHash: &hash,
		CreatedAt:    time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	h.auth.addUser(u, password)
	return u
}

// request performs an HTTP call against the router.
//
// remoteAddr is set explicitly because the trust decision under test depends on
// it: httptest's default (192.0.2.1:1234) is a documentation address, so tests
// that care pass their own.
func (h *routerHarness) request(method, path, body string, cookies map[string]string, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// decodeErrorCode returns error.code from the standard envelope.
func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if body.RequestID == "" {
		t.Error("error response has no requestId")
	}
	return body.Error.Code
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard error envelope: %v", err)
	}
	return body.Error.Message
}

// cookieByName returns a Set-Cookie from the response, or nil.
func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func decodeUser(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
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

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestStudentLoginSetsBothCookies(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.student(t, "S10086")

	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"s10086"}`, nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// The response is {"user": {...}} and nothing else: the DTO is the contract
	// the three frontends are built against.
	payload := decodeUser(t, rec)
	for _, field := range []string{"id", "account", "displayName", "role", "status", "createdAt", "lastLoginAt"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("user DTO is missing %q: %v", field, payload)
		}
	}
	// password_hash must never be serialised (§9/§58).
	if _, leaked := payload["passwordHash"]; leaked {
		t.Error("response contains passwordHash")
	}
	if _, leaked := payload["PasswordHash"]; leaked {
		t.Error("response contains PasswordHash")
	}
	if payload["role"] != "STUDENT" || payload["status"] != "ACTIVE" || payload["account"] != "S10086" {
		t.Errorf("user DTO = %v, want the stored account and STUDENT/ACTIVE", payload)
	}

	session := cookieByName(rec, "classwatch_session_student")
	if session == nil {
		t.Fatal("the student session cookie was not set")
	}
	if session.Value != "student-token-s10086" {
		t.Errorf("session cookie value = %q", session.Value)
	}
	if !session.HttpOnly {
		t.Error("the session cookie is not HttpOnly: an XSS would be able to read the token")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", session.Path)
	}
	if session.Secure {
		t.Error("session cookie is Secure while SESSION_COOKIE_SECURE=false (localhost development)")
	}
	if session.MaxAge != int(time.Hour.Seconds()) {
		t.Errorf("session cookie MaxAge = %d, want %d", session.MaxAge, int(time.Hour.Seconds()))
	}

	csrf := cookieByName(rec, "classwatch_session_student_csrf")
	if csrf == nil {
		t.Fatal("the CSRF cookie was not set")
	}
	// The frontend has to read this one, so it must NOT be HttpOnly.
	if csrf.HttpOnly {
		t.Error("the CSRF cookie is HttpOnly; the frontend cannot read it to send X-CSRF-Token")
	}
	if csrf.SameSite != http.SameSiteLaxMode || csrf.Path != "/" || csrf.MaxAge != int(time.Hour.Seconds()) {
		t.Errorf("csrf cookie attributes = %+v, want SameSite=Lax, Path=/, MaxAge=1h", csrf)
	}
	if csrf.Value == "" || csrf.Value == session.Value {
		t.Error("the CSRF token must be a different value from the session token")
	}
}

func TestLoginCookiesArePerEntryPoint(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"teacher001","password":"a-teacher-passphrase-1"}`, nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// Cookies are scoped by host, not by port: on localhost a single shared name
	// would let the teacher login overwrite the student's session.
	if cookieByName(rec, "classwatch_session_teacher") == nil {
		t.Error("the teacher session cookie was not set")
	}
	if cookieByName(rec, "classwatch_session_student") != nil {
		t.Error("a teacher login set the student cookie")
	}
}

func TestTeacherLoginRequiresAPassword(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"teacher001"}`, nil, nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing password (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

func TestTeacherLoginRejectsMalformedJSON(t *testing.T) {
	h := newHarness(t, nil, nil)

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":`, nil, nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

// TestLoginFailureIsIndistinguishable is the anti-enumeration test at the HTTP
// level: the two failure bodies must be byte-identical apart from the request id,
// which means the same code, the same message and the same status.
func TestLoginFailureIsIndistinguishable(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")

	unknown := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"nosuchaccount","password":"whatever-123456"}`, nil, nil, "")
	wrong := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"teacher001","password":"wrong-password-12345"}`, nil, nil, "")

	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401", unknown.Code, wrong.Code)
	}
	if code := decodeErrorCode(t, unknown); code != "INVALID_CREDENTIALS" {
		t.Errorf("unknown account code = %q, want INVALID_CREDENTIALS", code)
	}
	if code := decodeErrorCode(t, wrong); code != "INVALID_CREDENTIALS" {
		t.Errorf("wrong password code = %q, want INVALID_CREDENTIALS", code)
	}
	if errorMessage(t, unknown) != errorMessage(t, wrong) {
		t.Errorf("messages differ: %q vs %q — that difference is an account oracle", errorMessage(t, unknown), errorMessage(t, wrong))
	}

	// A rejected login must not set a cookie.
	if len(unknown.Result().Cookies()) != 0 || len(wrong.Result().Cookies()) != 0 {
		t.Error("a failed login set a cookie")
	}
}

func TestLoginDisabledAccount(t *testing.T) {
	h := newHarness(t, nil, nil)
	u := h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")
	u.Status = user.StatusDisabled

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"teacher001","password":"a-teacher-passphrase-1"}`, nil, nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "ACCOUNT_DISABLED" {
		t.Errorf("code = %q, want ACCOUNT_DISABLED", code)
	}
}

// ---------------------------------------------------------------------------
// /auth/me
// ---------------------------------------------------------------------------

func TestMeWithoutCookieIsUnauthorizedAndClearsCookies(t *testing.T) {
	h := newHarness(t, nil, nil)

	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", nil, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "AUTH_REQUIRED" {
		t.Errorf("code = %q, want AUTH_REQUIRED", code)
	}
	// The dead cookie must be removed, or the frontend keeps believing it is
	// logged in and retries forever instead of showing the login page.
	cleared := cookieByName(rec, "classwatch_session_student")
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("session cookie was not cleared on 401: %+v", cleared)
	}
}

func TestMeReturnsTheAuthenticatedUser(t *testing.T) {
	h := newHarness(t, nil, nil)
	u := h.student(t, "S10086")
	h.auth.addSession("raw-student-token", u)

	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", map[string]string{
		"classwatch_session_student": "raw-student-token",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	payload := decodeUser(t, rec)
	if payload["id"] != u.ID.String() {
		t.Errorf("id = %v, want %s", payload["id"], u.ID)
	}
	if payload["role"] != "STUDENT" {
		t.Errorf("role = %v, want STUDENT", payload["role"])
	}
	if _, leaked := payload["passwordHash"]; leaked {
		t.Error("response contains passwordHash")
	}
}

func TestMeWithExpiredSessionIsUnauthorized(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.student(t, "S10086")

	// The fake returns ErrSessionInvalid for a token it does not know, which is
	// exactly what the real service does for an expired or revoked session.
	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", map[string]string{
		"classwatch_session_student": "expired-token",
	}, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "AUTH_REQUIRED" {
		t.Errorf("code = %q, want AUTH_REQUIRED", code)
	}
}

func TestMeWithDisabledAccountIsForbidden(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.student(t, "S10086")
	h.auth.authenticateErr = auth.ErrAccountDisabled

	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", map[string]string{
		"classwatch_session_student": "any-token",
	}, nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "ACCOUNT_DISABLED" {
		t.Errorf("code = %q, want ACCOUNT_DISABLED", code)
	}
	if cleared := cookieByName(rec, "classwatch_session_student"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("the session cookie was not cleared on ACCOUNT_DISABLED")
	}
}

// ---------------------------------------------------------------------------
// Cross-entry rejection (§67)
// ---------------------------------------------------------------------------

func TestCrossEntryAccessIsForbidden(t *testing.T) {
	h := newHarness(t, nil, nil)
	student := h.student(t, "S10086")
	teacher := h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")
	admin := h.staff(t, user.RoleAdmin, "admin", "an-admin-passphrase-1")

	h.auth.addSession("student-token", student)
	h.auth.addSession("teacher-token", teacher)
	h.auth.addSession("admin-token", admin)

	cases := []struct {
		name    string
		path    string
		cookies map[string]string
		want    int
		code    string
	}{
		{
			name:    "student session on the teacher entry",
			path:    "/api/v1/teacher/auth/me",
			cookies: map[string]string{"classwatch_session_student": "student-token"},
			want:    http.StatusForbidden,
			code:    "ROLE_FORBIDDEN",
		},
		{
			name:    "teacher session on the admin entry",
			path:    "/api/v1/admin/auth/me",
			cookies: map[string]string{"classwatch_session_teacher": "teacher-token"},
			want:    http.StatusForbidden,
			code:    "ROLE_FORBIDDEN",
		},
		{
			name:    "admin session on the student entry",
			path:    "/api/v1/student/auth/me",
			cookies: map[string]string{"classwatch_session_admin": "admin-token"},
			want:    http.StatusForbidden,
			code:    "ROLE_FORBIDDEN",
		},
		{
			name:    "no session at all",
			path:    "/api/v1/teacher/auth/me",
			cookies: nil,
			want:    http.StatusUnauthorized,
			code:    "AUTH_REQUIRED",
		},
		{
			name: "a stale cookie from another entry is not authentication",
			path: "/api/v1/teacher/auth/me",
			// The student cookie exists but does not resolve to a session.
			cookies: map[string]string{"classwatch_session_student": "revoked-token"},
			want:    http.StatusUnauthorized,
			code:    "AUTH_REQUIRED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodGet, tc.path, "", tc.cookies, nil, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestRoleComesFromTheDatabaseNotTheCookie presents a teacher's session through
// the student cookie. The cookie name claims "student"; the session row says
// TEACHER, and the database wins — that is the whole point of RequireRole.
func TestRoleComesFromTheDatabaseNotTheCookie(t *testing.T) {
	h := newHarness(t, nil, nil)
	teacher := h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")
	h.auth.addSession("teacher-token", teacher)

	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", map[string]string{
		"classwatch_session_student": "teacher-token",
	}, nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "ROLE_FORBIDDEN" {
		t.Errorf("code = %q, want ROLE_FORBIDDEN", code)
	}
	// The bogus cookie must be cleared: it can never be used on this entry, and
	// leaving it would keep the student frontend convinced it is logged in.
	if cleared := cookieByName(rec, "classwatch_session_student"); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("ROLE_FORBIDDEN did not clear the entry cookie: %+v", cleared)
	}
}

func TestRoleForbiddenDoesNotClearTheOtherEntrysCookie(t *testing.T) {
	h := newHarness(t, nil, nil)
	student := h.student(t, "S10086")
	h.auth.addSession("student-token", student)

	rec := h.request(http.MethodGet, "/api/v1/teacher/auth/me", "", map[string]string{
		"classwatch_session_student": "student-token",
	}, nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// Clearing the teacher cookie (which was absent) is expected; clearing the
	// student's live session would log them out of their own console.
	for _, c := range rec.Result().Cookies() {
		if c.Name == "classwatch_session_student" && c.MaxAge < 0 {
			t.Error("a cross-entry 403 logged the user out of their own entry point")
		}
	}
}

// ---------------------------------------------------------------------------
// CSRF (§63)
// ---------------------------------------------------------------------------

func TestCSRFProtectionOnUnsafeMethods(t *testing.T) {
	h := newHarness(t, nil, nil)
	student := h.student(t, "S10086")
	principal := h.auth.addSession("student-token", student)

	cases := []struct {
		name       string
		csrfHeader string
		want       int
	}{
		{"missing header", "", http.StatusForbidden},
		{"wrong token", "not-the-token", http.StatusForbidden},
		{"token of another session", "csrf-someone-else", http.StatusForbidden},
		{"correct token", principal.CSRFToken, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.csrfHeader != "" {
				headers[headerCSRF] = tc.csrfHeader
			}
			rec := h.request(http.MethodPost, "/api/v1/student/auth/logout", "", map[string]string{
				"classwatch_session_student": "student-token",
			}, headers, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden {
				if code := decodeErrorCode(t, rec); code != "CSRF_INVALID" {
					t.Errorf("code = %q, want CSRF_INVALID", code)
				}
				if len(h.auth.logoutCalls) != 0 {
					t.Fatal("the handler ran despite a failed CSRF check")
				}
				// Both cookies must be cleared. WHY: a CSRF failure means the
				// browser's cookie state is self-contradictory, so leaving the
				// HttpOnly session cookie behind would strand the frontend in a
				// "looks logged in, every write fails" zombie state.
				for _, name := range []string{"classwatch_session_student", "classwatch_session_student_csrf"} {
					cleared := cookieByName(rec, name)
					if cleared == nil {
						t.Fatalf("%s was not cleared on CSRF failure", name)
					}
					if cleared.Value != "" || cleared.MaxAge >= 0 {
						t.Errorf("%s was not expired: %+v", name, cleared)
					}
				}
				// ...but the session itself must NOT be revoked: a failed CSRF
				// check is client-side inconsistency, not evidence of theft, and
				// revoking would give any website a logout button against any
				// logged-in user. The session is still resolvable here.
				if _, err := h.auth.Authenticate(context.Background(), "student-token"); err != nil {
					t.Fatalf("the server-side session was invalidated by a CSRF failure: %v", err)
				}
			}
		})
	}
}

func TestSafeMethodsDoNotRequireCSRF(t *testing.T) {
	h := newHarness(t, nil, nil)
	student := h.student(t, "S10086")
	h.auth.addSession("student-token", student)

	rec := h.request(http.MethodGet, "/api/v1/student/auth/me", "", map[string]string{
		"classwatch_session_student": "student-token",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/me without a CSRF header = %d, want 200", rec.Code)
	}
}

func TestLogoutRevokesAndClears(t *testing.T) {
	h := newHarness(t, nil, nil)
	student := h.student(t, "S10086")
	principal := h.auth.addSession("student-token", student)

	rec := h.request(http.MethodPost, "/api/v1/student/auth/logout", "", map[string]string{
		"classwatch_session_student": "student-token",
	}, map[string]string{headerCSRF: principal.CSRFToken}, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 response has a body: %q", rec.Body.String())
	}
	if len(h.auth.logoutCalls) != 1 || h.auth.logoutCalls[0] != "student-token" {
		t.Errorf("Logout calls = %v, want the presented token", h.auth.logoutCalls)
	}
	if cleared := cookieByName(rec, "classwatch_session_student"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout did not clear the session cookie")
	}
	if cleared := cookieByName(rec, "classwatch_session_student_csrf"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout did not clear the CSRF cookie")
	}
}

// ---------------------------------------------------------------------------
// Cookies under SESSION_COOKIE_SECURE=true
// ---------------------------------------------------------------------------

func TestSecureCookieConfiguration(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.SessionCookieSecure = true
	h := newHarness(t, cfg, nil)
	h.student(t, "S10086")

	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, name := range []string{"classwatch_session_student", "classwatch_session_student_csrf"} {
		c := cookieByName(rec, name)
		if c == nil {
			t.Fatalf("cookie %s missing", name)
		}
		if !c.Secure {
			t.Errorf("cookie %s is not Secure although SESSION_COOKIE_SECURE=true", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Rate limiting (§2.2/§63)
// ---------------------------------------------------------------------------

func TestLoginRateLimitReturns429WithRetryAfter(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.RateLimitLoginPerMinute = 2
	cfg.RateLimitLoginPerAccountPer10Min = 100
	h := newHarness(t, cfg, ratelimit.NewMemory())
	h.student(t, "S10086")

	for i := 0; i < 2; i++ {
		rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, nil, "203.0.113.9:5000")
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d = %d, want 200", i+1, rec.Code)
		}
	}

	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, nil, "203.0.113.9:5000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "RATE_LIMITED" {
		t.Errorf("code = %q, want RATE_LIMITED", code)
	}
	retryAfter := rec.Header().Get(headerRetryAfter)
	if retryAfter == "" {
		t.Fatal("429 response has no Retry-After header; the client would retry immediately")
	}
	if retryAfter == "0" {
		t.Error("Retry-After is 0")
	}
}

func TestLoginRateLimitIsPerAccount(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.RateLimitLoginPerMinute = 100
	cfg.RateLimitLoginPerAccountPer10Min = 2
	h := newHarness(t, cfg, ratelimit.NewMemory())
	h.student(t, "S10086")
	h.student(t, "S10087")

	for i := 0; i < 2; i++ {
		rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, nil, "203.0.113.9:5000")
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d = %d, want 200", i+1, rec.Code)
		}
	}
	// Same account, same IP: blocked.
	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, nil, "203.0.113.9:5000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third attempt for one account = %d, want 429", rec.Code)
	}
	// Case variations are the same account (citext): they must not buy a fresh
	// bucket.
	rec = h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"s10086"}`, nil, nil, "203.0.113.9:5000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("lowercase variant = %d, want 429 (the key must be case-insensitive)", rec.Code)
	}
	// A different account from the same IP is unaffected.
	rec = h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10087"}`, nil, nil, "203.0.113.9:5000")
	if rec.Code != http.StatusOK {
		t.Fatalf("a different account = %d, want 200", rec.Code)
	}
}

func TestAPIRateLimitCoversEveryRoute(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.RateLimitAPIPerMinute = 2
	h := newHarness(t, cfg, ratelimit.NewMemory())

	for i := 0; i < 2; i++ {
		if rec := h.request(http.MethodGet, "/api/v1/meta", "", nil, nil, "198.51.100.7:4000"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}
	rec := h.request(http.MethodGet, "/api/v1/meta", "", nil, nil, "198.51.100.7:4000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "RATE_LIMITED" {
		t.Errorf("code = %q, want RATE_LIMITED", code)
	}
}

// ---------------------------------------------------------------------------
// Proxy-header trust (the XFF spoofing test)
// ---------------------------------------------------------------------------

// TestSpoofedXForwardedForCannotBypassRateLimiting is the security regression
// test for the trust boundary: with no trusted proxies configured, rotating
// X-Forwarded-For must NOT create a fresh rate-limit bucket, and the access log
// must keep reporting the real TCP peer.
func TestSpoofedXForwardedForCannotBypassRateLimiting(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.RateLimitLoginPerMinute = 2
	cfg.RateLimitLoginPerAccountPer10Min = 100
	h := newHarness(t, cfg, ratelimit.NewMemory())
	h.student(t, "S10086")

	for i := 0; i < 2; i++ {
		rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, map[string]string{
			headerXForwardedFor: fmt.Sprintf("1.2.3.%d", i),
		}, "203.0.113.9:5000")
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d = %d, want 200", i+1, rec.Code)
		}
	}
	// A brand new forwarded address must not help: the limiter keys on the peer.
	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, map[string]string{
		headerXForwardedFor: "9.9.9.9",
	}, "203.0.113.9:5000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a spoofed X-Forwarded-For bypassed the limit", rec.Code)
	}

	// And the access log must not repeat the forged address.
	logged := h.logs.String()
	if strings.Contains(logged, "1.2.3.1") || strings.Contains(logged, "9.9.9.9") {
		t.Errorf("the access log trusted a spoofed X-Forwarded-For:\n%s", logged)
	}
	if !strings.Contains(logged, "203.0.113.9") {
		t.Errorf("the access log does not report the real peer address:\n%s", logged)
	}
}

// TestTrustedProxyXForwardedForIsUsed is the other half of the policy: behind a
// configured proxy the header IS believed, otherwise every client would share one
// bucket.
func TestTrustedProxyXForwardedForIsUsed(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.RateLimitLoginPerMinute = 1
	cfg.RateLimitLoginPerAccountPer10Min = 100
	// Testing addresses, so the tests do not depend on the host's own networks.
	_, network, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	cfg.TrustedProxies = []*net.IPNet{network}
	h := newHarness(t, cfg, ratelimit.NewMemory())
	h.student(t, "S10086")

	// Two different clients behind the trusted proxy: each gets its own bucket.
	for i, client := range []string{"198.51.100.1", "198.51.100.2"} {
		rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, map[string]string{
			headerXForwardedFor: client,
		}, "10.0.0.5:5000")
		if rec.Code != http.StatusOK {
			t.Fatalf("client %d = %d, want 200 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	// The same client again is limited.
	rec := h.request(http.MethodPost, "/api/v1/student/auth/login", `{"account":"S10086"}`, nil, map[string]string{
		headerXForwardedFor: "198.51.100.1",
	}, "10.0.0.5:5000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// And the log carries the client, not the proxy.
	if !strings.Contains(h.logs.String(), "198.51.100.1") {
		t.Errorf("access log does not show the forwarded client:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// Logging discipline (§59)
// ---------------------------------------------------------------------------

func TestHTTPLogsNeverContainCredentials(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.staff(t, user.RoleTeacher, "teacher001", "s3cret-teacher-password-9f")

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login",
		`{"account":"teacher001","password":"s3cret-teacher-password-9f"}`, nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	token := cookieByName(rec, "classwatch_session_teacher")
	if token == nil {
		t.Fatal("no session cookie")
	}
	csrf := cookieByName(rec, "classwatch_session_teacher_csrf")
	if csrf == nil {
		t.Fatal("no csrf cookie")
	}

	logged := h.logs.String()
	for _, secret := range []string{"s3cret-teacher-password-9f", token.Value, csrf.Value} {
		if strings.Contains(logged, secret) {
			t.Fatalf("a credential reached the log: %q\nlog:\n%s", secret, logged)
		}
	}
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

func TestAuthRoutesAreAbsentWithoutAService(t *testing.T) {
	// A router without an auth service (probe-only deployment, or a test) must
	// answer 404 rather than reach a handler with a nil service.
	router := NewRouter(Deps{Logger: discardLogger(), Config: authTestConfig(t)})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/student/auth/me", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUnknownAuthRouteStillUsesTheEnvelope(t *testing.T) {
	h := newHarness(t, nil, nil)
	rec := h.request(http.MethodGet, "/api/v1/student/auth/nope", "", nil, nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

func TestAuthServiceErrorsBecomeInternalErrors(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.auth.passwordErr = errors.New("database connection lost")
	h.staff(t, user.RoleTeacher, "teacher001", "a-teacher-passphrase-1")

	rec := h.request(http.MethodPost, "/api/v1/teacher/auth/login", `{"account":"teacher001","password":"a-teacher-passphrase-1"}`, nil, nil, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", code)
	}
	// The driver message must not reach the client.
	if strings.Contains(rec.Body.String(), "database connection lost") {
		t.Error("the internal cause leaked into the response body")
	}
}
