package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// testNow is a fixed clock so session expiry and idle touching are asserted
// exactly, without a single time.Sleep.
var testNow = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type setHashCall struct {
	id   uuid.UUID
	hash string
}

type fakeUserRepo struct {
	users map[string]*user.User

	findErr    error
	createErr  error
	touchErr   error
	setHashErr error

	created      []user.CreateParams
	touchCalls   []uuid.UUID
	setHashCalls []setHashCall
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{users: make(map[string]*user.User)}
}

func (f *fakeUserRepo) add(u *user.User) {
	f.users[strings.ToLower(u.Account)] = u
}

func (f *fakeUserRepo) FindByAccount(_ context.Context, account string) (*user.User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if u, ok := f.users[strings.ToLower(account)]; ok {
		return u, nil
	}
	return nil, user.ErrNotFound
}

func (f *fakeUserRepo) FindByID(_ context.Context, id uuid.UUID) (*user.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, user.ErrNotFound
}

func (f *fakeUserRepo) Create(_ context.Context, params user.CreateParams) (*user.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, params)
	u := &user.User{
		ID:           uuid.New(),
		Account:      params.Account,
		DisplayName:  params.DisplayName,
		Role:         params.Role,
		Status:       user.StatusActive,
		PasswordHash: params.PasswordHash,
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	}
	f.add(u)
	return u, nil
}

func (f *fakeUserRepo) TouchLastLogin(_ context.Context, id uuid.UUID) error {
	f.touchCalls = append(f.touchCalls, id)
	return f.touchErr
}

func (f *fakeUserRepo) SetPasswordHash(_ context.Context, id uuid.UUID, hash string) error {
	f.setHashCalls = append(f.setHashCalls, setHashCall{id: id, hash: hash})
	return f.setHashErr
}

func (f *fakeUserRepo) List(_ context.Context, _ *user.Role) ([]user.User, error) {
	out := make([]user.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, *u)
	}
	return out, nil
}

// The four methods below complete user.Repository for Phase 2's admin API. The
// auth service never calls them; they exist so this fake keeps satisfying the
// interface — and they fail loudly rather than returning a zero value, so a
// future auth code path that started using one would be caught here instead of
// silently reading "no users, no admins".
func (f *fakeUserRepo) ListPage(context.Context, user.ListFilter) (*user.ListResult, error) {
	panic("fakeUserRepo.ListPage: the auth service must not page accounts")
}

func (f *fakeUserRepo) UpdateDisplayName(context.Context, uuid.UUID, string) error {
	panic("fakeUserRepo.UpdateDisplayName: the auth service must not rename accounts")
}

func (f *fakeUserRepo) SetStatus(context.Context, uuid.UUID, user.Status) error {
	panic("fakeUserRepo.SetStatus: the auth service must not change account status")
}

func (f *fakeUserRepo) CountActiveAdmins(context.Context) (int, error) {
	panic("fakeUserRepo.CountActiveAdmins: the auth service must not count admins")
}

type touchCall struct {
	id  uuid.UUID
	now time.Time
}

type fakeSessionStore struct {
	// stored is keyed by string(token hash).
	stored map[string]*SessionWithUser

	createErr error
	findErr   error
	revokeErr error
	touchErr  error
	deleteErr error

	created        []CreateSessionParams
	revoked        []uuid.UUID
	revokedAll     []uuid.UUID
	touched        []touchCall
	deletedExpired []uuid.UUID
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{stored: make(map[string]*SessionWithUser)}
}

