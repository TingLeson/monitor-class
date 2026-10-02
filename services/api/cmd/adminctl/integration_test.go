package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// The tests below exercise the real command against a real database: this is the
// Phase 1 bootstrap path (§4), so "it compiles" is not an interesting claim about
// it. Skipped when TEST_DATABASE_URL is unset.

// cliEnv prepares a process environment in which config.Load() succeeds without
// touching the developer's .env, and returns the connected pool.
func cliEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := dbtest.URL(t)
	pool := dbtest.Pool(t)

	// A temp working directory keeps config.Load from picking up the repository
	// .env, so the test controls every value the CLI sees.
	t.Chdir(t.TempDir())
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("LIVEKIT_URL", "ws://localhost:7880")
	t.Setenv("LIVEKIT_API_KEY", "devkey")
	t.Setenv("LIVEKIT_API_SECRET", "devsecret")
	t.Setenv("PASSWORD_MIN_LENGTH", "12")
	t.Setenv("STARTUP_REQUIRE_DEPENDENCIES", "false")
	return pool
}

// runWithStdin drives the CLI with a scripted stdin.
func runWithStdin(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// deleteAccount removes a created account once the test is done, so the shared
// test database does not accumulate rows.
func deleteAccount(t *testing.T, pool *pgxpool.Pool, account string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE account = $1::citext`, account); err != nil {
			t.Logf("cleanup: delete %s: %v", account, err)
		}
	})
}

func TestCreateUserAndLoginWithIt(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()
	repo := user.NewPostgres(pool)

	const password = "a-teacher-passphrase-1"
	account := dbtest.RandomAccount("ctl_tea")
	deleteAccount(t, pool, account)

	code, stdout, stderr := runWithStdin(t, password+"\n",
		"create-user", "--account", account, "--display-name", "李老师", "--role", "TEACHER", "--password-stdin")
	if code != exitOK {
		t.Fatalf("create-user exit code = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, account) {
		t.Errorf("confirmation does not mention the account: %q", stdout)
	}
	// The password must not be echoed anywhere — not in the confirmation, not in
	// the log line (which goes to stderr).
	if strings.Contains(stdout, password) || strings.Contains(stderr, password) {
		t.Error("the password was echoed by adminctl")
	}

	created, err := repo.FindByAccount(ctx, account)
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if created.Role != user.RoleTeacher || created.Status != user.StatusActive {
		t.Errorf("created %s/%s, want TEACHER/ACTIVE", created.Role, created.Status)
	}
	if !created.HasPassword() {
		t.Fatal("a TEACHER account was created without a password hash")
	}
	// The stored value must be a hash this API can verify.
	ok, needsRehash, err := auth.Verify(*created.PasswordHash, password)
	if err != nil || !ok {
		t.Fatalf("Verify() = (%v, %v), want the created password to verify", ok, err)
	}
	if needsRehash {
		t.Error("the created hash uses stale parameters")
	}
	if strings.Contains(*created.PasswordHash, password) {
		t.Fatal("the stored hash contains the plaintext password")
	}

	// A second account with the same name fails with exit 1 (not 2: the
	// invocation was fine, the data was not).
	code, _, stderr = runWithStdin(t, password+"\n",
		"create-user", "--account", strings.ToUpper(account), "--display-name", "Duplicate", "--role", "TEACHER", "--password-stdin")
	if code != exitFailure {
		t.Fatalf("duplicate create-user exit code = %d, want %d (stderr=%s)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want a readable 'already exists' message", stderr)
	}
}

func TestCreateStudentMustNotHaveAPassword(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()
	repo := user.NewPostgres(pool)

	account := dbtest.RandomAccount("ctl_stu")
	deleteAccount(t, pool, account)

	code, _, stderr := runWithStdin(t, "",
		"create-user", "--account", account, "--display-name", "张三", "--role", "STUDENT")
	if code != exitOK {
		t.Fatalf("create-user exit code = %d (stderr=%s)", code, stderr)
	}
	created, err := repo.FindByAccount(ctx, account)
	if err != nil {
		t.Fatalf("the student was not created: %v", err)
	}
	if created.PasswordHash != nil {
		t.Fatal("a STUDENT account was created with a password hash")
	}
}

func TestCreateUserRejectsAWeakPassword(t *testing.T) {
	pool := cliEnv(t)
	account := dbtest.RandomAccount("ctl_weak")
	deleteAccount(t, pool, account)

	// The policy comes from PASSWORD_MIN_LENGTH (12 in this environment).
	const weak = "weakpass"
	code, _, stderr := runWithStdin(t, weak+"\n",
		"create-user", "--account", account, "--display-name", "Weak", "--role", "TEACHER", "--password-stdin")
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr=%s)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "password rejected") {
		t.Errorf("stderr = %q, want the policy violation to be explained", stderr)
	}
	// And the rejected password must not be echoed back.
	if strings.Contains(stderr, weak) {
		t.Errorf("stderr echoes the rejected password: %q", stderr)
	}
}

func TestResetPasswordRevokesSessions(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()
	repo := user.NewPostgres(pool)
	store := sessionstore.New(pool)

	const firstPassword = "a-teacher-passphrase-1"
	const secondPassword = "a-brand-new-passphrase-2"
	account := dbtest.RandomAccount("ctl_reset")
	deleteAccount(t, pool, account)

	if code, _, stderr := runWithStdin(t, firstPassword+"\n",
		"create-user", "--account", account, "--display-name", "Reset", "--role", "TEACHER", "--password-stdin"); code != exitOK {
		t.Fatalf("create-user exit code = %d (stderr=%s)", code, stderr)
	}
	created, err := repo.FindByAccount(ctx, account)
	if err != nil {
		t.Fatalf("FindByAccount: %v", err)
	}

	// A live session for that account, which the reset must terminate.
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken: %v", err)
	}
	now := time.Now().UTC()
	if _, err := store.Create(ctx, auth.CreateSessionParams{
		ID: uuid.New(), UserID: created.ID, TokenHash: hash, CSRFToken: csrf,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	code, stdout, stderr := runWithStdin(t, secondPassword+"\n", "reset-password", "--account", account, "--password-stdin")
	if code != exitOK {
		t.Fatalf("reset-password exit code = %d (stderr=%s)", code, stderr)
	}
	if strings.Contains(stdout, secondPassword) || strings.Contains(stderr, secondPassword) {
		t.Error("the new password was echoed")
	}

	updated, err := repo.FindByAccount(ctx, account)
	if err != nil {
		t.Fatalf("FindByAccount after reset: %v", err)
	}
	if ok, _, err := auth.Verify(*updated.PasswordHash, secondPassword); err != nil || !ok {
		t.Fatalf("the new password does not verify: (%v, %v)", ok, err)
	}
	if ok, _, _ := auth.Verify(*updated.PasswordHash, firstPassword); ok {
		t.Error("the old password still verifies after a reset")
	}

	// The old session must be gone: a reset that leaves sessions alive would be
	// cosmetic (the whole point is that the old credential may have leaked).
	if _, err := store.FindByTokenHash(ctx, auth.HashToken(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("session lookup error = %v, want ErrSessionNotFound after a password reset", err)
	}
}

func TestResetPasswordRejectsStudentAccounts(t *testing.T) {
	pool := cliEnv(t)
	account := dbtest.RandomAccount("ctl_stu_pw")
	deleteAccount(t, pool, account)

	if code, _, stderr := runWithStdin(t, "",
		"create-user", "--account", account, "--display-name", "Student", "--role", "STUDENT"); code != exitOK {
		t.Fatalf("create-user exit code = %d (stderr=%s)", code, stderr)
	}
	// Students have no password to reset: writing one would violate §9.
	code, _, stderr := runWithStdin(t, "a-brand-new-passphrase-2\n", "reset-password", "--account", account, "--password-stdin")
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr=%s)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "no password to reset") {
		t.Errorf("stderr = %q, want an explanation", stderr)
	}
}

func TestResetPasswordUnknownAccount(t *testing.T) {
	cliEnv(t)

	code, _, stderr := runWithStdin(t, "a-brand-new-passphrase-2\n", "reset-password", "--account", "no-such-account", "--password-stdin")
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr=%s)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want a not-found message", stderr)
	}
}

func TestListUsersNeverPrintsHashes(t *testing.T) {
	pool := cliEnv(t)
	account := dbtest.RandomAccount("ctl_list")
	deleteAccount(t, pool, account)

	const password = "a-teacher-passphrase-1"
	if code, _, stderr := runWithStdin(t, password+"\n",
		"create-user", "--account", account, "--display-name", "Listed", "--role", "TEACHER", "--password-stdin"); code != exitOK {
		t.Fatalf("create-user exit code = %d (stderr=%s)", code, stderr)
	}

	code, stdout, stderr := runWithStdin(t, "", "list-users", "--role", "TEACHER")
	if code != exitOK {
		t.Fatalf("list-users exit code = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, account) {
		t.Errorf("list-users did not list %s:\n%s", account, stdout)
	}
	if strings.Contains(stdout, "$argon2id$") || strings.Contains(stdout, password) {
		t.Error("list-users printed password material")
	}
	if !strings.Contains(stdout, "ROLE") || !strings.Contains(stdout, "STATUS") {
		t.Errorf("list-users output is missing its header:\n%s", stdout)
	}
}
