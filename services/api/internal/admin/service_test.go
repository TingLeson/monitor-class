package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/apperr"
	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// These tests run without PostgreSQL, Redis or a router. They exist to pin the
// RULES of §4/§68 — which role may be created, which field may be edited, what a
// disable does to a live session — because those rules must hold for every caller
// and not only for the HTTP path that happens to be tested first.

var testNow = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type statusCall struct {
	id     uuid.UUID
	status user.Status
}

type fakeRepo struct {
	byID      map[uuid.UUID]*user.User
	byAccount map[string]*user.User

	createErr       error
	findErr         error
	updateNameErr   error
	setStatusErr    error
	countAdminsErr  error
	listErr         error
	activeAdminHint int

	created        []user.CreateParams
	renamed        []uuid.UUID
	statusCalls    []statusCall
	lastStatusArg  user.Status
	lastFilter     user.ListFilter
	listResultStub *user.ListResult
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: make(map[uuid.UUID]*user.User), byAccount: make(map[string]*user.User)}
}

func (f *fakeRepo) add(u *user.User) *user.User {
	f.byID[u.ID] = u
	f.byAccount[strings.ToLower(u.Account)] = u
	return u
}

func (f *fakeRepo) FindByAccount(_ context.Context, account string) (*user.User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if u, ok := f.byAccount[strings.ToLower(account)]; ok {
		return u, nil
	}
	return nil, user.ErrNotFound
}

func (f *fakeRepo) FindByID(_ context.Context, id uuid.UUID) (*user.User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, user.ErrNotFound
}

func (f *fakeRepo) Create(_ context.Context, params user.CreateParams) (*user.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, taken := f.byAccount[strings.ToLower(params.Account)]; taken {
		return nil, user.ErrAccountTaken
	}
	f.created = append(f.created, params)
	u := f.add(&user.User{
		ID:           uuid.New(),
		Account:      params.Account,
		DisplayName:  params.DisplayName,
		Role:         params.Role,
		Status:       user.StatusActive,
		PasswordHash: params.PasswordHash,
		CreatedBy:    params.CreatedBy,
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	})
	return u, nil
}

func (f *fakeRepo) TouchLastLogin(context.Context, uuid.UUID) error { return nil }

func (f *fakeRepo) SetPasswordHash(_ context.Context, id uuid.UUID, hash string) error {
	u, ok := f.byID[id]
	if !ok {
		return user.ErrNotFound
	}
	u.PasswordHash = &hash
	u.UpdatedAt = u.UpdatedAt.Add(time.Millisecond)
	return nil
}

func (f *fakeRepo) List(_ context.Context, _ *user.Role) ([]user.User, error) {
	out := make([]user.User, 0, len(f.byID))
	for _, u := range f.byID {
		out = append(out, *u)
	}
	return out, nil
}