func (f *fakeSessionStore) Create(_ context.Context, params CreateSessionParams) (*Session, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, params)

	session := &Session{
		ID:         params.ID,
		UserID:     params.UserID,
		TokenHash:  params.TokenHash,
		CSRFToken:  params.CSRFToken,
		IssuedAt:   params.IssuedAt,
		ExpiresAt:  params.ExpiresAt,
		LastSeenAt: params.IssuedAt,
		UserAgent:  params.UserAgent,
		IP:         params.IP,
	}
	// Compose the joined view the real store returns: revoke/expiry filtering is
	// the store's job, so the fake keeps the entry until it is revoked.
	entry := &SessionWithUser{Session: *session}
	if existing, ok := f.stored["__user__"+params.UserID.String()]; ok {
		entry.Account = existing.Account
		entry.DisplayName = existing.DisplayName
		entry.Role = existing.Role
		entry.UserStatus = existing.UserStatus
		entry.UserCreatedAt = existing.UserCreatedAt
		entry.UserLastLoginAt = existing.UserLastLoginAt
	}
	f.stored[string(params.TokenHash)] = entry
	return session, nil
}

// setUser registers the account facts a created session joins to.
func (f *fakeSessionStore) setUser(u *user.User) {
	f.stored["__user__"+u.ID.String()] = &SessionWithUser{
		Account:         u.Account,
		DisplayName:     u.DisplayName,
		Role:            u.Role,
		UserStatus:      u.Status,
		UserCreatedAt:   u.CreatedAt,
		UserLastLoginAt: u.LastLoginAt,
	}
}

// put stores a session directly, for tests that need an existing one.
func (f *fakeSessionStore) put(sw *SessionWithUser) {
	f.stored[string(sw.TokenHash)] = sw
}

func (f *fakeSessionStore) FindByTokenHash(_ context.Context, hash []byte) (*SessionWithUser, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if sw, ok := f.stored[string(hash)]; ok {
		return sw, nil
	}
	return nil, ErrSessionNotFound
}

func (f *fakeSessionStore) Revoke(_ context.Context, id uuid.UUID) error {
	f.revoked = append(f.revoked, id)
	if f.revokeErr != nil {
		return f.revokeErr
	}
	// Mirror the real store: a revoked session is no longer findable.
	for hash, sw := range f.stored {
		if sw.ID == id {
			revokedAt := testNow
			sw.RevokedAt = &revokedAt
			delete(f.stored, hash)
		}
	}
	return nil
}

func (f *fakeSessionStore) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	f.revokedAll = append(f.revokedAll, userID)
	for hash, sw := range f.stored {
		if sw.UserID == userID {
			delete(f.stored, hash)
		}
	}
	return nil
}

func (f *fakeSessionStore) Touch(_ context.Context, id uuid.UUID, now time.Time) error {
	f.touched = append(f.touched, touchCall{id: id, now: now})
	return f.touchErr
}

func (f *fakeSessionStore) DeleteExpired(_ context.Context, userID uuid.UUID) error {
	f.deletedExpired = append(f.deletedExpired, userID)
	return f.deleteErr
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestService(t *testing.T, users *fakeUserRepo, sessions *fakeSessionStore) *Service {
	t.Helper()
	svc := NewService(users, sessions, Config{
		SessionTTL:        time.Hour,
		IdleTouchInterval: 5 * time.Minute,
		PasswordPolicy:    NewPasswordPolicy(12),
	})
	svc.now = func() time.Time { return testNow }
	return svc
}

func hashOrFail(t *testing.T, password string) string {
	t.Helper()
	encoded, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}
	return encoded
}

func activeUser(t *testing.T, account string, role user.Role, password string) *user.User {
	t.Helper()
	u := &user.User{
		ID:          uuid.New(),
		Account:     account,
		DisplayName: "Display " + account,
		Role:        role,
		Status:      user.StatusActive,
		CreatedAt:   testNow.Add(-24 * time.Hour),
		UpdatedAt:   testNow.Add(-24 * time.Hour),
	}
	if role != user.RoleStudent {
		hash := hashOrFail(t, password)
		u.PasswordHash = &hash
	}
	return u
}

// ---------------------------------------------------------------------------
// Student login
// ---------------------------------------------------------------------------

