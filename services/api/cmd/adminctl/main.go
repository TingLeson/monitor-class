// Command adminctl is the Phase 1 account-administration tool.
//
// WHY a CLI and not (yet) an HTTP API: creating the FIRST admin cannot require an
// admin — the bootstrap problem. §4 allows exactly this escape hatch, and Phase 2
// adds the admin HTTP surface on top of the same repository.
//
// Three rules shape this tool:
//
//   - Passwords are never command-line arguments. Everything passed as an
//     argument ends up in the shell history, in `ps` output for every user on the
//     box, and in CI logs. `--password-stdin` is the only accepted form: it works
//     with a pipe, a heredoc or `read -s`.
//   - Passwords are never echoed. Not in a log line, not in an error, not in the
//     confirmation message. This tool creates credentials, so it is exactly the
//     program whose output gets pasted into a ticket.
//   - Nothing here is a business API. It writes accounts and nothing else; it
//     cannot open a classroom, and it does not implement a "list students of a
//     classroom" shortcut that would later need to be kept in sync with the real
//     authorization rules.
//
// Usage:
//
//	adminctl create-user --account S10086 --display-name "张三" --role STUDENT
//	adminctl create-user --account teacher001 --display-name "李老师" --role TEACHER --password-stdin <<<"$PW"
//	adminctl reset-password --account teacher001 --password-stdin < /run/secrets/pw
//	adminctl list-users [--role TEACHER]
//
// Exit codes: 0 success, 1 failure, 2 usage error — a script can distinguish
// "you called me wrong" from "the operation failed".
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/auth"
	"github.com/classwatch/classwatch/services/api/internal/auth/sessionstore"
	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/postgres"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// commandTimeout bounds the whole run so an unreachable database fails with a