func (f *fakeRepo) ListPage(_ context.Context, filter user.ListFilter) (*user.ListResult, error) {
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResultStub != nil {
		return f.listResultStub, nil
	}
	out := make([]user.User, 0, len(f.byID))
	for _, u := range f.byID {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return &user.ListResult{Users: out, Total: len(f.byID)}, nil
}

func (f *fakeRepo) UpdateDisplayName(_ context.Context, id uuid.UUID, displayName string) error {
	if f.updateNameErr != nil {
		return f.updateNameErr
	}
	u, ok := f.byID[id]
	if !ok {
		return user.ErrNotFound
	}
	f.renamed = append(f.renamed, id)
	u.DisplayName = displayName
	u.UpdatedAt = u.UpdatedAt.Add(time.Millisecond)
	return nil
}

func (f *fakeRepo) SetStatus(_ context.Context, id uuid.UUID, status user.Status) error {
	if f.setStatusErr != nil {
		return f.setStatusErr
	}
	u, ok := f.byID[id]
	if !ok {
		return user.ErrNotFound
	}
	f.statusCalls = append(f.statusCalls, statusCall{id: id, status: status})
	f.lastStatusArg = status
	u.Status = status
	u.UpdatedAt = u.UpdatedAt.Add(time.Millisecond)
	return nil
}

func (f *fakeRepo) CountActiveAdmins(context.Context) (int, error) {
	if f.countAdminsErr != nil {
		return 0, f.countAdminsErr
	}
	if f.activeAdminHint > 0 {
		return f.activeAdminHint, nil
	}
	count := 0
	for _, u := range f.byID {
		if u.Role == user.RoleAdmin && u.Status == user.StatusActive {
			count++
		}
	}
	return count, nil
}

type fakeSessions struct {
	revoked []uuid.UUID
	err     error
}

func (f *fakeSessions) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	f.revoked = append(f.revoked, userID)
	return f.err
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	service  *Service
	repo     *fakeRepo
	sessions *fakeSessions
	logs     *bytes.Buffer
	// generated is what the injected generator returns, in order.
	generated []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := newFakeRepo()
	sessions := &fakeSessions{}
	logs := &bytes.Buffer{}
	// The service logs through logging.FromContext, which falls back to the slog
	// default. Installing a capturing default handler is what makes "the generated
	// password never reaches a log line" an assertion instead of a promise.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	service := NewService(repo, sessions, auth.NewPasswordPolicy(12))
	h := &harness{service: service, repo: repo, sessions: sessions, logs: logs}
	service.generatePassword = func() (string, error) {
		if len(h.generated) == 0 {
			return "", errors.New("test generator exhausted")
		}
		value := h.generated[0]
		h.generated = h.generated[1:]
		return value, nil
	}
	return h
}

// admin registers an active administrator and returns their id.
func (h *harness) admin(account string) *user.User {
	hash := "$argon2id$stub"
	return h.repo.add(&user.User{
		ID:           uuid.New(),
		Account:      account,
		DisplayName:  "Admin " + account,
		Role:         user.RoleAdmin,
		Status:       user.StatusActive,
		PasswordHash: &hash,
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	})
}

func (h *harness) teacher(account string) *user.User {
	hash := "$argon2id$stub"
	return h.repo.add(&user.User{
		ID:           uuid.New(),
		Account:      account,
		DisplayName:  "Teacher " + account,
		Role:         user.RoleTeacher,
		Status:       user.StatusActive,
		PasswordHash: &hash,
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	})
}

func (h *harness) student(account string) *user.User {
	return h.repo.add(&user.User{
		ID:          uuid.New(),
		Account:     account,
		DisplayName: "Student " + account,
		Role:        user.RoleStudent,
		Status:      user.StatusActive,
		CreatedAt:   testNow,
		UpdatedAt:   testNow,
	})
}

func strPtr(s string) *string { return &s }

// echoedSecret reports the longest run of at least five bytes that a message
// shares with a secret. Shorter fragments are ignored on purpose: a message like
// "password is too short" legitimately contains the word "short", and a test that
// cannot tell a rule from a value only teaches people to ignore it. Five or more
// bytes of an actual password appearing verbatim is never a coincidence.
func echoedSecret(message, secret string) string {
	const minRun = 5
	trimmed := strings.TrimSpace(secret)
	for length := len(trimmed); length >= minRun; length-- {
		for start := 0; start+length <= len(trimmed); start++ {
			if strings.Contains(message, trimmed[start:start+length]) {
				return trimmed[start : start+length]
			}
		}
	}
	return ""
}