func TestLoginStudentSuccess(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	student := activeUser(t, "S10086", user.RoleStudent, "")
	users.add(student)
	sessions.setUser(student)

	svc := newTestService(t, users, sessions)
	result, err := svc.LoginStudent(context.Background(), "s10086", LoginMeta{UserAgent: "test-agent"})
	if err != nil {
		t.Fatalf("LoginStudent() failed: %v", err)
	}

	// The lookup is case-insensitive because accounts are citext: the student
	// typed "s10086" and the stored account is "S10086".
	if result.Principal.UserID != student.ID {
		t.Errorf("principal user = %s, want %s", result.Principal.UserID, student.ID)
	}
	if result.Principal.Account != "S10086" {
		t.Errorf("principal account = %q, want the stored casing S10086", result.Principal.Account)
	}
	if result.Principal.Role != user.RoleStudent {
		t.Errorf("principal role = %q, want STUDENT", result.Principal.Role)
	}
	if result.CSRFToken == "" || result.CSRFToken != result.Principal.CSRFToken {
		t.Error("CSRF token missing or not mirrored into the principal")
	}
	if result.ExpiresAt.Sub(testNow) != time.Hour {
		t.Errorf("session expires in %v, want 1h", result.ExpiresAt.Sub(testNow))
	}

	if len(sessions.created) != 1 {
		t.Fatalf("created %d sessions, want 1", len(sessions.created))
	}
	created := sessions.created[0]
	// The database must never hold the raw token: only its SHA-256 (§41).
	if !bytes.Equal(created.TokenHash, HashToken(result.SessionToken)) {
		t.Error("stored hash is not SHA-256 of the returned raw token")
	}
	if bytes.Contains(created.TokenHash, []byte(result.SessionToken)) {
		t.Error("the raw token appears inside the stored hash")
	}
	if created.CSRFToken != result.CSRFToken {
		t.Error("the session row does not carry the CSRF token that was returned")
	}
	if created.UserID != student.ID {
		t.Error("session was created for the wrong user")
	}

	// last_login_at is bookkeeping and must be updated on a successful login.
	if len(users.touchCalls) != 1 || users.touchCalls[0] != student.ID {
		t.Errorf("TouchLastLogin calls = %v, want one for %s", users.touchCalls, student.ID)
	}
	if result.Principal.LastLoginAt == nil {
		t.Error("result lastLoginAt is nil after a successful login")
	}
	// Expired rows are cleaned opportunistically; Phase 1 has no background job.
	if len(sessions.deletedExpired) != 1 {
		t.Errorf("DeleteExpired calls = %v, want one", sessions.deletedExpired)
	}
}

func TestLoginStudentRejections(t *testing.T) {
	password := "teacher-password-1234"

	cases := []struct {
		name    string
		account string
		setup   func(*fakeUserRepo, *fakeSessionStore)
		wantErr error
	}{
		{
			name:    "unknown account",
			account: "S00000",
			setup:   func(*fakeUserRepo, *fakeSessionStore) {},
			wantErr: ErrInvalidCredentials,
		},
		{
			name:    "teacher account used on the student entry",
			account: "teacher001",
			setup: func(u *fakeUserRepo, s *fakeSessionStore) {
				teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
				u.add(teacher)
				s.setUser(teacher)
			},
			// Same error as an unknown account: naming the actual role would tell
			// an attacker which accounts exist and what they are.
			wantErr: ErrInvalidCredentials,
		},
		{
			name:    "disabled student",
			account: "S10086",
			setup: func(u *fakeUserRepo, s *fakeSessionStore) {
				student := activeUser(t, "S10086", user.RoleStudent, "")
				student.Status = user.StatusDisabled
				u.add(student)
				s.setUser(student)
			},
			wantErr: ErrAccountDisabled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users := newFakeUserRepo()
			sessions := newFakeSessionStore()
			tc.setup(users, sessions)
			svc := newTestService(t, users, sessions)

			result, err := svc.LoginStudent(context.Background(), tc.account, LoginMeta{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoginStudent() error = %v, want %v", err, tc.wantErr)
			}
			if result != nil {
				t.Error("LoginStudent() returned a result alongside an error")
			}
			if len(sessions.created) != 0 {
				t.Error("a session was created for a rejected login")
			}
		})
	}
}

