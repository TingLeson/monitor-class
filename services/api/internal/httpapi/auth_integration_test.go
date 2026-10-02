package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// End-to-end authentication against a real PostgreSQL server: the router, the
// middleware, the auth service and the SQL, wired exactly as cmd/api wires them.
// This is the test that would catch a broken JOIN, a wrong cookie name or a
// middleware installed in the wrong order — things the unit tests deliberately
// fake away.

type e2e struct {
	router *gin.Engine
	repo   *user.Postgres
	pool   *pgxpool.Pool
	cfg    *config.Config
	logs   *strings.Builder
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pool := dbtest.Pool(t)
	repo := user.NewPostgres(pool)

	cfg := &config.Config{
		AppEnv:                           config.EnvTest,
		SessionCookieName:                "classwatch_session",
		SessionTTL:                       time.Hour,
		SessionCookieSecure:              false,
		SessionIdleTouchInterval:         5 * time.Minute,
		RateLimitLoginPerMinute:          1000,
		RateLimitLoginPerAccountPer10Min: 1000,
		RateLimitAPIPerMinute:            1000,
		PasswordMinLength:                12,
	}
	service := auth.NewService(repo, sessionstore.New(pool), auth.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    auth.NewPasswordPolicy(cfg.PasswordMinLength),
	})

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := httpapi.NewRouter(httpapi.Deps{
		Logger:  logger,
		Config:  cfg,
		Auth:    service,
		Limiter: ratelimit.NewMemory(),
	})
	return &e2e{router: router, repo: repo, pool: pool, cfg: cfg, logs: logs}
}

