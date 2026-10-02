package main

import (
	"bytes"
	"strings"
	"testing"
)

// Usage errors must be distinguishable from failures: a deploy script branches on
// the exit code, and "you called me wrong" (2) is a different fix from "the
// operation failed" (1). Every case below is rejected before any connection is
// opened, so these tests need no database and no environment.

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"drop-everything"}},
		{"create-user without arguments", []string{"create-user"}},
		{"create-user with an unknown role", []string{"create-user", "--account", "teacher001", "--display-name", "T", "--role", "PRINCIPAL", "--password-stdin"}},
		{"create-user with an invalid account", []string{"create-user", "--account", "bad account", "--display-name", "T", "--role", "STUDENT"}},
		{"create-user with a blank display name", []string{"create-user", "--account", "teacher001", "--display-name", "   ", "--role", "STUDENT"}},
		// §9: the role/password pairing is checked as a usage error so the
		// operator gets a sentence instead of a constraint name.
		{"student with a password", []string{"create-user", "--account", "S10086", "--display-name", "S", "--role", "STUDENT", "--password-stdin"}},
		{"teacher without a password", []string{"create-user", "--account", "teacher001", "--display-name", "T", "--role", "TEACHER"}},
		{"admin without a password", []string{"create-user", "--account", "admin", "--display-name", "A", "--role", "ADMIN"}},
		{"create-user with a positional argument", []string{"create-user", "--account", "teacher001", "--display-name", "T", "--role", "STUDENT", "extra"}},
		{"reset-password without --password-stdin", []string{"reset-password", "--account", "teacher001"}},
		{"reset-password without an account", []string{"reset-password", "--password-stdin"}},
		{"list-users with an unknown role", []string{"list-users", "--role", "PRINCIPAL"}},
		{"list-users with a positional argument", []string{"list-users", "extra"}},
		{"unknown flag", []string{"create-user", "--nope"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args...)
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d (stderr=%s)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("usage errors must not write to stdout: %q", stdout)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Error("nothing was written to stderr; the operator has no idea what went wrong")
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, stdout, _ := runCLI(arg)
		if code != exitOK {
			t.Errorf("run(%q) exit code = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "create-user") {
			t.Errorf("run(%q) did not print the usage text", arg)
		}
	}
}

// TestUsageNeverDocumentsAPlaintextPasswordFlag guards the security decision: an
// argument form would be visible in the shell history and in `ps`.
func TestUsageNeverDocumentsAPlaintextPasswordFlag(t *testing.T) {
	_, stdout, _ := runCLI("help")
	if strings.Contains(stdout, "--password ") || strings.Contains(stdout, "--password=") {
		t.Errorf("usage text suggests a plaintext password flag:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--password-stdin") {
		t.Error("usage text does not document --password-stdin")
	}
}

func TestExitCodesAreDistinct(t *testing.T) {
	// The three constants are part of the tool's contract with scripts.
	if exitOK != 0 || exitFailure != 1 || exitUsage != 2 {
		t.Fatalf("exit codes = %d/%d/%d, want 0/1/2", exitOK, exitFailure, exitUsage)
	}
}
