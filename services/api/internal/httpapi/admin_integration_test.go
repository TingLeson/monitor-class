package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/admin"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/httpapi"
	"github.com/classwatch/classwatch/services/api/internal/ratelimit"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Admin user management against a real PostgreSQL server: the router, the three
// middleware layers, the admin service, the repository SQL and a real session
// store, wired exactly as cmd/api wires them.
//
// This is the layer that catches what unit tests deliberately fake away: a WHERE
// clause that filters the wrong column, an ORDER BY that is not stable, a password
// hash that is written but never verified against the new password, a session that
// is revoked in the service but not in the database.

type adminE2E struct {
	router  *gin.Engine
	repo    *user.Postgres
	service *admin.Service
	pool    *pgxpool.Pool
	cfg     *config.Config
	logs    *strings.Builder
}

func newAdminE2E(t *testing.T) *adminE2E {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pool := dbtest.Pool(t)
	repo := user.NewPostgres(pool)
	sessions := sessionstore.New(pool)

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
	policy := auth.NewPasswordPolicy(cfg.PasswordMinLength)
	authService := auth.NewService(repo, sessions, auth.Config{
		SessionTTL:        cfg.SessionTTL,
		IdleTouchInterval: cfg.SessionIdleTouchInterval,
		PasswordPolicy:    policy,
	})
	adminService := admin.NewService(repo, sessions, policy)

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := httpapi.NewRouter(httpapi.Deps{
		Logger:  logger,
		Config:  cfg,
		Auth:    authService,
		Admin:   adminService,
		Limiter: ratelimit.NewMemory(),
	})
	return &adminE2E{
		router: router, repo: repo, service: adminService, pool: pool, cfg: cfg, logs: logs,
	}
}

// admin is an administrator account whose sessions are cleaned up afterwards.
func (e *adminE2E) admin(t *testing.T, password string) *user.User {
	t.Helper()
	return e.staff(t, user.RoleAdmin, password)
}

func (e *adminE2E) staff(t *testing.T, role user.Role, password string) *user.User {
	t.Helper()
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	created, err := e.repo.Create(context.Background(), user.CreateParams{
		Account:      dbtest.RandomAccount(strings.ToLower(string(role))),
		DisplayName:  string(role) + " 测试",
		Role:         role,
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	e.cleanup(t, created.ID)
	return created
}

func (e *adminE2E) cleanup(t *testing.T, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			if _, err := e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: delete user %s: %v", id, err)
			}
		}
	})
}

// login returns the session and CSRF cookies of a successful login.
//
// The student entry takes no password at all (§2.2), so an empty password builds
// the account-only body that entry expects.
func (e *adminE2E) login(t *testing.T, entry, account, password string) []*http.Cookie {
	t.Helper()
	body := `{"account":"` + account + `"}`
	if password != "" {
		body = `{"account":"` + account + `","password":"` + password + `"}`
	}
	rec := e.call(t, http.MethodPost, "/api/v1/"+entry+"/auth/login", body, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s login: status = %d (%s)", entry, rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()
}

func (e *adminE2E) call(t *testing.T, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
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
	req.RemoteAddr = "198.51.100.20:34567"
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// adminCall performs an authenticated admin request, carrying the CSRF token when
// the method is unsafe.
func (e *adminE2E) adminCall(t *testing.T, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if method != http.MethodGet {
		csrf := cookieNamed(cookies, "classwatch_session_admin_csrf")
		if csrf == nil {
			t.Fatal("the admin session has no CSRF cookie")
		}
		headers = map[string]string{"X-CSRF-Token": csrf.Value}
	}
	return e.call(t, method, path, body, cookies, headers)
}

// errorMessage returns error.message from the standard envelope.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard error envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Message
}

// jsonBody decodes a response into a generic map.
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

// userOf returns the `user` object of a response.
func userOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := jsonBody(t, rec)
	u, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("response has no user object: %s", rec.Body.String())
	}
	return u
}

// assertNoPasswordMaterial is the §9/§58 assertion that runs on every response
// this file inspects.
func assertNoPasswordMaterial(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, forbidden := range []string{"passwordHash", "password_hash", "$argon2id$", "createdBy", "created_by"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the response contains %q: %s", forbidden, body)
		}
	}
}