// errorCode extracts the apperr code from an error, or "" when it is a sentinel.
func errorCode(t *testing.T, err error) apperr.Code {
	t.Helper()
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

// userMessage returns the sentence a client would actually receive.
//
// It is deliberately not err.Error(): for an *apperr.Error that renders the code
// and the internal cause as well, and only the Message field ever reaches a
// response body (§58).
func userMessage(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return appErr.Message
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// CreateUser
// ---------------------------------------------------------------------------

func TestCreateTeacherStoresArgon2idHashAndCreatedBy(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	created, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "teacher_01",
		DisplayName: "  李老师  ",
		Role:        user.RoleTeacher,
		Password:    strPtr("a-teacher-passphrase"),
		ActorID:     actor.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser() failed: %v", err)
	}
	if !created.HasPassword() {
		t.Fatal("the created teacher has no password hash")
	}
	if !strings.HasPrefix(*created.PasswordHash, "$argon2id$") {
		t.Errorf("stored hash = %q, want an Argon2id PHC string", *created.PasswordHash)
	}
	if strings.Contains(*created.PasswordHash, "a-teacher-passphrase") {
		t.Error("the stored hash contains the plaintext password")
	}
	if created.CreatedBy == nil || *created.CreatedBy != actor.ID {
		t.Errorf("created_by = %v, want the acting administrator %s", created.CreatedBy, actor.ID)
	}
	// The display name is stored trimmed: a trailing space renders as a different
	// name in the console while being invisible.
	if created.DisplayName != "李老师" {
		t.Errorf("display name = %q, want the trimmed value", created.DisplayName)
	}
	if created.Status != user.StatusActive {
		t.Errorf("status = %q, want ACTIVE", created.Status)
	}
}

func TestCreateStudentHasNoPassword(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	created, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "S20001",
		DisplayName: "张三",
		Role:        user.RoleStudent,
		ActorID:     actor.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser() failed: %v", err)
	}
	if created.HasPassword() || created.PasswordHash != nil {
		t.Error("a student account was created with a password hash (§2.2)")
	}
}

func TestCreateAdminIsRefused(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	// §4: the admin API creates teachers and students. Administrator accounts come
	// from the adminctl break-glass CLI, so no amount of API access can mint one.
	_, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "admin_2",
		DisplayName: "Another admin",
		Role:        user.RoleAdmin,
		Password:    strPtr("an-admin-passphrase"),
		ActorID:     actor.ID,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if code := errorCode(t, err); code != apperr.CodeInvalidRequest {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	if !strings.Contains(err.Error(), "adminctl") {
		t.Errorf("message %q does not explain that administrators come from adminctl", err.Error())
	}
	if len(h.repo.created) != 0 {
		t.Error("an ADMIN account reached the repository")
	}
}

func TestCreateStudentWithPasswordIsRefused(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	_, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "S20002",
		DisplayName: "李四",
		Role:        user.RoleStudent,
		Password:    strPtr("a-student-passphrase"),
		ActorID:     actor.ID,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "must not have a password") {
		t.Errorf("message = %q, want an explanation of §2.2", err.Error())
	}
	if len(h.repo.created) != 0 {
		t.Error("the student was created despite the password")
	}
}