// clear message instead of hanging a deploy script.
const commandTimeout = 60 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is separated from main so the commands can be exercised by tests with
// injected streams (and so no test ever has to fork a process to check an exit
// code).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "create-user":
		return createUser(args[1:], stdin, stdout, stderr)
	case "reset-password":
		return resetPassword(args[1:], stdin, stdout, stderr)
	case "list-users":
		return listUsers(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

// usageText is kept as a constant (and written with io.WriteString) so no
// formatting pass can interpret the literal % characters in the shell example.
const usageText = `adminctl — ClassWatch account administration (Phase 1)

Usage:
  adminctl create-user --account X --display-name Y --role ADMIN|TEACHER|STUDENT [--password-stdin] [--created-by ACCOUNT]
  adminctl reset-password --account X --password-stdin
  adminctl list-users [--role ADMIN|TEACHER|STUDENT]

Notes:
  --password-stdin reads the password from the first line of stdin. There is no
  argument form on purpose: an argument would be visible in the shell history and
  in the process list. When typing interactively, hide the echo yourself:
      read -rs PW && printf '%s\n' "$PW" | adminctl create-user ... --password-stdin

  STUDENT accounts must NOT have a password (§2.2); ADMIN and TEACHER accounts
  must have one.

Exit codes: 0 success, 1 failure, 2 usage error.
`

func usage(w io.Writer) {
	_, _ = io.WriteString(w, usageText)
}

// app bundles the process-wide dependencies of one command.
type app struct {
	cfg      *config.Config
	logger   *slog.Logger
	users    *user.Postgres
	sessions *sessionstore.Store
	db       *postgres.DB
}

// setup loads configuration and connects to PostgreSQL.
//
// Configuration comes from the same loader as the API and the migration runner:
// one definition of DATABASE_URL, one place where PASSWORD_MIN_LENGTH is parsed.
// The cost is that adminctl needs the same environment as the API (docker-compose
// gives it the same anchor), which is a fair price for not having a second,
// silently diverging configuration path.
func setup(stderr io.Writer) (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	// Logs go to stderr so stdout stays a clean data channel for `list-users`.
	logger := logging.New(cfg, stderr)
	logging.SetupDefault(logger)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	db, err := postgres.Connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	pool := db.Pool()
	return &app{
		cfg:      cfg,
		logger:   logger,
		users:    user.NewPostgres(pool),
		sessions: sessionstore.New(pool),
		db:       db,
	}, nil
}

func createUser(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	fs.SetOutput(stderr)
	account := fs.String("account", "", "login account (3-64 characters: A-Z a-z 0-9 . _ -)")
	displayName := fs.String("display-name", "", "name shown in the teacher console")
	roleRaw := fs.String("role", "", "ADMIN, TEACHER or STUDENT")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin (ADMIN/TEACHER only)")
	createdBy := fs.String("created-by", "", "optional: account of the admin performing this creation")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	role, err := user.ParseRole(*roleRaw)
	if err != nil {
		fmt.Fprintf(stderr, "invalid --role: %v\n", err)
		return exitUsage
	}
	if err := user.ValidateAccount(*account); err != nil {
		fmt.Fprintf(stderr, "invalid --account: %v\n", err)
		return exitUsage
	}
	name, err := user.ValidateDisplayName(*displayName)
	if err != nil {
		fmt.Fprintf(stderr, "invalid --display-name: %v\n", err)
		return exitUsage
	}

	// The role/password pairing is a business rule (§9). It is checked HERE, as a
	// usage error, so the operator gets a clear message instead of a database
	// constraint name; the CHECK constraint still enforces it as the last line of
	// defence.
	if role == user.RoleStudent && *passwordStdin {
		fmt.Fprintln(stderr, "invalid invocation: STUDENT accounts must not have a password (§2.2)")
		return exitUsage
	}
	if role != user.RoleStudent && !*passwordStdin {
		fmt.Fprintf(stderr, "invalid invocation: %s accounts require --password-stdin\n", role)
		return exitUsage
	}

	app, err := setup(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}
	defer app.db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	var hash *string
	if *passwordStdin {
		password, err := readPassword(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return exitFailure
		}
		policy := auth.NewPasswordPolicy(app.cfg.PasswordMinLength)
		if err := policy.Validate(*account, password); err != nil {
			// The policy error never contains the password: it is safe to print.
			fmt.Fprintf(stderr, "password rejected: %v\n", err)
			return exitFailure
		}
		encoded, err := auth.Hash(password)
		if err != nil {
			fmt.Fprintf(stderr, "hashing failed: %v\n", err)
			return exitFailure
		}
		hash = &encoded
	}

	params := user.CreateParams{
		Account:      *account,
		DisplayName:  name,
		Role:         role,
		PasswordHash: hash,
	}
	if *createdBy != "" {
		creator, err := app.users.FindByAccount(ctx, *createdBy)
		if err != nil {
			fmt.Fprintf(stderr, "creator %q not found: %v\n", *createdBy, err)
			return exitFailure
		}
		if creator.Role != user.RoleAdmin {
			fmt.Fprintf(stderr, "creator %q has role %s; only an ADMIN can be recorded as creator\n", *createdBy, creator.Role)
			return exitFailure
		}
		params.CreatedBy = &creator.ID
	}

	created, err := app.users.Create(ctx, params)
	if err != nil {
		if errors.Is(err, user.ErrAccountTaken) {
			fmt.Fprintf(stderr, "account %q already exists\n", *account)
			return exitFailure
		}
		fmt.Fprintf(stderr, "create user failed: %v\n", err)
		return exitFailure
	}

	// The log line and the confirmation carry identifiers only — never the
	// password, never a hash.
	app.logger.Info("user created",
		"user_id", created.ID.String(),
		"role", string(created.Role),
		"has_password", created.HasPassword(),
		"created_by", creatorIDString(created.CreatedBy),
	)
	fmt.Fprintf(stdout, "created %s account %q (id %s)\n", created.Role, created.Account, created.ID)
	return exitOK
}

func resetPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	account := fs.String("account", "", "login account whose password is replaced")
	passwordStdin := fs.Bool("password-stdin", false, "read the new password from the first line of stdin")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if strings.TrimSpace(*account) == "" || !*passwordStdin {
		fmt.Fprintln(stderr, "usage: adminctl reset-password --account X --password-stdin")
		return exitUsage
	}

	app, err := setup(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}
	defer app.db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	target, err := app.users.FindByAccount(ctx, *account)
	if err != nil {
		fmt.Fprintf(stderr, "account %q: %v\n", *account, err)
		return exitFailure
	}
	if !target.HasPassword() {
		// Writing a hash onto a student would violate users_password_by_role, and
		// silently refusing is friendlier than a constraint name.
		fmt.Fprintf(stderr, "account %q is a %s account and has no password to reset\n", target.Account, target.Role)
		return exitFailure
	}

	password, err := readPassword(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}
	policy := auth.NewPasswordPolicy(app.cfg.PasswordMinLength)
	if err := policy.Validate(target.Account, password); err != nil {
		fmt.Fprintf(stderr, "password rejected: %v\n", err)
		return exitFailure
	}
	encoded, err := auth.Hash(password)
	if err != nil {
		fmt.Fprintf(stderr, "hashing failed: %v\n", err)
		return exitFailure
	}
	if err := app.users.SetPasswordHash(ctx, target.ID, encoded); err != nil {
		fmt.Fprintf(stderr, "reset failed: %v\n", err)
		return exitFailure
	}

	// Changing a password must end every existing session: the old credential may
	// have been the reason the reset was needed (it leaked, or its owner left).
	// Leaving those sessions alive would make the reset cosmetic.
	if err := app.sessions.RevokeAllForUser(ctx, target.ID); err != nil {
		fmt.Fprintf(stderr, "password changed but revoking sessions failed: %v\n", err)
		return exitFailure
	}

	app.logger.Info("password reset",
		"user_id", target.ID.String(),
		"role", string(target.Role),
		"sessions_revoked", true,
	)
	fmt.Fprintf(stdout, "password updated for %q; all sessions revoked\n", target.Account)
	return exitOK
}

