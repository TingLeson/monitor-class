package user_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// These tests need a real PostgreSQL server. They verify the properties that only
// the database can provide: the citext case-insensitivity, the CHECK constraints
// that encode the business rules, and the mapping of driver errors onto business
// errors. They are skipped when TEST_DATABASE_URL is unset.
//
// Rows are created with unique accounts (dbtest.RandomAccount) and removed on
// cleanup, so several test packages can share one database without interfering.

func newRepository(t *testing.T) (*user.Postgres, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Pool(t)
	return user.NewPostgres(pool), pool
}

// registerCleanup removes the account (and its sessions, by cascade) after the
// test, keeping the shared test database from growing forever.
func registerCleanup(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("cleanup: delete user %s: %v", id, err)
			}
		}
	})
}

func hashPtr(t *testing.T, hash string) *string {
	t.Helper()
	return &hash
}

func TestCreateAndFindByAccountIsCaseInsensitive(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	// The stored account is upper case; the lookup uses lower case. This is the
	// single most important property of the account column: a student typing
	// "s10086" must reach the account created as "S10086".
	account := dbtest.RandomAccount("S10086")
	hash := "hash-value"
	created, err := repo.Create(ctx, user.CreateParams{
		Account:      account,
		DisplayName:  "张三",
		Role:         user.RoleStudent,
		PasswordHash: nil,
	})
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	registerCleanup(t, pool, created.ID)

	if created.ID == uuid.Nil {
		t.Error("Create() returned a nil id; the column default was not applied")
	}
	if created.Status != user.StatusActive {
		t.Errorf("status = %q, want ACTIVE (the column default)", created.Status)
	}
	if created.LastLoginAt != nil {
		t.Error("last_login_at is set on a brand new account")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("timestamps are not populated")
	}
	if created.PasswordHash != nil {
		t.Error("a STUDENT account came back with a password hash")
	}

	found, err := repo.FindByAccount(ctx, strings.ToLower(account))
	if err != nil {
		t.Fatalf("FindByAccount(lower case) failed: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("FindByAccount() returned %s, want %s", found.ID, created.ID)
	}
	if found.DisplayName != "张三" {
		t.Errorf("display_name = %q, want the UTF-8 value back unchanged", found.DisplayName)
	}

	byID, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if byID.Account != account {
		t.Errorf("account = %q, want %q", byID.Account, account)
	}
	_ = hash
}

func TestFindMissingAccountReturnsErrNotFound(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	if _, err := repo.FindByAccount(ctx, dbtest.RandomAccount("ghost")); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByAccount() error = %v, want ErrNotFound", err)
	}
	if _, err := repo.FindByID(ctx, uuid.New()); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByID() error = %v, want ErrNotFound", err)
	}
}

// TestCreateMapsUniqueViolationToAccountTaken covers the mapping §63 asks for:
// a duplicate account is a business error, not a driver error leaking a
// constraint name to the client.
func TestCreateMapsUniqueViolationToAccountTaken(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	account := dbtest.RandomAccount("Case")
	hash := "hash-value"
	first, err := repo.Create(ctx, user.CreateParams{
		Account:      account,
		DisplayName:  "First",
		Role:         user.RoleTeacher,
		PasswordHash: hashPtr(t, hash),
	})
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	registerCleanup(t, pool, first.ID)

	// Different letter case: citext makes this the SAME account, which is exactly
	// the ambiguity an impersonation attempt would rely on.
	duplicate := strings.ToUpper(account)
	if duplicate == account {
		duplicate = strings.ToLower(account)
	}
	_, err = repo.Create(ctx, user.CreateParams{
		Account:      duplicate,
		DisplayName:  "Second",
		Role:         user.RoleTeacher,
		PasswordHash: hashPtr(t, hash),
	})
	if !errors.Is(err, user.ErrAccountTaken) {
		t.Fatalf("Create(duplicate) error = %v, want ErrAccountTaken", err)
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("error leaks driver detail: %v", err)
	}
}