func TestCreateTeacherWithoutPasswordIsRefused(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	_, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "teacher_02",
		DisplayName: "王老师",
		Role:        user.RoleTeacher,
		ActorID:     actor.ID,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if code := errorCode(t, err); code != apperr.CodeInvalidRequest {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
}

func TestCreateUserValidation(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	cases := []struct {
		name    string
		in      CreateUserInput
		wantSub string
	}{
		{
			name:    "account too short",
			in:      CreateUserInput{Account: "ab", DisplayName: "Ok", Role: user.RoleStudent},
			wantSub: "3-64",
		},
		{
			name:    "account with a space",
			in:      CreateUserInput{Account: "teach er", DisplayName: "Ok", Role: user.RoleStudent},
			wantSub: "3-64",
		},
		{
			name:    "account with an at sign",
			in:      CreateUserInput{Account: "teach@example.com", DisplayName: "Ok", Role: user.RoleStudent},
			wantSub: "3-64",
		},
		{
			name:    "blank display name",
			in:      CreateUserInput{Account: "valid_account", DisplayName: "   ", Role: user.RoleStudent},
			wantSub: "display name",
		},
		{
			name:    "display name too long",
			in:      CreateUserInput{Account: "valid_account", DisplayName: strings.Repeat("名", user.MaxDisplayNameLength+1), Role: user.RoleStudent},
			wantSub: "too long",
		},
		{
			name:    "unknown role",
			in:      CreateUserInput{Account: "valid_account", DisplayName: "Ok", Role: user.Role("PRINCIPAL")},
			wantSub: "TEACHER or STUDENT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.ActorID = actor.ID
			_, err := h.service.CreateUser(context.Background(), tc.in)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("message = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
			if code := errorCode(t, err); code != apperr.CodeInvalidRequest {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}
		})
	}
	if len(h.repo.created) != 0 {
		t.Errorf("%d invalid requests reached the repository", len(h.repo.created))
	}
}

func TestCreateUserPasswordPolicy(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	cases := []struct {
		name     string
		account  string
		password string
	}{
		// A distinctive value, so "the message quotes the password" can be told
		// apart from "the message names the rule".
		{"too short", "teacher_03", "Sh0rt!"},
		{"blank", "teacher_04", "            "},
		// A 12-character account is used so the "not the account name" rule is
		// reached: a shorter account would be rejected for its length first, and the
		// test would silently assert the wrong rule.
		{"equals the account", "teacher_0005", "teacher_0005"},
		{"equals the account in another case", "teacher_0007", "TEACHER_0007"},
		{"too long", "teacher_06", strings.Repeat("x", auth.MaxPasswordLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.service.CreateUser(context.Background(), CreateUserInput{
				Account:     tc.account,
				DisplayName: "老师",
				Role:        user.RoleTeacher,
				Password:    strPtr(tc.password),
				ActorID:     actor.ID,
			})
			if code := errorCode(t, err); code != apperr.CodePasswordPolicyViolation {
				t.Fatalf("code = %q (%v), want PASSWORD_POLICY_VIOLATION", code, err)
			}
			// The message must be actionable for a form field, and must never echo
			// the password (§59). "short" appears in the message only as part of
			// "too short" — the rule, never the value — so the assertion looks for
			// anything that is uniquely the password.
			message := userMessage(t, err)
			if echoed := echoedSecret(message, tc.password); echoed != "" {
				t.Errorf("the policy error echoes the password (%q): %q", echoed, message)
			}
			// The sentence is shown next to a password input, so it must explain the
			// rule in plain terms — no internal package names, no code prefixes.
			for _, internal := range []string{"auth:", "admin:", "PASSWORD_POLICY_VIOLATION"} {
				if strings.Contains(message, internal) {
					t.Errorf("the policy message leaks an internal token %q: %q", internal, message)
				}
			}
		})
	}
	if len(h.repo.created) != 0 {
		t.Errorf("%d rejected passwords produced an account", len(h.repo.created))
	}
}

func TestCreateDuplicateAccountIsAPlainConflict(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	h.student("S30001")

	_, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "s30001", // citext: the same account in another case
		DisplayName: "张三",
		Role:        user.RoleStudent,
		ActorID:     actor.ID,
	})
	if !errors.Is(err, ErrAccountTaken) {
		t.Fatalf("error = %v, want ErrAccountTaken", err)
	}
	if code := errorCode(t, err); code != "" {
		t.Errorf("code = %q; the transport mapping belongs to the HTTP layer", code)
	}
}

// ---------------------------------------------------------------------------
// List / Get
// ---------------------------------------------------------------------------

func TestListUsersRejectsOutOfRangePagination(t *testing.T) {
	h := newHarness(t)

	for _, filter := range []user.ListFilter{
		{Limit: 0},
		{Limit: MaxListPageSize + 1},
		{Limit: 10, Offset: -1},
	} {
		if _, err := h.service.ListUsers(context.Background(), filter); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("filter %+v: error = %v, want ErrInvalidRequest", filter, err)
		}
	}
}

func TestListUsersPassesTheFilterThrough(t *testing.T) {
	h := newHarness(t)
	role := user.RoleTeacher
	status := user.StatusActive
	filter := user.ListFilter{Role: &role, Status: &status, Query: "li", Limit: 10, Offset: 20}

	if _, err := h.service.ListUsers(context.Background(), filter); err != nil {
		t.Fatalf("ListUsers() failed: %v", err)
	}
	if got := h.repo.lastFilter; got != filter {
		t.Errorf("repository filter = %+v, want %+v", got, filter)
	}
}