func TestLoginStudentEmptyAccount(t *testing.T) {
	svc := newTestService(t, newFakeUserRepo(), newFakeSessionStore())
	if _, err := svc.LoginStudent(context.Background(), "   ", LoginMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("LoginStudent(\"   \") error = %v, want ErrInvalidCredentials", err)
	}
}

// ---------------------------------------------------------------------------
// Password login
// ---------------------------------------------------------------------------

func TestLoginWithPasswordSuccess(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	users.add(teacher)
	sessions.setUser(teacher)

	svc := newTestService(t, users, sessions)
	result, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "Teacher001", password, LoginMeta{})
	if err != nil {
		t.Fatalf("LoginWithPassword() failed: %v", err)
	}
	if result.Principal.Role != user.RoleTeacher {
		t.Errorf("role = %q, want TEACHER", result.Principal.Role)
	}
	if len(users.setHashCalls) != 0 {
		t.Error("a correct, current hash was rewritten (needsRehash must be false)")
	}
}

// TestLoginFailuresAreIndistinguishable is the anti-enumeration test: an unknown
// account and a wrong password must produce the SAME error value, not merely two
// errors that happen to share a status code.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	users.add(teacher)
	sessions.setUser(teacher)
	svc := newTestService(t, users, sessions)

	_, unknownErr := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "nosuchaccount", password, LoginMeta{})
	_, wrongPasswordErr := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", "definitely-not-it", LoginMeta{})

	if !errors.Is(unknownErr, ErrInvalidCredentials) {
		t.Fatalf("unknown account error = %v, want ErrInvalidCredentials", unknownErr)
	}
	if !errors.Is(wrongPasswordErr, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", wrongPasswordErr)
	}
	if unknownErr.Error() != wrongPasswordErr.Error() {
		t.Errorf("errors differ: %q vs %q — the difference is an account oracle", unknownErr, wrongPasswordErr)
	}
}

func TestLoginWithPasswordRoleMismatch(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	users.add(teacher)
	sessions.setUser(teacher)
	svc := newTestService(t, users, sessions)

	// A teacher account on the admin entry: correct password, wrong door.
	_, err := svc.LoginWithPassword(context.Background(), user.RoleAdmin, "teacher001", password, LoginMeta{})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials for a role mismatch", err)
	}

	// And a student account (no password at all) on a password entry.
	student := activeUser(t, "S10086", user.RoleStudent, "")
	users.add(student)
	_, err = svc.LoginWithPassword(context.Background(), user.RoleTeacher, "S10086", "whatever", LoginMeta{})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials for a student account", err)
	}
}

// TestLoginWithPasswordDisabledOrder documents the disclosure policy: a disabled
// account is only revealed to a caller who proved the password. Anyone else gets
// the generic error, so "disabled" is not an existence oracle.
func TestLoginWithPasswordDisabledOrder(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	teacher.Status = user.StatusDisabled
	users.add(teacher)
	sessions.setUser(teacher)
	svc := newTestService(t, users, sessions)

	if _, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", password, LoginMeta{}); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("correct password on a disabled account = %v, want ErrAccountDisabled", err)
	}
	if _, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", "wrong", LoginMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password on a disabled account = %v, want ErrInvalidCredentials", err)
	}
}

// staleHash derives a valid Argon2id hash with OLD parameters, which is what a
// row written before the current parameters were chosen actually looks like.
// Rewriting only the PHC header would produce a hash that cannot verify at all,
// which is a different (and much less interesting) case.
func staleHash(t *testing.T, password string) string {
	t.Helper()
	salt := make([]byte, argon2SaltLength)
	key := argon2.IDKey([]byte(password), salt, 1, 8*1024, 1, argon2KeyLength)
	return encodePHC(salt, key, 8*1024, 1, 1)
}