// TestDatabaseEnforcesPasswordByRole proves the §9 rule is enforced by the
// database, not only by the code path that happens to create users today.
func TestDatabaseEnforcesPasswordByRole(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	hash := "argon2id-placeholder"

	cases := []struct {
		name    string
		params  user.CreateParams
		wantMsg string
	}{
		{
			name: "student with a password",
			params: user.CreateParams{
				Account:      dbtest.RandomAccount("stu"),
				DisplayName:  "Student",
				Role:         user.RoleStudent,
				PasswordHash: hashPtr(t, hash),
			},
			wantMsg: "users_password_by_role",
		},
		{
			name: "teacher without a password",
			params: user.CreateParams{
				Account:     dbtest.RandomAccount("tea"),
				DisplayName: "Teacher",
				Role:        user.RoleTeacher,
			},
			wantMsg: "users_password_by_role",
		},
		{
			name: "admin without a password",
			params: user.CreateParams{
				Account:     dbtest.RandomAccount("adm"),
				DisplayName: "Admin",
				Role:        user.RoleAdmin,
			},
			wantMsg: "users_password_by_role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.Create(ctx, tc.params)
			if err == nil {
				t.Fatal("Create() succeeded, want the CHECK constraint to reject it")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to name constraint %s", err, tc.wantMsg)
			}
		})
	}
}

func TestDatabaseEnforcesAccountFormatAndDisplayName(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	hash := "argon2id-placeholder"

	cases := []struct {
		name    string
		params  user.CreateParams
		wantMsg string
	}{
		{
			// A blank or invisible account would be unrestrictable and impossible
			// to tell apart from another account on screen.
			name: "account with a space",
			params: user.CreateParams{
				Account: "bad account", DisplayName: "X", Role: user.RoleStudent,
			},
			wantMsg: "users_account_format",
		},
		{
			name: "account too short",
			params: user.CreateParams{
				Account: "ab", DisplayName: "X", Role: user.RoleStudent,
			},
			wantMsg: "users_account_format",
		},
		{
			name: "account with a homoglyph",
			params: user.CreateParams{
				// Cyrillic "а" instead of Latin "a".
				Account: "S1008а", DisplayName: "X", Role: user.RoleStudent,
			},
			wantMsg: "users_account_format",
		},
		{
			name: "blank display name",
			params: user.CreateParams{
				Account: dbtest.RandomAccount("blank"), DisplayName: "   ", Role: user.RoleStudent,
			},
			wantMsg: "users_display_name_not_blank",
		},
		{
			name: "unknown role",
			params: user.CreateParams{
				// A password is supplied as well so that only the role constraint
				// can fail: PostgreSQL does not promise which CHECK it reports
				// first, and a test asserting the wrong one would be flaky.
				Account: dbtest.RandomAccount("role"), DisplayName: "X",
				Role: user.Role("PRINCIPAL"), PasswordHash: hashPtr(t, hash),
			},
			wantMsg: "users_role_valid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.Create(ctx, tc.params)
			if err == nil {
				t.Fatal("Create() succeeded, want the CHECK constraint to reject it")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to name constraint %s", err, tc.wantMsg)
			}
			// The business rule is explained in prose, not only as a SQLSTATE.
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Errorf("error %v does not wrap the driver error; nothing to log", err)
			}
		})
	}
	_ = hash
}