func TestGetUserMapsNotFound(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.GetUser(context.Background(), uuid.New()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("error = %v, want ErrUserNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// UpdateDisplayName
// ---------------------------------------------------------------------------

func TestUpdateDisplayNameOnlyTouchesTheName(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.student("S40001")

	updated, err := h.service.UpdateDisplayName(context.Background(), UserUpdateInput{
		TargetID:    target.ID,
		DisplayName: "  李四  ",
		ActorID:     actor.ID,
	})
	if err != nil {
		t.Fatalf("UpdateDisplayName() failed: %v", err)
	}
	if updated.DisplayName != "李四" {
		t.Errorf("display name = %q, want the trimmed value", updated.DisplayName)
	}
	if updated.Role != user.RoleStudent || updated.Status != user.StatusActive {
		t.Errorf("role/status changed: %s/%s", updated.Role, updated.Status)
	}
	if !updated.UpdatedAt.After(target.CreatedAt) {
		t.Error("updated_at did not move forward")
	}
	if len(h.repo.statusCalls) != 0 {
		t.Error("a rename wrote the status")
	}
}

func TestUpdateDisplayNameRejectsBlankName(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.student("S40002")

	if _, err := h.service.UpdateDisplayName(context.Background(), UserUpdateInput{
		TargetID:    target.ID,
		DisplayName: "   ",
		ActorID:     actor.ID,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if len(h.repo.renamed) != 0 {
		t.Error("a blank name reached the repository")
	}
}

// ---------------------------------------------------------------------------
// SetStatus
// ---------------------------------------------------------------------------

func TestDisableRevokesEverySessionImmediately(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.teacher("teacher_10")

	updated, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: target.ID,
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	})
	if err != nil {
		t.Fatalf("SetStatus() failed: %v", err)
	}
	if updated.Status != user.StatusDisabled {
		t.Errorf("status = %q, want DISABLED", updated.Status)
	}
	// The point of the revoke: "disabled" must mean the person in the middle of a
	// lesson is out on their next request, not after SESSION_TTL.
	if len(h.sessions.revoked) != 1 || h.sessions.revoked[0] != target.ID {
		t.Fatalf("revoked = %v, want exactly [%s]", h.sessions.revoked, target.ID)
	}
	if !strings.Contains(h.logs.String(), "user.status_changed") {
		t.Error("no structured audit line for the status change")
	}
	if !strings.Contains(h.logs.String(), actor.ID.String()) ||
		!strings.Contains(h.logs.String(), target.ID.String()) {
		t.Error("the audit line is missing actor_user_id or target_user_id")
	}
}

func TestEnableDoesNotResurrectSessions(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.teacher("teacher_11")
	target.Status = user.StatusDisabled

	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: target.ID,
		Status:   user.StatusActive,
		ActorID:  actor.ID,
	}); err != nil {
		t.Fatalf("SetStatus() failed: %v", err)
	}
	if len(h.sessions.revoked) != 0 {
		t.Error("re-enabling touched sessions; old sessions must stay dead and the owner logs in again")
	}
}

func TestSetStatusIsIdempotent(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.teacher("teacher_12")
	target.Status = user.StatusDisabled

	updated, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: target.ID,
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	})
	if err != nil {
		t.Fatalf("setting the current status must succeed: %v", err)
	}
	if updated.Status != user.StatusDisabled {
		t.Errorf("status = %q, want DISABLED", updated.Status)
	}
	if len(h.repo.statusCalls) != 0 {
		t.Error("an idempotent request still wrote the row")
	}
}

func TestDisablingYourselfIsRefused(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	h.admin("admin-2")

	_, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: actor.ID,
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	})
	if !errors.Is(err, ErrCannotDisableSelf) {
		t.Fatalf("error = %v, want ErrCannotDisableSelf", err)
	}
	// The sentinel is internal vocabulary; the *user-facing* sentence is the
	// CANNOT_DISABLE_SELF message attached by the HTTP layer (asserted in
	// httpapi/admin_test.go). What matters here is that the rule is named
	// unambiguously instead of collapsing into a generic invalid request.
	if !strings.Contains(err.Error(), "disable itself") {
		t.Errorf("message = %q, want the rule to be named", err.Error())
	}
	if len(h.repo.statusCalls) != 0 {
		t.Error("the self-disable reached the repository")
	}
}

func TestDisablingTheLastActiveAdminIsRefused(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	lastAdmin := h.admin("admin-2")
	// admin-1 is already disabled, so admin-2 is the only one left.
	actor.Status = user.StatusDisabled

	_, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: lastAdmin.ID,
		Status:   user.StatusDisabled,
		ActorID:  uuid.New(), // a third admin, so the "not yourself" rule does not fire
	})
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("error = %v, want ErrLastAdmin", err)
	}
	if len(h.repo.statusCalls) != 0 {
		t.Error("the last administrator was disabled")
	}
}