func TestLoginWithPasswordRehashesStaleHash(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	stale := staleHash(t, password)

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	teacher.PasswordHash = &stale
	users.add(teacher)
	sessions.setUser(teacher)

	svc := newTestService(t, users, sessions)
	if _, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", password, LoginMeta{}); err != nil {
		t.Fatalf("LoginWithPassword() failed: %v", err)
	}

	if len(users.setHashCalls) != 1 {
		t.Fatalf("SetPasswordHash calls = %d, want 1 (transparent rehash)", len(users.setHashCalls))
	}
	upgraded := users.setHashCalls[0].hash
	if upgraded == stale {
		t.Fatal("stored hash was not upgraded")
	}
	ok, needsRehash, err := Verify(upgraded, password)
	if err != nil || !ok {
		t.Fatalf("Verify(upgraded) = (%v, %v), want a valid hash", ok, err)
	}
	if needsRehash {
		t.Error("rehash did not reach the current parameters")
	}
}

func TestLoginWithPasswordIgnoresRehashFailure(t *testing.T) {
	const password = "a-teacher-passphrase-1"

	stale := staleHash(t, password)

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, password)
	teacher.PasswordHash = &stale
	users.add(teacher)
	sessions.setUser(teacher)
	users.setHashErr = errors.New("database is unhappy")

	svc := newTestService(t, users, sessions)
	// The credential was correct, so the login must succeed even though the
	// opportunistic upgrade failed.
	if _, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", password, LoginMeta{}); err != nil {
		t.Fatalf("LoginWithPassword() failed on a rehash error: %v", err)
	}
}

func TestLoginWithPasswordRejectsCorruptStoredHash(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	corrupt := "not-a-phc-string"
	teacher := activeUser(t, "teacher001", user.RoleTeacher, "whatever-password")
	teacher.PasswordHash = &corrupt
	users.add(teacher)
	sessions.setUser(teacher)

	svc := newTestService(t, users, sessions)
	_, err := svc.LoginWithPassword(context.Background(), user.RoleTeacher, "teacher001", "whatever-password", LoginMeta{})
	if err == nil {
		t.Fatal("LoginWithPassword() accepted a corrupt stored hash")
	}
	// It must NOT be reported as invalid credentials: every login for this account
	// is broken, and the operator has to see that instead of a "wrong password".
	if errors.Is(err, ErrInvalidCredentials) {
		t.Error("a corrupt stored hash was reported as invalid credentials")
	}
}

func TestLoginWithPasswordRejectsStudentRole(t *testing.T) {
	// Guard against a future caller wiring the student entry to the password
	// method: that would silently make student login require a password.
	svc := newTestService(t, newFakeUserRepo(), newFakeSessionStore())
	if _, err := svc.LoginWithPassword(context.Background(), user.RoleStudent, "S10086", "x", LoginMeta{}); err == nil {
		t.Fatal("LoginWithPassword accepted STUDENT")
	}
}

// ---------------------------------------------------------------------------
// Authenticate
// ---------------------------------------------------------------------------

// storedSession inserts a live session for the given raw token and returns it.
func storedSession(t *testing.T, sessions *fakeSessionStore, u *user.User, raw string, expiresAt time.Time) *SessionWithUser {
	t.Helper()
	sessions.setUser(u)
	sw := &SessionWithUser{
		Session: Session{
			ID:         uuid.New(),
			UserID:     u.ID,
			TokenHash:  HashToken(raw),
			CSRFToken:  "csrf-token-value",
			IssuedAt:   testNow.Add(-time.Minute),
			ExpiresAt:  expiresAt,
			LastSeenAt: testNow.Add(-time.Minute),
		},
		Account:         u.Account,
		DisplayName:     u.DisplayName,
		Role:            u.Role,
		UserStatus:      u.Status,
		UserCreatedAt:   u.CreatedAt,
		UserLastLoginAt: u.LastLoginAt,
	}
	sessions.put(sw)
	return sw
}