// createStudent inserts a student account that is removed after the test.
func (e *e2e) createStudent(t *testing.T) *user.User {
	t.Helper()
	created, err := e.repo.Create(context.Background(), user.CreateParams{
		Account:     dbtest.RandomAccount("S1"),
		DisplayName: "张三",
		Role:        user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	e.cleanup(t, created.ID)
	return created
}

func (e *e2e) createStaff(t *testing.T, role user.Role, password string) *user.User {
	t.Helper()
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.repo.Create(context.Background(), user.CreateParams{
		Account:      dbtest.RandomAccount(strings.ToLower(string(role))),
		DisplayName:  string(role) + " 李四",
		Role:         role,
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	e.cleanup(t, created.ID)
	return created
}

func (e *e2e) cleanup(t *testing.T, id uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Logf("cleanup: delete user %s: %v", id, err)
		}
	})
}

// call performs a request; cookies and headers are optional.
func (e *e2e) call(method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "203.0.113.9:51234"
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *e2e) login(path, body string) (*httptest.ResponseRecorder, []*http.Cookie) {
	rec := e.call(http.MethodPost, path, body, nil, nil)
	return rec, rec.Result().Cookies()
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

func userField(t *testing.T, rec *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var body struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	value, _ := body.User[field].(string)
	return value
}

// TestStudentLoginMeLogoutFlow is the acceptance flow of §38/§41: log in with an
// account only, read the session back, log out, and lose access.
func TestStudentLoginMeLogoutFlow(t *testing.T) {
	e := newE2E(t)
	student := e.createStudent(t)

	rec, cookies := e.login("/api/v1/student/auth/login", `{"account":"`+strings.ToLower(student.Account)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// The lookup is case-insensitive: the body used the lower-cased account.
	if got := userField(t, rec, "account"); got != student.Account {
		t.Errorf("login returned account %q, want the stored %q", got, student.Account)
	}

	session := cookieNamed(cookies, "classwatch_session_student")
	csrf := cookieNamed(cookies, "classwatch_session_student_csrf")
	if session == nil || csrf == nil {
		t.Fatalf("login did not set both cookies (session=%v csrf=%v)", session, csrf)
	}

	rec = e.call(http.MethodGet, "/api/v1/student/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := userField(t, rec, "id"); got != student.ID.String() {
		t.Errorf("me id = %q, want %s", got, student.ID)
	}

	// Logout is a state-changing request, so it must carry the CSRF token.
	rec = e.call(http.MethodPost, "/api/v1/student/auth/logout", "", []*http.Cookie{session},
		map[string]string{"X-CSRF-Token": csrf.Value})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}

	// The cookie is cleared by the response, and even if the client kept it, the
	// session is revoked server-side.
	if cleared := cookieNamed(rec.Result().Cookies(), "classwatch_session_student"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout did not clear the session cookie")
	}
	rec = e.call(http.MethodGet, "/api/v1/student/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "AUTH_REQUIRED" {
		t.Errorf("code = %q, want AUTH_REQUIRED", code)
	}
}

func TestTeacherPasswordLogin(t *testing.T) {
	e := newE2E(t)
	const password = "a-teacher-passphrase-1"
	teacher := e.createStaff(t, user.RoleTeacher, password)

	// Wrong password.
	rec, _ := e.login("/api/v1/teacher/auth/login", `{"account":"`+teacher.Account+`","password":"definitely-wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	wrongPasswordCode := errorCode(t, rec)

	// Unknown account: the same code, the same message, the same status.
	rec, _ = e.login("/api/v1/teacher/auth/login", `{"account":"nobody-here","password":"definitely-wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown account status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if errorCode(t, rec) != wrongPasswordCode {
		t.Errorf("codes differ: %q vs %q", errorCode(t, rec), wrongPasswordCode)
	}
	if rec.Body.String() != "" && strings.Contains(rec.Body.String(), "nobody-here") {
		t.Error("the response echoes the attempted account")
	}

	// Correct password.
	rec, cookies := e.login("/api/v1/teacher/auth/login", `{"account":"`+teacher.Account+`","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	session := cookieNamed(cookies, "classwatch_session_teacher")
	if session == nil {
		t.Fatal("the teacher session cookie was not set")
	}
	rec = e.call(http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := userField(t, rec, "role"); got != "TEACHER" {
		t.Errorf("role = %q, want TEACHER", got)
	}
	if strings.Contains(rec.Body.String(), "passwordHash") || strings.Contains(rec.Body.String(), "$argon2id$") {
		t.Error("the response contains password material")
	}
}

// TestCrossEntryRejectionEndToEnd is the §67 test with real sessions: a student
// session must not open the teacher or admin entries, and vice versa.
func TestCrossEntryRejectionEndToEnd(t *testing.T) {
	e := newE2E(t)
	student := e.createStudent(t)
	admin := e.createStaff(t, user.RoleAdmin, "an-admin-passphrase-1")

	_, studentCookies := e.login("/api/v1/student/auth/login", `{"account":"`+student.Account+`"}`)
	studentSession := cookieNamed(studentCookies, "classwatch_session_student")
	if studentSession == nil {
		t.Fatal("student login did not set a cookie")
	}

	_, adminCookies := e.login("/api/v1/admin/auth/login", `{"account":"`+admin.Account+`","password":"an-admin-passphrase-1"}`)
	adminSession := cookieNamed(adminCookies, "classwatch_session_admin")
	if adminSession == nil {
		t.Fatal("admin login did not set a cookie")
	}

	cases := []struct {
		name    string
		path    string
		cookies []*http.Cookie
		want    int
		code    string
	}{
		{"student session on the teacher entry", "/api/v1/teacher/auth/me", []*http.Cookie{studentSession}, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"student session on the admin entry", "/api/v1/admin/auth/me", []*http.Cookie{studentSession}, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"admin session on the student entry", "/api/v1/student/auth/me", []*http.Cookie{adminSession}, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"no session on the teacher entry", "/api/v1/teacher/auth/me", nil, http.StatusUnauthorized, "AUTH_REQUIRED"},
		{"no session on the admin entry", "/api/v1/admin/auth/me", nil, http.StatusUnauthorized, "AUTH_REQUIRED"},
		{"no session on the student entry", "/api/v1/student/auth/me", nil, http.StatusUnauthorized, "AUTH_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.call(http.MethodGet, tc.path, "", tc.cookies, nil)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if code := errorCode(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestDisablingAnAccountKillsItsSessionNow is the "server-side session, not a
// token" acceptance test: no waiting for an expiry, no re-login required for the
// change to be visible on the very next request.
func TestDisablingAnAccountKillsItsSessionNow(t *testing.T) {
	e := newE2E(t)
	const password = "a-teacher-passphrase-1"
	teacher := e.createStaff(t, user.RoleTeacher, password)

	_, cookies := e.login("/api/v1/teacher/auth/login", `{"account":"`+teacher.Account+`","password":"`+password+`"}`)
	session := cookieNamed(cookies, "classwatch_session_teacher")
	if session == nil {
		t.Fatal("login did not set a cookie")
	}

	// Sanity: the session works.
	if rec := e.call(http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil); rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200", rec.Code)
	}

	// An admin disables the account.
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET status = 'DISABLED' WHERE id = $1`, teacher.ID); err != nil {
		t.Fatalf("disable teacher: %v", err)
	}

	rec := e.call(http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("me after disable = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "ACCOUNT_DISABLED" {
		t.Errorf("code = %q, want ACCOUNT_DISABLED", code)
	}

	// The session was revoked while it was being rejected, so re-enabling the
	// account must NOT bring the old session back to life.
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET status = 'ACTIVE' WHERE id = $1`, teacher.ID); err != nil {
		t.Fatalf("re-enable teacher: %v", err)
	}
	rec = e.call(http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after re-enable = %d, want 401 (%s): the revoked session came back", rec.Code, rec.Body.String())
	}
}

func TestCSRFIsRequiredEndToEnd(t *testing.T) {
	e := newE2E(t)
	student := e.createStudent(t)

	_, cookies := e.login("/api/v1/student/auth/login", `{"account":"`+student.Account+`"}`)
	session := cookieNamed(cookies, "classwatch_session_student")
	csrf := cookieNamed(cookies, "classwatch_session_student_csrf")
	if session == nil || csrf == nil {
		t.Fatal("login did not set both cookies")
	}

	// Missing header.
	rec := e.call(http.MethodPost, "/api/v1/student/auth/logout", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CSRF_INVALID" {
		t.Errorf("code = %q, want CSRF_INVALID", code)
	}

	// Both cookies must be cleared so the browser stops claiming a session it
	// cannot use — while the session itself stays valid, because a failed CSRF
	// check is client-side inconsistency, not evidence of theft (asserted below).
	for _, name := range []string{"classwatch_session_student", "classwatch_session_student_csrf"} {
		cleared := cookieNamed(rec.Result().Cookies(), name)
		if cleared == nil {
			t.Fatalf("%s was not cleared on a CSRF failure", name)
		}
		if cleared.Value != "" || cleared.MaxAge >= 0 {
			t.Errorf("%s was not expired: %+v", name, cleared)
		}
	}

	// Wrong token.
	rec = e.call(http.MethodPost, "/api/v1/student/auth/logout", "", []*http.Cookie{session},
		map[string]string{"X-CSRF-Token": "forged-token"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout with a forged CSRF token = %d, want 403", rec.Code)
	}

	// The session is still alive after the rejected attempts: the same token still
	// authenticates, and the row is still resolvable straight from the store.
	rec = e.call(http.MethodGet, "/api/v1/student/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me after rejected CSRF attempts = %d, want 200: a CSRF failure must not revoke the session", rec.Code)
	}
	if _, err := sessionstore.New(e.pool).FindByTokenHash(context.Background(), auth.HashToken(session.Value)); err != nil {
		t.Fatalf("the session row was revoked by a CSRF failure: %v", err)
	}

	// Correct token.
	rec = e.call(http.MethodPost, "/api/v1/student/auth/logout", "", []*http.Cookie{session},
		map[string]string{"X-CSRF-Token": csrf.Value})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout with the correct CSRF token = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// TestLogsCarryIdentityButNoSecrets is the §59 test at the HTTP layer: the
// successful login must be auditable (user_id, role) without the log ever
// containing a credential.
func TestLogsCarryIdentityButNoSecrets(t *testing.T) {
	e := newE2E(t)
	const password = "s3cret-teacher-password-9f"
	teacher := e.createStaff(t, user.RoleTeacher, password)

	_, cookies := e.login("/api/v1/teacher/auth/login", `{"account":"`+teacher.Account+`","password":"`+password+`"}`)
	session := cookieNamed(cookies, "classwatch_session_teacher")
	if session == nil {
		t.Fatal("login did not set a cookie")
	}
	csrf := cookieNamed(cookies, "classwatch_session_teacher_csrf")

	e.call(http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	e.call(http.MethodPost, "/api/v1/teacher/auth/logout", "", []*http.Cookie{session},
		map[string]string{"X-CSRF-Token": csrf.Value})

	logged := e.logs.String()
	for _, secret := range []string{password, session.Value, csrf.Value} {
		if strings.Contains(logged, secret) {
			t.Fatalf("a credential reached the log: %q", secret)
		}
	}
	if !strings.Contains(logged, teacher.ID.String()) {
		t.Error("the log does not carry user_id, so a login cannot be audited")
	}
	if !strings.Contains(logged, "TEACHER") {
		t.Error("the log does not carry the role")
	}
}