func TestSetStatusRejectsUnknownStatus(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.teacher("teacher_13")

	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: target.ID,
		Status:   user.Status("PAUSED"),
		ActorID:  actor.ID,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

func TestSetStatusMapsRepositoryLastAdminGuard(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.teacher("teacher_14")
	// The read-then-write check passes (the target is a teacher), but the SQL
	// guard refuses: this is the concurrent path, and the service must translate
	// it rather than answering 500.
	h.repo.setStatusErr = user.ErrLastAdmin

	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: target.ID,
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("error = %v, want ErrLastAdmin", err)
	}
}

func TestSetStatusUnknownTargetIsNotFound(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: uuid.New(),
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error = %v, want ErrUserNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// ResetTeacherPassword
// ---------------------------------------------------------------------------

func TestResetPasswordWithSuppliedPassword(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_20")

	result, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID,
		Password:  strPtr("a-brand-new-passphrase"),
		ActorID:   actor.ID,
	})
	if err != nil {
		t.Fatalf("ResetTeacherPassword() failed: %v", err)
	}
	if result.GeneratedPassword != "" {
		t.Error("a caller-supplied password must not be echoed back in the response")
	}
	if result.User.PasswordHash == nil || !strings.HasPrefix(*result.User.PasswordHash, "$argon2id$") {
		t.Fatalf("stored hash = %v, want an Argon2id PHC string", result.User.PasswordHash)
	}
	ok, _, err := auth.Verify(*result.User.PasswordHash, "a-brand-new-passphrase")
	if err != nil || !ok {
		t.Fatalf("Verify() = %v, %v; the new password does not match the stored hash", ok, err)
	}
	// Phase 1's rule: a password reset forces a fresh login everywhere.
	if len(h.sessions.revoked) != 1 || h.sessions.revoked[0] != teacher.ID {
		t.Fatalf("revoked = %v, want exactly [%s]", h.sessions.revoked, teacher.ID)
	}
}

func TestResetPasswordGeneratesAndReturnsAValueOnce(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_21")
	h.generated = []string{"generated-password-value-for-test"}

	result, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID,
		ActorID:   actor.ID,
	})
	if err != nil {
		t.Fatalf("ResetTeacherPassword() failed: %v", err)
	}
	if result.GeneratedPassword != "generated-password-value-for-test" {
		t.Fatalf("generated = %q", result.GeneratedPassword)
	}
	ok, _, err := auth.Verify(*result.User.PasswordHash, result.GeneratedPassword)
	if err != nil || !ok {
		t.Fatalf("the generated password does not verify against the stored hash: %v %v", ok, err)
	}
	// The value exists in the response and nowhere else: not in a log line
	// (§59), and not recoverable from the user row.
	if strings.Contains(h.logs.String(), result.GeneratedPassword) {
		t.Fatal("the generated password reached the log")
	}
	if strings.Contains(*result.User.PasswordHash, result.GeneratedPassword) {
		t.Fatal("the password was stored in plaintext")
	}
	if !strings.Contains(h.logs.String(), "user.password_reset") {
		t.Error("no audit line for the reset")
	}
	if !strings.Contains(h.logs.String(), teacher.ID.String()) {
		t.Error("the audit line does not name the target account")
	}
}

func TestGeneratedPasswordLengthAndUniqueness(t *testing.T) {
	// This test exercises the real generator, not the injected one.
	const draws = 200
	seen := make(map[string]struct{}, draws)
	for i := 0; i < draws; i++ {
		password, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword() failed: %v", err)
		}
		// 24 bytes of base64url without padding: 32 characters, 192 bits.
		if len(password) != 32 {
			t.Fatalf("generated password length = %d (%q), want 32", len(password), password)
		}
		if base64.RawURLEncoding.DecodedLen(len(password)) != generatedPasswordBytes {
			t.Fatalf("generated password %q does not decode to %d bytes", password, generatedPasswordBytes)
		}
		if _, duplicate := seen[password]; duplicate {
			t.Fatalf("the generator repeated a value after %d draws: %q", i, password)
		}
		seen[password] = struct{}{}
		if err := auth.NewPasswordPolicy(12).Validate("teacher_x", password); err != nil {
			t.Fatalf("generated password rejected by the policy: %v", err)
		}
	}
}

