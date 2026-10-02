package user_test

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/testsupport/dbtest"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Phase 2 repository surface: the paged list, the display-name update, the status
// transition and the admin count. These need a real PostgreSQL server — the
// properties under test are the SQL's (citext matching, a stable ORDER BY, the
// last-admin guard evaluated inside the UPDATE) and none of them can be faked.
//
// The shared test database is never assumed to be empty: every row these tests
// create carries a unique account and is removed on cleanup, and the assertions
// that need a known population are filtered down to that namespace.

// indexLetters names the seeded rows. It is a fixed alphabet rather than a rune
// offset from 'a': arithmetic on runes produces names whose characters depend on
// the random tag's characters, which is how a shell of a test ends up asserting
// something other than what it reads like.
const indexLetters = "abcdefghijklmnopqrstuvwxyz"

// seedAccounts creates n accounts whose account and display name both contain tag,
// so a single `q` filter isolates them from everything else in the database.
//
// Every other row is a TEACHER with a (placeholder) password hash, because
// users_password_by_role requires exactly that pairing.
func seedAccounts(t *testing.T, repo *user.Postgres, tag string, n int) []*user.User {
	t.Helper()
	ctx := context.Background()
	out := make([]*user.User, 0, n)
	for i := 0; i < n; i++ {
		hash := "argon2id-placeholder"
		role := user.RoleStudent
		var passwordHash *string
		if i%2 == 1 {
			role = user.RoleTeacher
			passwordHash = &hash
		}
		created, err := repo.Create(ctx, user.CreateParams{
			Account:      tag + "_" + string(indexLetters[i]),
			DisplayName:  "账号 " + tag + " 编号" + string(indexLetters[i]),
			Role:         role,
			PasswordHash: passwordHash,
		})
		if err != nil {
			t.Fatalf("seed account %d: %v", i, err)
		}
		out = append(out, created)
	}
	return out
}

func TestListPageFiltersAndTotals(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "pg" + dbtest.RandomHex(6)
	seeded := seedAccounts(t, repo, tag, 5)
	ids := make([]uuid.UUID, 0, len(seeded))
	for _, u := range seeded {
		ids = append(ids, u.ID)
	}
	registerCleanup(t, pool, ids...)

	teacher := user.RoleTeacher
	student := user.RoleStudent
	active := user.StatusActive

	cases := []struct {
		name   string
		filter user.ListFilter
		want   int
	}{
		{"only this namespace", user.ListFilter{Query: tag, Limit: 50}, 5},
		{"role TEACHER", user.ListFilter{Query: tag, Role: &teacher, Limit: 50}, 2},
		{"role STUDENT", user.ListFilter{Query: tag, Role: &student, Limit: 50}, 3},
		{"status ACTIVE", user.ListFilter{Query: tag, Status: &active, Limit: 50}, 5},
		{"role and status together", user.ListFilter{Query: tag, Role: &teacher, Status: &active, Limit: 50}, 2},
		{"unknown fragment", user.ListFilter{Query: tag + "_nothing", Limit: 50}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := repo.ListPage(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListPage() failed: %v", err)
			}
			if result.Total != tc.want {
				t.Errorf("total = %d, want %d", result.Total, tc.want)
			}
			if len(result.Users) != tc.want {
				t.Errorf("returned %d rows, want %d", len(result.Users), tc.want)
			}
			for _, u := range result.Users {
				if !strings.Contains(u.Account, tag) {
					t.Errorf("row %q does not belong to the %q namespace", u.Account, tag)
				}
			}
		})
	}
}