// countRows runs a count query for the assertions that must look behind the API.
func (e *adminE2E) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return count
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// TestCreateTeacherHashesThePasswordInTheDatabase walks the full path — HTTP, service,
// SQL — and then checks the row itself: a teacher must come back with an Argon2id
// hash that is not the password, and a student with nothing at all.
func TestCreateTeacherHashesThePasswordInTheDatabase(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	const teacherPassword = "a-teacher-passphrase-2"
	teacherAccount := dbtest.RandomAccount("teacher")
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+teacherAccount+`","displayName":"李老师","role":"TEACHER","password":"`+teacherPassword+`"}`,
		cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	dto := userOf(t, rec)
	assertNoPasswordMaterial(t, rec)
	if dto["role"] != "TEACHER" || dto["status"] != "ACTIVE" {
		t.Errorf("dto role/status = %v/%v, want TEACHER/ACTIVE", dto["role"], dto["status"])
	}
	if _, ok := dto["updatedAt"]; !ok {
		t.Error("the DTO has no updatedAt")
	}
	id, err := uuid.Parse(dto["id"].(string))
	if err != nil {
		t.Fatalf("dto id is not a UUID: %v", err)
	}
	e.cleanup(t, id)
	if got := rec.Header().Get("Location"); got != "/api/v1/admin/users/"+id.String() {
		t.Errorf("Location = %q, want the new account's URL", got)
	}

	// The row itself: the hash is Argon2id, differs from the password, and
	// verifies against it.
	var storedHash *string
	var createdBy *uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT password_hash, created_by FROM users WHERE id = $1`, id).Scan(&storedHash, &createdBy); err != nil {
		t.Fatalf("read back the created teacher: %v", err)
	}
	if storedHash == nil {
		t.Fatal("no password hash was stored for a TEACHER")
	}
	if !strings.HasPrefix(*storedHash, "$argon2id$") {
		t.Errorf("stored hash = %q, want an Argon2id PHC string", *storedHash)
	}
	if *storedHash == teacherPassword || strings.Contains(*storedHash, teacherPassword) {
		t.Error("the plaintext password is stored")
	}
	ok, _, err := auth.Verify(*storedHash, teacherPassword)
	if err != nil || !ok {
		t.Fatalf("Verify(stored hash, password) = %v, %v; the teacher could not log in", ok, err)
	}
	if createdBy == nil || *createdBy != adminUser.ID {
		t.Errorf("created_by = %v, want the acting administrator %s", createdBy, adminUser.ID)
	}

	// The created teacher can actually log in through the teacher entry.
	sessionCookies := e.login(t, "teacher", teacherAccount, teacherPassword)
	if cookieNamed(sessionCookies, "classwatch_session_teacher") == nil {
		t.Fatal("the new teacher could not log in")
	}
}

func TestCreateStudentStoresNoPassword(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	account := dbtest.RandomAccount("S1")
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+account+`","displayName":"张三","role":"STUDENT"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	dto := userOf(t, rec)
	id, _ := uuid.Parse(dto["id"].(string))
	e.cleanup(t, id)
	assertNoPasswordMaterial(t, rec)

	var storedHash *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT password_hash FROM users WHERE id = $1`, id).Scan(&storedHash); err != nil {
		t.Fatalf("read back the created student: %v", err)
	}
	if storedHash != nil {
		t.Errorf("a STUDENT row has a password hash: %q (§2.2)", *storedHash)
	}

	// A password in a student body is refused, and nothing is created.
	withPassword := dbtest.RandomAccount("S1")
	rec = e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+withPassword+`","displayName":"李四","role":"STUDENT","password":"a-student-passphrase"}`, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("student with a password: status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	if got := e.countRows(t, `SELECT count(*) FROM users WHERE account = $1::citext`, withPassword); got != 0 {
		t.Error("a student was created despite the rejected password")
	}
}

func TestCreateAdminIsRefusedEndToEnd(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")
	account := dbtest.RandomAccount("admin")

	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+account+`","displayName":"另一个管理员","role":"ADMIN","password":"an-admin-passphrase-2"}`, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	if !strings.Contains(errorMessage(t, rec), "adminctl") {
		t.Errorf("message = %q, want the adminctl explanation", errorMessage(t, rec))
	}
	if got := e.countRows(t, `SELECT count(*) FROM users WHERE account = $1::citext`, account); got != 0 {
		t.Fatal("the API created an ADMIN account: that is a privilege-escalation surface")
	}
}