func TestGeneratedPasswordIsNotDerivableFromTheStoredHash(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_22")
	h.generated = []string{"another-generated-value-here"}

	result, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID,
		ActorID:   actor.ID,
	})
	if err != nil {
		t.Fatalf("ResetTeacherPassword() failed: %v", err)
	}
	digest := sha256.Sum256([]byte(result.GeneratedPassword))
	hexDigest := hex.EncodeToString(digest[:])
	// The hash is Argon2id over a random salt; a SHA-256 of the password must not
	// appear in it (it would mean the value was stored in a recoverable form).
	if strings.Contains(*result.User.PasswordHash, hexDigest) {
		t.Error("the stored hash contains a digest of the password")
	}
}

func TestResetPasswordRejectsNonTeacherTargets(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	cases := []struct {
		name   string
		target *user.User
	}{
		{"a student", h.student("S50001")},
		{"another administrator", h.admin("admin-9")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
				TeacherID: tc.target.ID,
				ActorID:   actor.ID,
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
			// The message must point at the supported path for admin accounts.
			if !strings.Contains(err.Error(), "adminctl") {
				t.Errorf("message = %q, want a pointer to adminctl", err.Error())
			}
		})
	}
	if len(h.sessions.revoked) != 0 {
		t.Error("a rejected reset still revoked sessions")
	}
}

func TestResetPasswordUnknownTargetIsNotFound(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")

	if _, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: uuid.New(),
		ActorID:   actor.ID,
	}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error = %v, want ErrUserNotFound", err)
	}
}

func TestResetPasswordValidatesASuppliedPassword(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_23")

	_, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID,
		Password:  strPtr("short"),
		ActorID:   actor.ID,
	})
	if code := errorCode(t, err); code != apperr.CodePasswordPolicyViolation {
		t.Fatalf("code = %q (%v), want PASSWORD_POLICY_VIOLATION", code, err)
	}
	if len(h.sessions.revoked) != 0 {
		t.Error("a rejected password still revoked sessions")
	}
}

// ---------------------------------------------------------------------------
// No password material in logs
// ---------------------------------------------------------------------------

// TestNoCredentialReachesTheLog walks every mutating use case and asserts that the
// log lines it produces contain identifiers only (§59). The password material
// includes the plaintext the caller supplied, the server-generated value and the
// Argon2id hash that ends up in the database.
func TestNoCredentialReachesTheLog(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	const supplied = "supplied-passphrase-9f3a"
	h.generated = []string{"server-generated-7c1d"}

	teacher, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "teacher_30",
		DisplayName: "赵老师",
		Role:        user.RoleTeacher,
		Password:    strPtr(supplied),
		ActorID:     actor.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser() failed: %v", err)
	}
	teacherHash := *teacher.PasswordHash

	student, err := h.service.CreateUser(context.Background(), CreateUserInput{
		Account:     "S60001",
		DisplayName: "钱五",
		Role:        user.RoleStudent,
		ActorID:     actor.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser(student) failed: %v", err)
	}

	if _, err := h.service.UpdateDisplayName(context.Background(), UserUpdateInput{
		TargetID: student.ID, DisplayName: "钱五同学", ActorID: actor.ID,
	}); err != nil {
		t.Fatalf("UpdateDisplayName() failed: %v", err)
	}
	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: teacher.ID, Status: user.StatusDisabled, ActorID: actor.ID,
	}); err != nil {
		t.Fatalf("SetStatus() failed: %v", err)
	}
	reset, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID, ActorID: actor.ID,
	})
	if err != nil {
		t.Fatalf("ResetTeacherPassword() failed: %v", err)
	}

	logged := h.logs.String()
	if logged == "" {
		t.Fatal("the service produced no log lines at all; the audit trail is missing")
	}
	for _, secret := range []string{supplied, reset.GeneratedPassword, teacherHash} {
		if secret == "" {
			continue
		}
		if strings.Contains(logged, secret) {
			t.Fatalf("password material reached the log: %q", secret)
		}
	}
	// The audit trail must be usable: every mutating action names the actor and
	// the target.
	for _, want := range []string{
		"action=user.created",
		"action=user.display_name_updated",
		"action=user.status_changed",
		"action=user.password_reset",
		"actor_user_id=" + actor.ID.String(),
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log is missing %q", want)
		}
	}
}