// TestListPageIsCaseInsensitive pins the citext behaviour the admin search box
// depends on: an admin typing a student's account in the wrong case must still
// find them.
func TestListPageIsCaseInsensitive(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "CI" + dbtest.RandomHex(6)
	seeded := seedAccounts(t, repo, tag, 1)
	registerCleanup(t, pool, seeded[0].ID)

	for _, query := range []string{tag, strings.ToLower(tag), strings.ToUpper(tag)} {
		result, err := repo.ListPage(ctx, user.ListFilter{Query: query, Limit: 10})
		if err != nil {
			t.Fatalf("ListPage(%q) failed: %v", query, err)
		}
		if result.Total != 1 {
			t.Errorf("ListPage(%q).Total = %d, want 1: the search is case-sensitive", query, result.Total)
		}
	}

	// The display name is matched too — that is the field an admin usually knows.
	// The fragment is taken from the stored value rather than typed out, so the
	// assertion cannot depend on the collation's idea of case for non-ASCII text.
	displayFragment, _, _ := strings.Cut(seeded[0].DisplayName, " ")
	result, err := repo.ListPage(ctx, user.ListFilter{Query: displayFragment, Limit: 10})
	if err != nil {
		t.Fatalf("ListPage(display name) failed: %v", err)
	}
	if result.Total < 1 {
		t.Errorf("display-name search for %q found nothing", displayFragment)
	}
	found := false
	for _, u := range result.Users {
		if u.ID == seeded[0].ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the display-name search did not return %s", seeded[0].ID)
	}
}