func TestCreateDuplicateAccountIs409(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	account := dbtest.RandomAccount("S2")
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+account+`","displayName":"张三","role":"STUDENT"}`, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	dto := userOf(t, rec)
	id, _ := uuid.Parse(dto["id"].(string))
	e.cleanup(t, id)

	// The same account in a different letter case: citext makes them one account.
	rec = e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+strings.ToUpper(account)+`","displayName":"张三","role":"STUDENT"}`, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "ACCOUNT_ALREADY_EXISTS" {
		t.Errorf("code = %q, want ACCOUNT_ALREADY_EXISTS", code)
	}
}

func TestCreateTeacherPasswordPolicyIsEnforced(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"`+dbtest.RandomAccount("teacher")+`","displayName":"李老师","role":"TEACHER","password":"short"}`, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "PASSWORD_POLICY_VIOLATION" {
		t.Errorf("code = %q, want PASSWORD_POLICY_VIOLATION", code)
	}
	if !strings.Contains(errorMessage(t, rec), "12") {
		t.Errorf("message = %q, want the configured minimum length", errorMessage(t, rec))
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// TestListFiltersPaginationAndStableOrder is the acceptance test for the list
// contract. The stable-order half matters most: paging with an unstable sort is
// how an admin sees one row twice and never sees another.
func TestListFiltersPaginationAndStableOrder(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	// A private namespace of accounts, so the assertions do not depend on what
	// other tests have left in the shared database.
	tag := dbtest.RandomHex(5)
	teacherAID := e.createViaAPI(t, cookies, "t_"+tag+"_a", "老师 甲", "TEACHER")
	teacherBID := e.createViaAPI(t, cookies, "t_"+tag+"_b", "老师 乙", "TEACHER")
	studentAID := e.createViaAPI(t, cookies, "s_"+tag+"_a", "学生 甲", "STUDENT")

	// Filter by role.
	rec := e.adminCall(t, http.MethodGet, "/api/v1/admin/users?role=TEACHER&q="+tag, "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	if total := int(body["total"].(float64)); total != 2 {
		t.Errorf("total = %d, want 2 (%s)", total, rec.Body.String())
	}
	assertNoPasswordMaterial(t, rec)

	// Filter by status, after disabling one of them.
	rec = e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+teacherBID.String()+"/status", `{"status":"DISABLED"}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users?role=TEACHER&status=DISABLED&q="+tag, "", cookies)
	body = jsonBody(t, rec)
	if total := int(body["total"].(float64)); total != 1 {
		t.Errorf("total after disabling = %d, want 1 (%s)", total, rec.Body.String())
	}
	users := usersOf(t, body)
	if len(users) != 1 || users[0]["id"] != teacherBID.String() {
		t.Errorf("disabled filter returned %v, want only %s", users, teacherBID)
	}

	// Search by display name fragment and by account fragment.
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q="+tag+"_a", "", cookies)
	body = jsonBody(t, rec)
	if total := int(body["total"].(float64)); total != 2 {
		t.Errorf("account search total = %d, want 2 (teacher A and student A)", total)
	}
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q=%E8%80%81%E5%B8%88", "", cookies) // "老师"
	body = jsonBody(t, rec)
	if total := int(body["total"].(float64)); total < 2 {
		t.Errorf("display-name search total = %d, want at least the two created teachers", total)
	}

	// Pagination over the private namespace: 3 accounts, page size 2.
	page1 := jsonBody(t, e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q="+tag+"&page=1&pageSize=2", "", cookies))
	page2 := jsonBody(t, e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q="+tag+"&page=2&pageSize=2", "", cookies))
	if got := int(page1["total"].(float64)); got != 3 {
		t.Fatalf("total = %d, want 3", got)
	}
	first, second := usersOf(t, page1), usersOf(t, page2)
	if len(first) != 2 || len(second) != 1 {
		t.Fatalf("page sizes = %d/%d, want 2/1", len(first), len(second))
	}
	seen := map[string]int{}
	for _, u := range append(append([]map[string]any{}, first...), second...) {
		seen[u["id"].(string)]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("account %s appeared %d times across two pages", id, count)
		}
	}
	for _, want := range []uuid.UUID{teacherAID, teacherBID, studentAID} {
		if seen[want.String()] == 0 {
			t.Errorf("account %s was skipped while paging", want)
		}
	}

	// The order itself: newest first. Each of these accounts was created by its own
	// HTTP request, so their created_at values differ by microseconds and the
	// RFC3339 rendering of two rows can coincide (it truncates to the second) while
	// the full-precision values do not. The exact tie-breaking by id DESC is
	// therefore asserted where the ties are real — the repository test seeds a page
	// inside one transaction — and what this test can assert is the visible property:
	// the sequence never goes back in time, and paging reproduces it exactly.
	//
	// The full page is fetched first because it is the reference the paging queries
	// below are compared against.
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q="+tag+"&pageSize=200", "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("full list: status = %d (%s)", rec.Code, rec.Body.String())
	}
	all := usersOf(t, jsonBody(t, rec))
	if len(all) != 3 {
		t.Fatalf("full list returned %d rows, want 3 (%s)", len(all), rec.Body.String())
	}
	for i := 1; i < len(all); i++ {
		prevCreated := all[i-1]["createdAt"].(string)
		curCreated := all[i]["createdAt"].(string)
		if prevCreated < curCreated {
			t.Errorf("order is not newest-first: %s before %s", prevCreated, curCreated)
		}
	}

	// The two page queries must agree with the full result, entry for entry: this
	// is the property a stable ORDER BY buys.
	fromPages := append(append([]map[string]any{}, first...), second...)
	for i := range fromPages {
		if fromPages[i]["id"] != all[i]["id"] {
			t.Errorf("position %d: paging returned %v, full list returned %v", i, fromPages[i]["id"], all[i]["id"])
		}
	}
}