func TestTouchLastLoginAndSetPasswordHash(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	account := dbtest.RandomAccount("tea")
	created, err := repo.Create(ctx, user.CreateParams{
		Account:      account,
		DisplayName:  "Teacher",
		Role:         user.RoleTeacher,
		PasswordHash: hashPtr(t, "first-hash"),
	})
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	registerCleanup(t, pool, created.ID)

	if err := repo.TouchLastLogin(ctx, created.ID); err != nil {
		t.Fatalf("TouchLastLogin() failed: %v", err)
	}
	afterLogin, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if afterLogin.LastLoginAt == nil {
		t.Fatal("last_login_at is still NULL after TouchLastLogin")
	}
	// updated_at is maintained by the application (see migrations/0002), so a
	// write must be visible there too.
	if !afterLogin.UpdatedAt.After(created.UpdatedAt) && afterLogin.UpdatedAt.Equal(created.UpdatedAt) {
		t.Error("updated_at did not move on update")
	}

	if err := repo.SetPasswordHash(ctx, created.ID, "second-hash"); err != nil {
		t.Fatalf("SetPasswordHash() failed: %v", err)
	}
	afterReset, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if afterReset.PasswordHash == nil || *afterReset.PasswordHash != "second-hash" {
		t.Errorf("password_hash = %v, want the new value", afterReset.PasswordHash)
	}

	// Unknown ids are a business error, not a silent success.
	if err := repo.TouchLastLogin(ctx, uuid.New()); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("TouchLastLogin(unknown) = %v, want ErrNotFound", err)
	}
	if err := repo.SetPasswordHash(ctx, uuid.New(), "x"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("SetPasswordHash(unknown) = %v, want ErrNotFound", err)
	}
}

func TestListFiltersByRole(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	studentAccount := dbtest.RandomAccount("list_stu")
	teacherAccount := dbtest.RandomAccount("list_tea")

	student, err := repo.Create(ctx, user.CreateParams{Account: studentAccount, DisplayName: "S", Role: user.RoleStudent})
	if err != nil {
		t.Fatalf("Create(student) failed: %v", err)
	}
	teacher, err := repo.Create(ctx, user.CreateParams{
		Account: teacherAccount, DisplayName: "T", Role: user.RoleTeacher, PasswordHash: hashPtr(t, "h"),
	})
	if err != nil {
		t.Fatalf("Create(teacher) failed: %v", err)
	}
	registerCleanup(t, pool, student.ID, teacher.ID)

	role := user.RoleStudent
	students, err := repo.List(ctx, &role)
	if err != nil {
		t.Fatalf("List(STUDENT) failed: %v", err)
	}
	var sawStudent, sawTeacher bool
	for _, u := range students {
		if u.Role != user.RoleStudent {
			t.Fatalf("List(STUDENT) returned a %s account (%s)", u.Role, u.Account)
		}
		if u.ID == student.ID {
			sawStudent = true
		}
		if u.ID == teacher.ID {
			sawTeacher = true
		}
	}
	if !sawStudent {
		t.Error("List(STUDENT) did not include the student that was just created")
	}
	if sawTeacher {
		t.Error("List(STUDENT) included a teacher")
	}

	all, err := repo.List(ctx, nil)
	if err != nil {
		t.Fatalf("List(nil) failed: %v", err)
	}
	if len(all) < 2 {
		t.Errorf("List(nil) returned %d rows, want at least the 2 created here", len(all))
	}
}

// TestCreatedByIsRecordedAndSurvivesDeletion checks the self-referencing foreign
// key: ON DELETE SET NULL must keep the student when the creating admin is gone.
func TestCreatedByIsRecordedAndSurvivesDeletion(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	admin, err := repo.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("adm"), DisplayName: "Admin",
		Role: user.RoleAdmin, PasswordHash: hashPtr(t, "h"),
	})
	if err != nil {
		t.Fatalf("Create(admin) failed: %v", err)
	}
	student, err := repo.Create(ctx, user.CreateParams{
		Account: dbtest.RandomAccount("stu"), DisplayName: "Student",
		Role: user.RoleStudent, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("Create(student) failed: %v", err)
	}
	registerCleanup(t, pool, student.ID, admin.ID)

	if student.CreatedBy == nil || *student.CreatedBy != admin.ID {
		t.Fatalf("created_by = %v, want %s", student.CreatedBy, admin.ID)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, admin.ID); err != nil {
		t.Fatalf("delete admin: %v", err)
	}
	after, err := repo.FindByID(ctx, student.ID)
	if err != nil {
		t.Fatalf("the student disappeared with the admin: %v", err)
	}
	if after.CreatedBy != nil {
		t.Errorf("created_by = %v, want NULL after the creator was deleted", after.CreatedBy)
	}
}