// TestReservedFieldsAreRefusedByConstruction documents, at the service level, the
// rule the HTTP layer enforces for the PATCH body: a rename cannot carry a role,
// a status or a password, because there is no field to put them in and no code
// path that would read one.
func TestRenameCannotChangeRoleOrStatus(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	target := h.student("S60002")
	originalRole, originalStatus := target.Role, target.Status

	if _, err := h.service.UpdateDisplayName(context.Background(), UserUpdateInput{
		TargetID:    target.ID,
		DisplayName: "新名字",
		ActorID:     actor.ID,
	}); err != nil {
		t.Fatalf("UpdateDisplayName() failed: %v", err)
	}
	stored, err := h.repo.FindByID(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if stored.Role != originalRole || stored.Status != originalStatus {
		t.Errorf("role/status = %s/%s, want %s/%s", stored.Role, stored.Status, originalRole, originalStatus)
	}
}

// TestRepositoryErrorsAreNotSwallowed keeps the service honest about failures it
// cannot classify: an infrastructure error must travel out unchanged so the HTTP
// layer can answer INTERNAL, not "account not found".
func TestRepositoryErrorsPropagate(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("connection refused")
	h.repo.findErr = boom

	if _, err := h.service.GetUser(context.Background(), uuid.New()); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the original repository error", err)
	}
	if errors.Is(h.repo.findErr, ErrUserNotFound) {
		t.Error("an infrastructure error was classified as not-found")
	}
}

// TestRevokeFailureFailsTheReset makes the session revoke a hard requirement of a
// reset: a reset whose sessions survive is not a reset.
func TestRevokeFailureFailsTheReset(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_31")
	h.sessions.err = errors.New("sessions table unavailable")

	if _, err := h.service.ResetTeacherPassword(context.Background(), ResetPasswordInput{
		TeacherID: teacher.ID,
		ActorID:   actor.ID,
	}); err == nil {
		t.Fatal("ResetTeacherPassword() succeeded although sessions could not be revoked")
	}
	// The password must NOT have been written: failing before the write is what
	// keeps the credential and the sessions consistent.
	stored, _ := h.repo.FindByID(context.Background(), teacher.ID)
	if stored.UpdatedAt != testNow {
		t.Error("the password was written even though the session revoke failed")
	}
}

// TestDisableKeepsGoingWhenRevokeFails pins the documented ordering: the status is
// durable before the revoke is attempted, so a failing revoke is reported (the
// call fails) but the account is already DISABLED. Losing the status change would
// be worse: the admin would retry and the account would keep working.
func TestDisableKeepsGoingWhenRevokeFails(t *testing.T) {
	h := newHarness(t)
	actor := h.admin("admin-1")
	teacher := h.teacher("teacher_32")
	h.sessions.err = errors.New("sessions table unavailable")

	if _, err := h.service.SetStatus(context.Background(), SetStatusInput{
		TargetID: teacher.ID,
		Status:   user.StatusDisabled,
		ActorID:  actor.ID,
	}); err == nil {
		t.Fatal("SetStatus() hid a failed session revoke")
	}
	stored, _ := h.repo.FindByID(context.Background(), teacher.ID)
	if stored.Status != user.StatusDisabled {
		t.Errorf("status = %q, want DISABLED: the status change must be durable before the revoke", stored.Status)
	}
}

func TestActorRefUsesNullForAnUnknownActor(t *testing.T) {
	if got := actorRef(uuid.Nil); got != nil {
		t.Errorf("actorRef(zero uuid) = %v, want nil: created_by must not point at a non-existent user", got)
	}
	id := uuid.New()
	if got := actorRef(id); got == nil || *got != id {
		t.Errorf("actorRef(%s) = %v, want the same id", id, got)
	}
}