// usersOf extracts the `users` array of a list response.
func usersOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["users"].([]any)
	if !ok {
		t.Fatalf("response has no users array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		u, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("users entry is not an object: %v", entry)
		}
		out = append(out, u)
	}
	return out
}

// createViaAPI creates an account through the API and returns its id.
func (e *adminE2E) createViaAPI(t *testing.T, cookies []*http.Cookie, account, displayName, role string) uuid.UUID {
	t.Helper()
	body := `{"account":"` + account + `","displayName":"` + displayName + `","role":"` + role + `"}`
	if role == "TEACHER" {
		body = `{"account":"` + account + `","displayName":"` + displayName + `","role":"TEACHER","password":"a-teacher-passphrase-3"}`
	}
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/users", body, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status = %d (%s)", account, rec.Code, rec.Body.String())
	}
	dto := userOf(t, rec)
	id, err := uuid.Parse(dto["id"].(string))
	if err != nil {
		t.Fatalf("created id is not a UUID: %v", err)
	}
	e.cleanup(t, id)
	return id
}

func TestListEmptyResultIsAnEmptyArrayEndToEnd(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	rec := e.adminCall(t, http.MethodGet, "/api/v1/admin/users?q="+dbtest.RandomHex(8), "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"users":[]`) {
		t.Errorf("empty result = %s, want an empty array", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Get / Patch
// ---------------------------------------------------------------------------

func TestGetAndPatchUser(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	id := e.createViaAPI(t, cookies, "s_"+dbtest.RandomHex(5), "张三", "STUDENT")

	rec := e.adminCall(t, http.MethodGet, "/api/v1/admin/users/"+id.String(), "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", rec.Code, rec.Body.String())
	}
	assertNoPasswordMaterial(t, rec)
	before := userOf(t, rec)

	// updated_at is bumped by the rename. The DTO renders RFC3339, i.e. to the
	// second, so the wait has to cross a second boundary for the JSON comparison to
	// be meaningful; the microsecond-level check below reads the column directly and
	// does not depend on it.
	var updatedBefore time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT updated_at FROM users WHERE id = $1`, id).Scan(&updatedBefore); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	rec = e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+id.String(), `{"displayName":"  李四  "}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d (%s)", rec.Code, rec.Body.String())
	}
	after := userOf(t, rec)
	if after["displayName"] != "李四" {
		t.Errorf("displayName = %v, want the trimmed value", after["displayName"])
	}
	if after["updatedAt"] == before["updatedAt"] {
		t.Errorf("updatedAt did not change: %v", after["updatedAt"])
	}
	if after["createdAt"] != before["createdAt"] {
		t.Errorf("createdAt changed on a rename: %v -> %v", before["createdAt"], after["createdAt"])
	}
	for _, field := range []string{"id", "account", "displayName", "role", "status", "createdAt", "updatedAt", "lastLoginAt"} {
		if _, ok := after[field]; !ok {
			t.Errorf("DTO is missing %q", field)
		}
	}
	assertNoPasswordMaterial(t, rec)

	// The stored row agrees, role/status are untouched, and updated_at really moved
	// forward at microsecond resolution.
	var displayName, role, status string
	var updatedAfter time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT display_name, role, status, updated_at FROM users WHERE id = $1`, id).
		Scan(&displayName, &role, &status, &updatedAfter); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if displayName != "李四" || role != "STUDENT" || status != "ACTIVE" {
		t.Errorf("row = %q/%q/%q, want 李四/STUDENT/ACTIVE", displayName, role, status)
	}
	if !updatedAfter.After(updatedBefore) {
		t.Errorf("updated_at did not move forward: %s -> %s", updatedBefore, updatedAfter)
	}

	// A forbidden field is refused and changes nothing.
	rec = e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+id.String(), `{"displayName":"王五","role":"TEACHER"}`, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("role in a PATCH body: status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT display_name, role FROM users WHERE id = $1`, id).Scan(&displayName, &role); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if displayName != "李四" || role != "STUDENT" {
		t.Errorf("a rejected PATCH changed the row: %q/%q", displayName, role)
	}

	// An unknown id is a 404, and a malformed one a 400.
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users/"+uuid.New().String(), "", cookies)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "USER_NOT_FOUND" {
		t.Errorf("unknown id: status/code = %d/%q, want 404/USER_NOT_FOUND", rec.Code, errorCode(t, rec))
	}
	rec = e.adminCall(t, http.MethodGet, "/api/v1/admin/users/not-a-uuid", "", cookies)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "INVALID_REQUEST" {
		t.Errorf("malformed id: status/code = %d/%q, want 400/INVALID_REQUEST", rec.Code, errorCode(t, rec))
	}
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// TestDisablingATeacherKillsTheirLiveSession is the end-to-end version of the
// requirement that matters most operationally: a disabled account loses its
// session on the NEXT request, with a real login and a real session row.
func TestDisablingATeacherKillsTheirLiveSession(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	adminCookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	const password = "a-teacher-passphrase-4"
	teacher := e.staff(t, user.RoleTeacher, password)
	teacherCookies := e.login(t, "teacher", teacher.Account, password)
	session := cookieNamed(teacherCookies, "classwatch_session_teacher")
	if session == nil {
		t.Fatal("the teacher login set no session cookie")
	}

	// Sanity: the session works.
	if rec := e.call(t, http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil); rec.Code != http.StatusOK {
		t.Fatalf("me before disable = %d (%s)", rec.Code, rec.Body.String())
	}

	rec := e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+teacher.ID.String()+"/status", `{"status":"DISABLED"}`, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if userOf(t, rec)["status"] != "DISABLED" {
		t.Errorf("response status = %v, want DISABLED", userOf(t, rec)["status"])
	}

	// The session row itself is revoked: this is the strongest form of the
	// assertion, because it does not depend on which error the middleware chose.
	revoked := e.countRows(t,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NOT NULL`, teacher.ID)
	if revoked == 0 {
		t.Fatal("disabling the account did not revoke its sessions in the database")
	}

	// And the very next request is rejected.
	rec = e.call(t, http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("me after disable = %d, want 401 or 403 (%s)", rec.Code, rec.Body.String())
	}

	// Re-enabling does NOT resurrect the session: the teacher logs in again.
	rec = e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+teacher.ID.String()+"/status", `{"status":"ACTIVE"}`, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = e.call(t, http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{session}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after re-enable = %d, want 401: a revoked session came back", rec.Code)
	}

	// The account is usable again after a fresh login.
	fresh := e.login(t, "teacher", teacher.Account, password)
	if rec := e.call(t, http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{cookieNamed(fresh, "classwatch_session_teacher")}, nil); rec.Code != http.StatusOK {
		t.Fatalf("me after a fresh login = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestStatusIsIdempotentAndSelfDisableIsRefused(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	id := e.createViaAPI(t, cookies, "s_"+dbtest.RandomHex(5), "张三", "STUDENT")

	// Disabling an already-disabled account succeeds.
	for i := 0; i < 2; i++ {
		rec := e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+id.String()+"/status", `{"status":"DISABLED"}`, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("disable #%d: status = %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	// Disabling yourself is refused: the usual cause is a mis-click, and the
	// result would be an administrator locked out mid-task. It carries its own
	// code (409) so the console can say exactly which rule stopped it instead of
	// showing a generic "invalid request".
	rec := e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+adminUser.ID.String()+"/status", `{"status":"DISABLED"}`, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-disable: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "CANNOT_DISABLE_SELF" {
		t.Errorf("self-disable: code = %q, want CANNOT_DISABLE_SELF", code)
	}
	if message := errorMessage(t, rec); message == "" || strings.Contains(message, "CANNOT_DISABLE_SELF") {
		t.Errorf("self-disable message = %q, want a human sentence without the code inside it", message)
	}

	// An unknown id is 404 rather than a silent success.
	rec = e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+uuid.New().String()+"/status", `{"status":"DISABLED"}`, cookies)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "USER_NOT_FOUND" {
		t.Errorf("unknown id: status/code = %d/%q, want 404/USER_NOT_FOUND", rec.Code, errorCode(t, rec))
	}
}

// TestTheLastActiveAdminCannotBeDisabled exercises the guard against the real SQL.
//
// The test owns the state it needs: the two administrators it creates are the only
// ACTIVE admins the database is allowed to have for the duration, and the previous
// state is restored by the cleanup registered first.
func TestTheLastActiveAdminCannotBeDisabled(t *testing.T) {
	e := newAdminE2E(t)
	ctx := context.Background()

	// Park every pre-existing active admin, and restore them on the way out.
	rows, err := e.pool.Query(ctx, `SELECT id FROM users WHERE role = 'ADMIN' AND status = 'ACTIVE'`)
	if err != nil {
		t.Fatalf("list active admins: %v", err)
	}
	var parked []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parked = append(parked, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate active admins: %v", err)
	}
	t.Cleanup(func() {
		for _, id := range parked {
			if _, err := e.pool.Exec(context.Background(),
				`UPDATE users SET status = 'ACTIVE' WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: re-enable admin %s: %v", id, err)
			}
		}
	})
	if len(parked) > 0 {
		if _, err := e.pool.Exec(ctx,
			`UPDATE users SET status = 'DISABLED' WHERE role = 'ADMIN' AND status = 'ACTIVE'`); err != nil {
			t.Fatalf("park active admins: %v", err)
		}
	}

	first := e.admin(t, "an-admin-passphrase-1")
	second := e.admin(t, "an-admin-passphrase-2")
	cookies := e.login(t, "admin", first.Account, "an-admin-passphrase-1")

	// Two active admins: disabling the other one is allowed.
	rec := e.adminCall(t, http.MethodPatch, "/api/v1/admin/users/"+second.ID.String()+"/status", `{"status":"DISABLED"}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling a second administrator: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Now `first` is the only active administrator. The request that would remove
	// them cannot come from their own session (that is the "not yourself" rule), so
	// it is made through the service with an unrelated actor: this is the shape of
	// "one admin disables another", and it must fail on the system-wide rule.
	if _, err := e.service.SetStatus(ctx, admin.SetStatusInput{
		TargetID: first.ID,
		Status:   user.StatusDisabled,
		ActorID:  uuid.New(), // an unrelated actor: the "not yourself" rule must not fire
	}); !errors.Is(err, admin.ErrLastAdmin) {
		t.Fatalf("disabling the last active administrator: error = %v, want ErrLastAdmin", err)
	}

	var status string
	if err := e.pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, first.ID).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "ACTIVE" {
		t.Errorf("status = %q, want ACTIVE: the last administrator was disabled", status)
	}
}

// ---------------------------------------------------------------------------
// Password reset
// ---------------------------------------------------------------------------

// TestResetPasswordEndToEnd is the §41 acceptance test: after a reset the old
// password is dead, the new one works, and the sessions that existed before are
// gone.
func TestResetPasswordEndToEnd(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	adminCookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	const oldPassword = "the-old-teacher-passphrase"
	teacher := e.staff(t, user.RoleTeacher, oldPassword)
	oldSession := cookieNamed(e.login(t, "teacher", teacher.Account, oldPassword), "classwatch_session_teacher")
	if oldSession == nil {
		t.Fatal("no teacher session cookie")
	}

	const newPassword = "the-new-teacher-passphrase"
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password",
		`{"password":"`+newPassword+`"}`, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	assertNoPasswordMaterial(t, rec)
	if _, present := body["password"]; present {
		t.Error("the response carries a password field although the admin supplied one")
	}
	if _, ok := body["user"].(map[string]any); !ok {
		t.Fatalf("the response has no user object: %s", rec.Body.String())
	}

	// The new hash verifies against the new password and not the old one.
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE id = $1`, teacher.ID).Scan(&stored); err != nil {
		t.Fatalf("read back hash: %v", err)
	}
	if !strings.HasPrefix(stored, "$argon2id$") {
		t.Errorf("stored hash = %q, want an Argon2id PHC string", stored)
	}
	if ok, _, err := auth.Verify(stored, newPassword); err != nil || !ok {
		t.Fatalf("the new password does not verify: %v %v", ok, err)
	}
	if ok, _, _ := auth.Verify(stored, oldPassword); ok {
		t.Error("the old password still verifies")
	}

	// Behaviour, not just storage: the old password is rejected, the new one
	// works, and the pre-reset session is dead.
	rec = e.call(t, http.MethodPost, "/api/v1/teacher/auth/login",
		`{"account":"`+teacher.Account+`","password":"`+oldPassword+`"}`, nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	rec = e.call(t, http.MethodPost, "/api/v1/teacher/auth/login",
		`{"account":"`+teacher.Account+`","password":"`+newPassword+`"}`, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the new password = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rec = e.call(t, http.MethodGet, "/api/v1/teacher/auth/me", "", []*http.Cookie{oldSession}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the pre-reset session is still usable: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestResetPasswordGeneratesAndReturnsAValueOnce proves the generated-password
// path end to end: the value the API returns is the one that logs in, and it
// appears in no log line.
func TestResetPasswordGeneratesAndReturnsAValueOnce(t *testing.T) {
	e := newAdminE2E(t)
	const generated = "generated-password-for-e2e-1"
	e.service.SetPasswordGeneratorForTest(func() (string, error) { return generated, nil })

	adminUser := e.admin(t, "an-admin-passphrase-1")
	adminCookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")
	teacher := e.staff(t, user.RoleTeacher, "the-old-teacher-passphrase")

	// No body at all: the server generates the value.
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password", "", adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	if body["password"] != generated {
		t.Fatalf("password = %v, want the generated value", body["password"])
	}
	assertNoPasswordMaterial(t, rec)

	// The returned value is the one that logs in.
	rec = e.call(t, http.MethodPost, "/api/v1/teacher/auth/login",
		`{"account":"`+teacher.Account+`","password":"`+generated+`"}`, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the generated password = %d (%s)", rec.Code, rec.Body.String())
	}

	// §59: the value exists in that one response and nowhere in the logs.
	logged := e.logs.String()
	if logged == "" {
		t.Fatal("no logs were produced")
	}
	if strings.Contains(logged, generated) {
		t.Fatal("the generated password reached the log")
	}
	if strings.Contains(logged, "$argon2id$") {
		t.Fatal("a password hash reached the log")
	}
	if !strings.Contains(logged, "user.password_reset") {
		t.Error("the reset produced no audit line")
	}
	if !strings.Contains(logged, teacher.ID.String()) || !strings.Contains(logged, adminUser.ID.String()) {
		t.Error("the audit line is missing the actor or the target")
	}
}

func TestResetPasswordRejectsNonTeacherTargetsEndToEnd(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	studentID := e.createViaAPI(t, cookies, "s_"+dbtest.RandomHex(5), "张三", "STUDENT")
	otherAdmin := e.admin(t, "an-admin-passphrase-3")

	// A student has no credential to reset and an administrator's account is
	// managed out-of-band: both are 400, not a silent success.
	for _, target := range []uuid.UUID{studentID, otherAdmin.ID} {
		rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/teachers/"+target.String()+"/reset-password", "", cookies)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("target %s: status = %d, want 400 (%s)", target, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "INVALID_REQUEST" {
			t.Errorf("target %s: code = %q, want INVALID_REQUEST", target, code)
		}
	}

	// A genuinely unknown id is a 404, not a 400.
	rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/teachers/"+uuid.New().String()+"/reset-password", "", cookies)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "USER_NOT_FOUND" {
		t.Errorf("unknown target: status/code = %d/%q, want 404/USER_NOT_FOUND", rec.Code, errorCode(t, rec))
	}
}

