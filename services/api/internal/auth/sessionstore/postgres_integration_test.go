package sessionstore_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Session storage against a real PostgreSQL server: the properties under test are
// the ones the SQL is responsible for (revoked/expired rows never resolve, the
// JOIN brings back the CURRENT account state), not the service policy above it.

type fixture struct {
	store *sessionstore.Store
	users *user.Postgres
	pool  *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	return &fixture{
		store: sessionstore.New(pool),
		users: user.NewPostgres(pool),
		pool:  pool,
	}
}

// newStudent creates an account that the sessions belong to and removes it (and
// its sessions, by cascade) afterwards.
func (f *fixture) newStudent(t *testing.T) *user.User {
	t.Helper()
	created, err := f.users.Create(context.Background(), user.CreateParams{
		Account:     dbtest.RandomAccount("sess_stu"),
		DisplayName: "学生 张三",
		Role:        user.RoleStudent,
	})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID); err != nil {
			t.Logf("cleanup: delete user %s: %v", created.ID, err)
		}
	})
	return created
}

// newSession issues a session row and returns the raw token plus the parameters.
func (f *fixture) newSession(t *testing.T, u *user.User, ttl time.Duration) (string, auth.CreateSessionParams) {
	t.Helper()
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken(): %v", err)
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	ip := netip.MustParseAddr("203.0.113.9")
	params := auth.CreateSessionParams{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: hash,
		CSRFToken: csrf,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
		UserAgent: "go-test-agent/1.0",
		IP:        &ip,
	}
	created, err := f.store.Create(context.Background(), params)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	if created.ID != params.ID {
		t.Errorf("Create() returned id %s, want %s", created.ID, params.ID)
	}
	return raw, params
}

func TestCreateAndFindByTokenHash(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	raw, params := f.newSession(t, student, time.Hour)

	found, err := f.store.FindByTokenHash(ctx, auth.HashToken(raw))
	if err != nil {
		t.Fatalf("FindByTokenHash() failed: %v", err)
	}

	// The session row itself.
	if found.ID != params.ID || found.UserID != student.ID {
		t.Errorf("found session %s/%s, want %s/%s", found.ID, found.UserID, params.ID, student.ID)
	}
	if found.CSRFToken != params.CSRFToken {
		t.Error("the CSRF token did not round-trip")
	}
	if found.RevokedAt != nil {
		t.Error("a fresh session is already revoked")
	}
	if !found.ExpiresAt.After(found.IssuedAt) {
		t.Error("expires_at is not after issued_at")
	}
	if found.UserAgent != params.UserAgent {
		t.Errorf("user_agent = %q, want %q", found.UserAgent, params.UserAgent)
	}
	if found.IP == nil || found.IP.String() != "203.0.113.9" {
		t.Errorf("ip = %v, want 203.0.113.9", found.IP)
	}

	// The joined account state: this is what makes role/status checks current on
	// every request instead of frozen at login (§37).
	if found.Account != student.Account || found.DisplayName != student.DisplayName {
		t.Errorf("joined account = %q/%q, want %q/%q", found.Account, found.DisplayName, student.Account, student.DisplayName)
	}
	if found.Role != user.RoleStudent || found.UserStatus != user.StatusActive {
		t.Errorf("joined role/status = %s/%s, want STUDENT/ACTIVE", found.Role, found.UserStatus)
	}
	if found.UserCreatedAt.IsZero() {
		t.Error("joined created_at is zero")
	}
}

// TestFindByTokenHashReflectsCurrentAccountState is the "disable takes effect
// immediately" test at the storage layer: the same session row must report the
// new status without anything having touched the session.
func TestFindByTokenHashReflectsCurrentAccountState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	raw, _ := f.newSession(t, student, time.Hour)

	if _, err := f.pool.Exec(ctx, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, student.ID); err != nil {
		t.Fatalf("disable student: %v", err)
	}

	found, err := f.store.FindByTokenHash(ctx, auth.HashToken(raw))
	if err != nil {
		t.Fatalf("FindByTokenHash() failed: %v", err)
	}
	// The row must still be returned — with the disabled status — so the service
	// can answer ACCOUNT_DISABLED (403) rather than a generic 401.
	if found.UserStatus != user.StatusDisabled {
		t.Errorf("user status = %q, want DISABLED", found.UserStatus)
	}

	// A role change must be equally visible: this is what stops a teacher demoted
	// to student from keeping teacher access until the session expires.
	if _, err := f.pool.Exec(ctx, `UPDATE users SET role = 'TEACHER', password_hash = 'x' WHERE id = $1`, student.ID); err != nil {
		t.Fatalf("promote student: %v", err)
	}
	found, err = f.store.FindByTokenHash(ctx, auth.HashToken(raw))
	if err != nil {
		t.Fatalf("FindByTokenHash() failed: %v", err)
	}
	if found.Role != user.RoleTeacher {
		t.Errorf("role = %q, want the updated TEACHER", found.Role)
	}
}

func TestRevokedSessionIsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	raw, params := f.newSession(t, student, time.Hour)

	if err := f.store.Revoke(ctx, params.ID); err != nil {
		t.Fatalf("Revoke() failed: %v", err)
	}
	// "Logged out" must mean the token cannot authenticate again, even though the
	// row is kept for audit.
	if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("FindByTokenHash() error = %v, want ErrSessionNotFound after revoke", err)
	}
	// Revoking twice is a no-op, not an error: logout is idempotent.
	if err := f.store.Revoke(ctx, params.ID); err != nil {
		t.Errorf("second Revoke() = %v, want nil", err)
	}

	// The audit row survives with revoked_at set.
	var revokedAt *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE id = $1`, params.ID).Scan(&revokedAt); err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if revokedAt == nil {
		t.Error("revoked_at is NULL: the session was hard-deleted instead of revoked")
	}
}

func TestExpiredSessionIsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	raw, params := f.newSession(t, student, time.Hour)

	// Age the session beyond its expiry. issued_at must stay before expires_at
	// (the table asserts it), so both move into the past.
	if _, err := f.pool.Exec(ctx, `
		UPDATE sessions SET issued_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		 WHERE id = $1`, params.ID); err != nil {
		t.Fatalf("age session: %v", err)
	}

	if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("FindByTokenHash() error = %v, want ErrSessionNotFound for an expired session", err)
	}
}

func TestRevokeAllForUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	firstRaw, _ := f.newSession(t, student, time.Hour)
	secondRaw, _ := f.newSession(t, student, time.Hour)

	if err := f.store.RevokeAllForUser(ctx, student.ID); err != nil {
		t.Fatalf("RevokeAllForUser() failed: %v", err)
	}
	for name, raw := range map[string]string{"first": firstRaw, "second": secondRaw} {
		if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("%s session: error = %v, want ErrSessionNotFound", name, err)
		}
	}

	// A session of a DIFFERENT account must not be touched.
	other := f.newStudent(t)
	otherRaw, _ := f.newSession(t, other, time.Hour)
	if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(otherRaw)); err != nil {
		t.Errorf("another account's session was revoked as well: %v", err)
	}
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	_, params := f.newSession(t, student, time.Hour)

	touchedAt := params.IssuedAt.Add(10 * time.Minute)
	if err := f.store.Touch(ctx, params.ID, touchedAt); err != nil {
		t.Fatalf("Touch() failed: %v", err)
	}

	var lastSeen time.Time
	var expiresAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT last_seen_at, expires_at FROM sessions WHERE id = $1`, params.ID).
		Scan(&lastSeen, &expiresAt); err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if !lastSeen.Equal(touchedAt) {
		t.Errorf("last_seen_at = %v, want %v", lastSeen, touchedAt)
	}
	// Activity must NOT extend the session: the TTL is absolute by design.
	if !expiresAt.Equal(params.ExpiresAt) {
		t.Errorf("expires_at moved to %v; session activity must not extend the TTL", expiresAt)
	}
}

func TestDeleteExpiredRemovesOnlyThatUsersExpiredRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)

	expiredRaw, expiredParams := f.newSession(t, student, time.Hour)
	liveRaw, _ := f.newSession(t, student, time.Hour)
	if _, err := f.pool.Exec(ctx, `
		UPDATE sessions SET issued_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		 WHERE id = $1`, expiredParams.ID); err != nil {
		t.Fatalf("age session: %v", err)
	}

	// Someone else's expired session must survive this user's cleanup.
	other := f.newStudent(t)
	_, otherParams := f.newSession(t, other, time.Hour)
	if _, err := f.pool.Exec(ctx, `
		UPDATE sessions SET issued_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		 WHERE id = $1`, otherParams.ID); err != nil {
		t.Fatalf("age session: %v", err)
	}

	if err := f.store.DeleteExpired(ctx, student.ID); err != nil {
		t.Fatalf("DeleteExpired() failed: %v", err)
	}

	var expiredCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1`, expiredParams.ID).Scan(&expiredCount); err != nil {
		t.Fatalf("count expired session: %v", err)
	}
	if expiredCount != 0 {
		t.Error("the expired row was not deleted")
	}
	if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(liveRaw)); err != nil {
		t.Errorf("the live session was deleted too: %v", err)
	}
	if _, err := f.store.FindByTokenHash(ctx, auth.HashToken(expiredRaw)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("expired session error = %v, want ErrSessionNotFound", err)
	}

	var otherCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1`, otherParams.ID).Scan(&otherCount); err != nil {
		t.Fatalf("count other session: %v", err)
	}
	if otherCount != 1 {
		t.Error("DeleteExpired removed another account's row")
	}
}

// TestCreateRejectsNilUser proves the foreign key is doing its job: a session for
// a non-existent account must not be storable.
func TestCreateRejectsNilUser(t *testing.T) {
	f := newFixture(t)
	_, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken(): %v", err)
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken(): %v", err)
	}
	now := time.Now().UTC()
	_, err = f.store.Create(context.Background(), auth.CreateSessionParams{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TokenHash: hash,
		CSRFToken: csrf,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	if err == nil {
		t.Fatal("Create() succeeded for a non-existent user")
	}
}

// TestStoredTokenIsOnlyAHash is the §41 regression test: what is in the database
// must not be usable as a bearer token.
func TestStoredTokenIsOnlyAHash(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	student := f.newStudent(t)
	raw, params := f.newSession(t, student, time.Hour)

	var stored []byte
	if err := f.pool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1`, params.ID).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if string(stored) == raw {
		t.Fatal("the raw session token is stored in the database")
	}
	if len(stored) != 32 {
		t.Errorf("token_hash is %d bytes, want a 32-byte SHA-256", len(stored))
	}
	// Looking the session up by its SHA-256 is the only way in.
	if _, err := f.store.FindByTokenHash(ctx, []byte(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Error("the raw token was accepted as if it were the stored hash")
	}
}