func TestAuthenticateSuccess(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, "a-teacher-passphrase-1")
	users.add(teacher)
	stored := storedSession(t, sessions, teacher, "raw-token", testNow.Add(time.Hour))

	svc := newTestService(t, users, sessions)
	principal, err := svc.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate() failed: %v", err)
	}
	if principal.SessionID != stored.ID {
		t.Errorf("session id = %s, want %s", principal.SessionID, stored.ID)
	}
	// Role and status come from the joined users row, not from the session.
	if principal.Role != user.RoleTeacher || principal.Status != user.StatusActive {
		t.Errorf("principal = %+v, want the database role and status", principal)
	}
	if principal.CSRFToken != "csrf-token-value" {
		t.Errorf("csrf token = %q, want the stored one", principal.CSRFToken)
	}
	// The last_seen_at write is throttled: this session was seen a minute ago.
	if len(sessions.touched) != 0 {
		t.Error("Touch was called for a session seen inside the throttle interval")
	}
}

func TestAuthenticateTouchesIdleSession(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, "a-teacher-passphrase-1")
	users.add(teacher)
	stored := storedSession(t, sessions, teacher, "raw-token", testNow.Add(time.Hour))
	stored.LastSeenAt = testNow.Add(-10 * time.Minute)

	svc := newTestService(t, users, sessions)
	if _, err := svc.Authenticate(context.Background(), "raw-token"); err != nil {
		t.Fatalf("Authenticate() failed: %v", err)
	}
	if len(sessions.touched) != 1 {
		t.Fatalf("Touch calls = %d, want 1 for a session idle for 10 minutes", len(sessions.touched))
	}
	if !sessions.touched[0].now.Equal(testNow) {
		t.Errorf("Touch time = %v, want %v", sessions.touched[0].now, testNow)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		wantErr  error
		wantRevo bool
	}{
		{"empty token", "", ErrSessionInvalid, false},
		{"whitespace token", "   ", ErrSessionInvalid, false},
		{"unknown token", "no-such-token", ErrSessionInvalid, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t, newFakeUserRepo(), newFakeSessionStore())
			_, err := svc.Authenticate(context.Background(), tc.token)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAuthenticateRejectsExpiredAndRevoked(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, "teacher001", user.RoleTeacher, "a-teacher-passphrase-1")
	users.add(teacher)

	// Expired: the store would normally filter it, but the service re-checks
	// because the store is an interface (defence in depth).
	storedSession(t, sessions, teacher, "expired-token", testNow.Add(-time.Second))
	// Revoked: same reasoning.
	sw := storedSession(t, sessions, teacher, "revoked-token", testNow.Add(time.Hour))
	revokedAt := testNow.Add(-time.Minute)
	sw.RevokedAt = &revokedAt

	svc := newTestService(t, users, sessions)
	for _, token := range []string{"expired-token", "revoked-token"} {
		if _, err := svc.Authenticate(context.Background(), token); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("Authenticate(%q) error = %v, want ErrSessionInvalid", token, err)
		}
	}
}

// TestAuthenticateRevokesSessionOfDisabledAccount is the "disabled means stopped,
// not paused" test: the session must be revoked, or re-enabling the account would
// resurrect a session created before it was switched off.
func TestAuthenticateRevokesSessionOfDisabledAccount(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	student := activeUser(t, "S10086", user.RoleStudent, "")
	student.Status = user.StatusDisabled
	users.add(student)
	stored := storedSession(t, sessions, student, "raw-token", testNow.Add(time.Hour))

	svc := newTestService(t, users, sessions)
	_, err := svc.Authenticate(context.Background(), "raw-token")
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("Authenticate() error = %v, want ErrAccountDisabled", err)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != stored.ID {
		t.Errorf("revoked = %v, want the disabled account's session %s", sessions.revoked, stored.ID)
	}
	// And a second call must no longer find it at all.
	if _, err := svc.Authenticate(context.Background(), "raw-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("second Authenticate() error = %v, want ErrSessionInvalid", err)
	}
}