// ---------------------------------------------------------------------------
// RBAC end to end
// ---------------------------------------------------------------------------

// TestNonAdminSessionsAreRefusedOnAdminRoutesEndToEnd uses real sessions of the
// other two entries: the admin group must refuse them, and it must never reach the
// service.
func TestNonAdminSessionsAreRefusedOnAdminRoutesEndToEnd(t *testing.T) {
	e := newAdminE2E(t)
	ctx := context.Background()

	const teacherPassword = "a-teacher-passphrase-5"
	teacher := e.staff(t, user.RoleTeacher, teacherPassword)
	teacherCookies := e.login(t, "teacher", teacher.Account, teacherPassword)

	student, err := e.repo.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("S3"), DisplayName: "张三", Role: user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	e.cleanup(t, student.ID)
	studentCookies := e.login(t, "student", student.Account, "")

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		cookies []*http.Cookie
		want    int
		code    string
	}{
		{"teacher on the list", http.MethodGet, "/api/v1/admin/users", "", teacherCookies, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"teacher on create", http.MethodPost, "/api/v1/admin/users", `{"account":"x","displayName":"X","role":"STUDENT"}`, teacherCookies, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"student on the list", http.MethodGet, "/api/v1/admin/users", "", studentCookies, http.StatusForbidden, "ROLE_FORBIDDEN"},
		{"no session at all", http.MethodGet, "/api/v1/admin/users", "", nil, http.StatusUnauthorized, "AUTH_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.method != http.MethodGet {
				for _, c := range tc.cookies {
					if strings.HasSuffix(c.Name, "_csrf") {
						headers["X-CSRF-Token"] = c.Value
					}
				}
			}
			rec := e.call(t, tc.method, tc.path, tc.body, tc.cookies, headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if code := errorCode(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}

	// An admin request without the CSRF token is refused even with a valid
	// session, and a wrong method is a 405 with the standard envelope.
	adminAccount := e.admin(t, "an-admin-passphrase-1")
	adminCookies := e.login(t, "admin", adminAccount.Account, "an-admin-passphrase-1")
	sessionOnly := []*http.Cookie{cookieNamed(adminCookies, "classwatch_session_admin")}
	rec := e.call(t, http.MethodPost, "/api/v1/admin/users",
		`{"account":"x","displayName":"X","role":"STUDENT"}`, sessionOnly, nil)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "CSRF_INVALID" {
		t.Errorf("write without CSRF: status/code = %d/%q, want 403/CSRF_INVALID", rec.Code, errorCode(t, rec))
	}

	rec = e.call(t, http.MethodDelete, "/api/v1/admin/users/"+student.ID.String(), "", adminCookies,
		map[string]string{"X-CSRF-Token": cookieNamed(adminCookies, "classwatch_session_admin_csrf").Value})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on a user: status = %d, want 405 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAdminDTOOfALoggedInAccountCarriesLastLogin is a small cross-check between
// the two DTOs: the admin list must show the real last_login_at, which is written
// by the auth service.
func TestAdminDTOOfALoggedInAccountCarriesLastLogin(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	rec := e.adminCall(t, http.MethodGet, "/api/v1/admin/users/"+adminUser.ID.String(), "", cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	dto := userOf(t, rec)
	if dto["lastLoginAt"] == nil {
		t.Error("lastLoginAt is null right after a login")
	}
	createdAt, _ := dto["createdAt"].(string)
	updatedAt, _ := dto["updatedAt"].(string)
	if createdAt == "" || updatedAt == "" {
		t.Fatalf("timestamps are empty: %v", dto)
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		t.Errorf("updatedAt is not RFC3339: %q", updatedAt)
	}
}

// TestGeneratedPasswordsAreUniqueAcrossResets checks the real generator through the
// API: two resets must not hand out the same value, and both must satisfy the
// policy the login path enforces.
func TestGeneratedPasswordsAreUniqueAcrossResets(t *testing.T) {
	e := newAdminE2E(t)
	adminUser := e.admin(t, "an-admin-passphrase-1")
	cookies := e.login(t, "admin", adminUser.Account, "an-admin-passphrase-1")

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		teacher := e.staff(t, user.RoleTeacher, fmt.Sprintf("a-teacher-passphrase-%d", i))
		rec := e.adminCall(t, http.MethodPost, "/api/v1/admin/teachers/"+teacher.ID.String()+"/reset-password", "", cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("reset %d: status = %d (%s)", i, rec.Code, rec.Body.String())
		}
		generated, _ := jsonBody(t, rec)["password"].(string)
		if len(generated) != 32 {
			t.Fatalf("generated password %q has length %d, want 32", generated, len(generated))
		}
		if seen[generated] {
			t.Fatalf("the generator repeated a value: %q", generated)
		}
		seen[generated] = true
		if err := auth.NewPasswordPolicy(e.cfg.PasswordMinLength).Validate(teacher.Account, generated); err != nil {
			t.Fatalf("the generated password fails the policy: %v", err)
		}
	}
}