// TestListPagePagingIsStable is the property that makes the admin list usable: two
// pages of the same filtered, stably ordered result must partition it. The seeded
// rows are created in one tight loop, so their created_at values collide — which is
// exactly the case an ORDER BY without the id tiebreaker gets wrong.
func TestListPagePagingIsStable(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "pg" + dbtest.RandomHex(6)
	seeded := seedAccounts(t, repo, tag, 7)
	ids := make([]uuid.UUID, 0, len(seeded))
	for _, u := range seeded {
		ids = append(ids, u.ID)
	}
	registerCleanup(t, pool, ids...)

	full, err := repo.ListPage(ctx, user.ListFilter{Query: tag, Limit: 50})
	if err != nil {
		t.Fatalf("ListPage() failed: %v", err)
	}
	if full.Total != 7 {
		t.Fatalf("total = %d, want 7", full.Total)
	}

	// Newest first, ties broken by id DESC.
	for i := 1; i < len(full.Users); i++ {
		prev, cur := full.Users[i-1], full.Users[i]
		if prev.CreatedAt.Before(cur.CreatedAt) {
			t.Errorf("row %d is older than row %d: created_at is not DESC", i-1, i)
		}
		if prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID.String() < cur.ID.String() {
			t.Errorf("tie between %s and %s is not broken by id DESC", prev.ID, cur.ID)
		}
	}

	// Paging must reproduce that order exactly, with no repeats and no gaps.
	var paged []user.User
	for offset := 0; offset < full.Total; offset += 3 {
		page, err := repo.ListPage(ctx, user.ListFilter{Query: tag, Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("ListPage(offset=%d) failed: %v", offset, err)
		}
		if page.Total != full.Total {
			t.Errorf("page at offset %d reports total %d, want %d", offset, page.Total, full.Total)
		}
		paged = append(paged, page.Users...)
	}
	if len(paged) != len(full.Users) {
		t.Fatalf("paging returned %d rows, want %d", len(paged), len(full.Users))
	}
	seen := map[uuid.UUID]int{}
	for i := range paged {
		if paged[i].ID != full.Users[i].ID {
			t.Errorf("position %d: paging returned %s, the full list returned %s",
				i, paged[i].ID, full.Users[i].ID)
		}
		seen[paged[i].ID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("account %s appeared %d times across pages", id, count)
		}
	}

	// An offset past the end is an empty page, not an error.
	past, err := repo.ListPage(ctx, user.ListFilter{Query: tag, Limit: 3, Offset: 100})
	if err != nil {
		t.Fatalf("ListPage(past the end) failed: %v", err)
	}
	if len(past.Users) != 0 || past.Total != full.Total {
		t.Errorf("page past the end = %d rows (total %d), want 0 rows and total %d",
			len(past.Users), past.Total, full.Total)
	}
	// Non-nil so the JSON layer can emit [] rather than null.
	if past.Users == nil {
		t.Error("ListPage returned a nil slice; the API would serialise it as null")
	}
}

// TestListPageBreaksTimestampTiesById is the direct test of the second ORDER BY key.
//
// It creates its rows in ONE statement, so every one of them carries the same
// created_at — the situation that makes an ORDER BY on created_at alone
// non-deterministic, and therefore makes an admin paging through the list see a row
// twice and never see another. The assertion is on the order the database returns,
// including the paging of it, because that is the property the admin list depends
// on.
func TestListPageBreaksTimestampTiesById(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "tie" + dbtest.RandomHex(6)
	// One INSERT, one transaction timestamp, five rows: created_at is identical for
	// all of them by construction.
	rows, err := pool.Query(ctx, `
		INSERT INTO users (account, display_name, role, password_hash, created_at, updated_at)
		SELECT $1 || '_' || i, '并列 ' || i, 'STUDENT', NULL, now(), now()
		  FROM generate_series(1, 5) AS i
		RETURNING id`, tag)
	if err != nil {
		t.Fatalf("seed tied rows: %v", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(ids) != 5 {
		t.Fatalf("seeded %d rows, want 5", len(ids))
	}
	registerCleanup(t, pool, ids...)

	// Confirm the premise: the timestamps really are equal, otherwise this test
	// would pass for the wrong reason.
	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT created_at) FROM users WHERE account LIKE $1`, tag+"_%").Scan(&distinct); err != nil {
		t.Fatalf("count distinct timestamps: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("the seeded rows have %d distinct created_at values, want 1: the tie is not real", distinct)
	}

	sorted := append([]uuid.UUID{}, ids...)
	sort.Slice(sorted, func(i, j int) bool {
		// uuid.UUID is a byte array, and PostgreSQL orders the type by those bytes.
		// The expectation is derived from that same rule — a byte-wise comparison —
		// rather than from a text rendering of it, which would disagree about the
		// position of the hyphens.
		return bytes.Compare(sorted[i][:], sorted[j][:]) > 0
	})

	full, err := repo.ListPage(ctx, user.ListFilter{Query: tag, Limit: 10})
	if err != nil {
		t.Fatalf("ListPage() failed: %v", err)
	}
	if len(full.Users) != 5 {
		t.Fatalf("returned %d rows, want 5", len(full.Users))
	}
	for i, want := range sorted {
		if full.Users[i].ID != want {
			t.Errorf("position %d = %s, want %s (id DESC on equal created_at)", i, full.Users[i].ID, want)
		}
	}

	// Paging the tied rows must partition them in exactly that order.
	var paged []uuid.UUID
	for offset := 0; offset < 5; offset += 2 {
		page, err := repo.ListPage(ctx, user.ListFilter{Query: tag, Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("ListPage(offset=%d) failed: %v", offset, err)
		}
		for _, u := range page.Users {
			paged = append(paged, u.ID)
		}
	}
	if len(paged) != 5 {
		t.Fatalf("paging returned %d rows, want 5", len(paged))
	}
	for i, want := range sorted {
		if paged[i] != want {
			t.Errorf("paged position %d = %s, want %s", i, paged[i], want)
		}
	}
}

func TestListPageRejectsAnUnboundedQuery(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	// A page size of zero must be refused rather than meaning "everything": the
	// caller's bug would otherwise turn into a full-table read.
	if _, err := repo.ListPage(ctx, user.ListFilter{Limit: 0}); err == nil {
		t.Error("ListPage(limit=0) succeeded, want an error")
	}
	if _, err := repo.ListPage(ctx, user.ListFilter{Limit: 10, Offset: -1}); err == nil {
		t.Error("ListPage(offset=-1) succeeded, want an error")
	}
}

func TestUpdateDisplayNameWritesAndBumpsUpdatedAt(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "pg" + dbtest.RandomHex(6)
	created := seedAccounts(t, repo, tag, 1)[0]
	registerCleanup(t, pool, created.ID)

	before := created.UpdatedAt
	time.Sleep(2 * time.Millisecond)

	if err := repo.UpdateDisplayName(ctx, created.ID, "新名字"); err != nil {
		t.Fatalf("UpdateDisplayName() failed: %v", err)
	}
	updated, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if updated.DisplayName != "新名字" {
		t.Errorf("display_name = %q, want 新名字", updated.DisplayName)
	}
	if !updated.UpdatedAt.After(before) {
		t.Errorf("updated_at did not move forward: %s -> %s", before, updated.UpdatedAt)
	}
	if updated.Role != created.Role || updated.Status != created.Status || updated.Account != created.Account {
		t.Errorf("a rename changed something else: %+v", updated)
	}

	// A missing row is ErrNotFound, mapped the same way as every other write.
	if err := repo.UpdateDisplayName(ctx, uuid.New(), "ghost"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("UpdateDisplayName(unknown) error = %v, want ErrNotFound", err)
	}
	// A blank name is rejected by the CHECK constraint and translated into a
	// readable error rather than a SQLSTATE.
	err = repo.UpdateDisplayName(ctx, created.ID, "   ")
	if err == nil {
		t.Fatal("UpdateDisplayName(blank) succeeded, want a constraint failure")
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "23514") {
		t.Errorf("the error leaks driver detail: %v", err)
	}
}

func TestSetStatusTransitionsAndTimestamps(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	tag := "pg" + dbtest.RandomHex(6)
	created := seedAccounts(t, repo, tag, 2)[1] // a TEACHER: the admin guard does not apply
	registerCleanup(t, pool, created.ID)

	before := created.UpdatedAt
	time.Sleep(2 * time.Millisecond)

	if err := repo.SetStatus(ctx, created.ID, user.StatusDisabled); err != nil {
		t.Fatalf("SetStatus(DISABLED) failed: %v", err)
	}
	disabled, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if disabled.Status != user.StatusDisabled {
		t.Errorf("status = %q, want DISABLED", disabled.Status)
	}
	if !disabled.UpdatedAt.After(before) {
		t.Error("updated_at did not move forward on a status change")
	}

	// Idempotent: setting the current status is a successful write.
	if err := repo.SetStatus(ctx, created.ID, user.StatusDisabled); err != nil {
		t.Errorf("SetStatus(DISABLED) on an already disabled account failed: %v", err)
	}

	if err := repo.SetStatus(ctx, created.ID, user.StatusActive); err != nil {
		t.Fatalf("SetStatus(ACTIVE) failed: %v", err)
	}
	reenabled, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if reenabled.Status != user.StatusActive {
		t.Errorf("status = %q, want ACTIVE", reenabled.Status)
	}

	if err := repo.SetStatus(ctx, uuid.New(), user.StatusDisabled); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("SetStatus(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestSetStatusRefusesToRemoveTheLastActiveAdmin exercises the guard INSIDE the
// UPDATE, which is the only place it can be race-free. The test parks the
// pre-existing administrators for its duration and restores them afterwards.
func TestSetStatusRefusesToRemoveTheLastActiveAdmin(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	restore := parkActiveAdmins(t, pool)
	defer restore()

	first := createAdmin(t, repo, pool, "adm1"+dbtest.RandomHex(4))
	second := createAdmin(t, repo, pool, "adm2"+dbtest.RandomHex(4))

	// Two active admins: the transition is allowed.
	if err := repo.SetStatus(ctx, second.ID, user.StatusDisabled); err != nil {
		t.Fatalf("disabling one of two administrators failed: %v", err)
	}
	// Setting the status the row already has must still succeed: the guard only
	// covers the ACTIVE→DISABLED transition, otherwise an idempotent disable of an
	// already-disabled admin would fail.
	if err := repo.SetStatus(ctx, second.ID, user.StatusDisabled); err != nil {
		t.Fatalf("re-disabling an already disabled administrator failed: %v", err)
	}

	// One active admin left: the guard refuses.
	err := repo.SetStatus(ctx, first.ID, user.StatusDisabled)
	if !errors.Is(err, user.ErrLastAdmin) {
		t.Fatalf("disabling the last active administrator: error = %v, want ErrLastAdmin", err)
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "users_") {
		t.Errorf("the error leaks schema detail: %v", err)
	}
	current, err := repo.FindByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("FindByID() failed: %v", err)
	}
	if current.Status != user.StatusActive {
		t.Errorf("status = %q, want ACTIVE: the last administrator was disabled", current.Status)
	}

	// Re-enabling the second one lets the first be disabled, so the guard is a
	// rule about the current state and not a permanent lock.
	if err := repo.SetStatus(ctx, second.ID, user.StatusActive); err != nil {
		t.Fatalf("re-enabling the second administrator failed: %v", err)
	}
	if err := repo.SetStatus(ctx, first.ID, user.StatusDisabled); err != nil {
		t.Fatalf("disabling one of two administrators failed: %v", err)
	}
}

// TestSetStatusGuardDoesNotBlockNonAdmins states the other half: the guard must be
// invisible for teachers and students, including the last disabled teacher.
func TestSetStatusGuardDoesNotBlockNonAdmins(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	restore := parkActiveAdmins(t, pool)
	defer restore()

	tag := "pg" + dbtest.RandomHex(6)
	seeded := seedAccounts(t, repo, tag, 2)
	registerCleanup(t, pool, seeded[0].ID, seeded[1].ID)

	for _, u := range seeded {
		if err := repo.SetStatus(ctx, u.ID, user.StatusDisabled); err != nil {
			t.Errorf("disabling a %s account with no active admin present failed: %v", u.Role, err)
		}
	}
}

func TestCountActiveAdmins(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	restore := parkActiveAdmins(t, pool)
	defer restore()

	base, err := repo.CountActiveAdmins(ctx)
	if err != nil {
		t.Fatalf("CountActiveAdmins() failed: %v", err)
	}
	if base != 0 {
		t.Fatalf("active admins after parking = %d, want 0", base)
	}

	created := createAdmin(t, repo, pool, "cnt"+dbtest.RandomHex(4))
	after, err := repo.CountActiveAdmins(ctx)
	if err != nil {
		t.Fatalf("CountActiveAdmins() failed: %v", err)
	}
	if after != 1 {
		t.Errorf("active admins = %d, want 1", after)
	}

	// A DISABLED admin does not count: the question the count answers is "who can
	// still manage this deployment?". The row is flipped directly because the
	// repository correctly refuses to disable the only active administrator — that
	// rule is asserted in TestSetStatusRefusesToRemoveTheLastActiveAdmin, and a
	// count test must not depend on being able to break it.
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("disable directly: %v", err)
	}
	disabled, err := repo.CountActiveAdmins(ctx)
	if err != nil {
		t.Fatalf("CountActiveAdmins() failed: %v", err)
	}
	if disabled != 0 {
		t.Errorf("active admins = %d, want 0 after disabling the only one", disabled)
	}
}

// parkActiveAdmins disables every active administrator and returns a function that
// puts them back.
//
// It lets a test make statements about "the last active admin" without depending
// on what else happens to share the test database — the guard is about the state of
// the whole users table, so a test for it has to own that state. The restore
// function is registered with t.Cleanup by the caller, so even a failing test
// leaves the database as it found it.
func parkActiveAdmins(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	ctx := context.Background()

	rows, err := pool.Query(ctx, `SELECT id FROM users WHERE role = 'ADMIN' AND status = 'ACTIVE'`)
	if err != nil {
		t.Fatalf("list active admins: %v", err)
	}
	var parked []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan active admin: %v", err)
		}
		parked = append(parked, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate active admins: %v", err)
	}

	if len(parked) > 0 {
		if _, err := pool.Exec(ctx,
			`UPDATE users SET status = 'DISABLED' WHERE role = 'ADMIN' AND status = 'ACTIVE'`); err != nil {
			t.Fatalf("park active admins: %v", err)
		}
	}
	return func() {
		for _, id := range parked {
			if _, err := pool.Exec(context.Background(),
				`UPDATE users SET status = 'ACTIVE' WHERE id = $1`, id); err != nil {
				t.Logf("restore admin %s: %v", id, err)
			}
		}
	}
}

// createAdmin inserts an ACTIVE administrator. Password hashes are placeholders:
// nothing in this file authenticates, and the repository stores what it is given.
func createAdmin(t *testing.T, repo *user.Postgres, pool *pgxpool.Pool, account string) *user.User {
	t.Helper()
	hash := "argon2id-placeholder"
	created, err := repo.Create(context.Background(), user.CreateParams{
		Account:      account,
		DisplayName:  "管理员 " + account,
		Role:         user.RoleAdmin,
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	registerCleanup(t, pool, created.ID)
	return created
}