func listUsers(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list-users", flag.ContinueOnError)
	fs.SetOutput(stderr)
	roleRaw := fs.String("role", "", "optional role filter: ADMIN, TEACHER or STUDENT")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	var roleFilter *user.Role
	if strings.TrimSpace(*roleRaw) != "" {
		role, err := user.ParseRole(*roleRaw)
		if err != nil {
			fmt.Fprintf(stderr, "invalid --role: %v\n", err)
			return exitUsage
		}
		roleFilter = &role
	}

	app, err := setup(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}
	defer app.db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	users, err := app.users.List(ctx, roleFilter)
	if err != nil {
		fmt.Fprintf(stderr, "list users failed: %v\n", err)
		return exitFailure
	}

	// Tab-separated columns, aligned by tabwriter: readable for a human, still
	// parseable by awk. password_hash has no column here and cannot be added by
	// accident — it is not part of the printed set.
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tACCOUNT\tDISPLAY NAME\tROLE\tSTATUS\tCREATED\tLAST LOGIN")
	for _, u := range users {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			u.ID, u.Account, u.DisplayName, u.Role, u.Status,
			u.CreatedAt.UTC().Format(time.RFC3339),
			formatOptionalTime(u.LastLoginAt),
		)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "write output: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "\n%d account(s)\n", len(users))
	return exitOK
}

// readPassword reads the first line of stdin.
//
// Only the trailing newline is removed: leading or trailing spaces are legitimate
// parts of a passphrase, and silently trimming them would create a credential the
// user did not choose.
func readPassword(stdin io.Reader) (string, error) {
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("no password received on stdin (expected the first line; nothing is read from the terminal)")
	}
	return password, nil
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func creatorIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