func TestAuthenticatePropagatesStoreErrors(t *testing.T) {
	sessions := newFakeSessionStore()
	sessions.findErr = errors.New("connection reset")
	svc := newTestService(t, newFakeUserRepo(), sessions)

	_, err := svc.Authenticate(context.Background(), "raw-token")
	if err == nil || errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("Authenticate() error = %v, want the underlying store error (a 500, not a 401)", err)
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestLogoutRevokesAndIsIdempotent(t *testing.T) {
	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	student := activeUser(t, "S10086", user.RoleStudent, "")
	users.add(student)
	stored := storedSession(t, sessions, student, "raw-token", testNow.Add(time.Hour))

	svc := newTestService(t, users, sessions)
	if err := svc.Logout(context.Background(), "raw-token"); err != nil {
		t.Fatalf("Logout() failed: %v", err)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != stored.ID {
		t.Fatalf("revoked = %v, want %s", sessions.revoked, stored.ID)
	}

	// Second call, unknown token and empty token: all must succeed, because the
	// caller's goal ("this browser is logged out") is already true.
	for _, token := range []string{"raw-token", "never-existed", ""} {
		if err := svc.Logout(context.Background(), token); err != nil {
			t.Errorf("Logout(%q) = %v, want nil (idempotent)", token, err)
		}
	}
}

func TestLogoutPropagatesStoreErrors(t *testing.T) {
	sessions := newFakeSessionStore()
	student := activeUser(t, "S10086", user.RoleStudent, "")
	storedSession(t, sessions, student, "raw-token", testNow.Add(time.Hour))
	sessions.revokeErr = errors.New("connection reset")

	svc := newTestService(t, newFakeUserRepo(), sessions)
	if err := svc.Logout(context.Background(), "raw-token"); err == nil {
		t.Fatal("Logout() swallowed a store error; the client would believe it is logged out")
	}
}

// ---------------------------------------------------------------------------
// Logging discipline (§59)
// ---------------------------------------------------------------------------

// TestServiceLogsNeverContainSecrets runs the success and failure paths with a
// distinctive password, then asserts that nothing secret reached the log. It is
// the regression test for "someone logs the request body while debugging".
func TestServiceLogsNeverContainSecrets(t *testing.T) {
	const password = "s3cret-teacher-password-9f"
	const account = "teacher001"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logging.ContextWithLogger(context.Background(), logger)

	users := newFakeUserRepo()
	sessions := newFakeSessionStore()
	teacher := activeUser(t, account, user.RoleTeacher, password)
	users.add(teacher)
	sessions.setUser(teacher)
	svc := newTestService(t, users, sessions)

	result, err := svc.LoginWithPassword(ctx, user.RoleTeacher, account, password, LoginMeta{
		UserAgent: "go-test-agent",
		IP:        addrPtr(netip.MustParseAddr("203.0.113.9")),
	})
	if err != nil {
		t.Fatalf("LoginWithPassword() failed: %v", err)
	}
	// Failure paths too: they are the ones that tend to log the attempt verbatim.
	if _, err := svc.LoginWithPassword(ctx, user.RoleTeacher, account, password+"x", LoginMeta{}); err == nil {
		t.Fatal("expected a failed login")
	}
	if _, err := svc.LoginWithPassword(ctx, user.RoleTeacher, "unknown-account", password, LoginMeta{}); err == nil {
		t.Fatal("expected a failed login")
	}
	if _, err := svc.Authenticate(ctx, result.SessionToken); err != nil {
		t.Fatalf("Authenticate() failed: %v", err)
	}
	if err := svc.Logout(ctx, result.SessionToken); err != nil {
		t.Fatalf("Logout() failed: %v", err)
	}

	logged := buf.String()
	for _, secret := range []string{password, password + "x", result.SessionToken, result.CSRFToken} {
		if strings.Contains(logged, secret) {
			t.Fatalf("a secret value reached the log: %q\nlog:\n%s", secret, logged)
		}
	}
	// The user id and role are required by §59, so the log must not be empty
	// either — a silently disabled logger would make this test vacuous.
	if !strings.Contains(logged, teacher.ID.String()) {
		t.Errorf("logs do not carry user_id\nlog:\n%s", logged)
	}
	if !strings.Contains(logged, string(user.RoleTeacher)) {
		t.Errorf("logs do not carry role\nlog:\n%s", logged)
	}
}

func addrPtr(a netip.Addr) *netip.Addr { return &a }
