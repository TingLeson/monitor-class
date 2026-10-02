package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// The tests in this file exercise the password path directly. Argon2id at the
// production parameters costs tens of milliseconds per call, so the table-driven
// cases stay small on purpose: a password test suite that takes a minute is a
// password test suite nobody runs before committing.

func TestHashVerifyRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("Hash() = %q, want an argon2id PHC string", encoded)
	}
	// The PHC header pins the parameters. If someone lowers them to make a test
	// faster, this fails loudly instead of silently weakening every stored hash.
	const wantPrefix = "$argon2id$v=19$m=65536,t=3,p=2$"
	if !strings.HasPrefix(encoded, wantPrefix) {
		t.Errorf("Hash() = %q, want prefix %q", encoded, wantPrefix)
	}
	if strings.Contains(encoded, password) {
		t.Fatal("Hash() output contains the plaintext password")
	}

	ok, needsRehash, err := Verify(encoded, password)
	if err != nil {
		t.Fatalf("Verify() failed: %v", err)
	}
	if !ok {
		t.Error("Verify() = false for the correct password")
	}
	if needsRehash {
		t.Error("Verify() asked for a rehash of a hash created with the current parameters")
	}
}

func TestHashIsSaltedPerCall(t *testing.T) {
	const password = "correct horse battery staple"

	first, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}
	second, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}
	// Identical hashes would mean a shared or missing salt, which turns a stolen
	// users table into a lookup table.
	if first == second {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	encoded, err := Hash("the-right-password")
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}

	ok, needsRehash, err := Verify(encoded, "the-wrong-password")
	if err != nil {
		// A wrong password is an expected outcome, not an error: the caller must
		// treat it exactly like an unknown account, and an error here would be
		// logged as a system fault.
		t.Fatalf("Verify() returned an error for a wrong password: %v", err)
	}
	if ok {
		t.Error("Verify() = true for a wrong password")
	}
	if needsRehash {
		t.Error("Verify() asked for a rehash after a failed verification")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	salt := make([]byte, argon2SaltLength)
	key := argon2.IDKey([]byte("pw"), salt, argon2Time, argon2MemoryKiB, argon2Threads, argon2KeyLength)
	valid := encodePHC(salt, key, argon2MemoryKiB, argon2Time, argon2Threads)

	cases := map[string]string{
		"empty":            "",
		"not PHC at all":   "hunter2",
		"bcrypt":           "$2y$10$abcdefghijklmnopqrstuv",
		"argon2i":          strings.Replace(valid, "$argon2id$", "$argon2i$", 1),
		"too few fields":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"bad base64 salt":  "$argon2id$v=19$m=65536,t=3,p=2$!!!!$" + strings.Split(valid, "$")[5],
		"empty hash":       "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
		"non numeric time": "$argon2id$v=19$m=65536,t=x,p=2$c2FsdA$aGFzaA",
		// A corrupted row must not be able to allocate a gigabyte per login.
		"absurd memory":   "$argon2id$v=19$m=99999999,t=3,p=2$c2FsdA$aGFzaA",
		"absurd threads":  "$argon2id$v=19$m=65536,t=3,p=200$c2FsdA$aGFzaA",
		"unknown version": strings.Replace(valid, "v=19", "v=16", 1),
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, needsRehash, err := Verify(encoded, "pw")
			if err == nil {
				t.Fatalf("Verify(%q) returned no error", encoded)
			}
			if !strings.Contains(err.Error(), ErrPasswordHashFormat.Error()) {
				t.Errorf("Verify() error = %v, want it to wrap ErrPasswordHashFormat", err)
			}
			if ok || needsRehash {
				t.Errorf("Verify() = (%v, %v), want (false, false) on a parse failure", ok, needsRehash)
			}
			// The malformed value is a hash; echoing it into a log or an error
			// message is exactly what §59 forbids.
			if encoded != "" && strings.Contains(err.Error(), encoded) {
				t.Errorf("Verify() error echoes the stored hash: %v", err)
			}
		})
	}
}

func TestVerifyNeedsRehashOnStaleParameters(t *testing.T) {
	// Simulate a hash written before the parameters were raised — the case that
	// makes a transparent upgrade possible at all, since the plaintext only exists
	// during a successful login.
	salt := make([]byte, argon2SaltLength)
	key := argon2.IDKey([]byte("pw"), salt, 1, 8*1024, 1, argon2KeyLength)
	stale := encodePHC(salt, key, 8*1024, 1, 1)

	ok, needsRehash, err := Verify(stale, "pw")
	if err != nil {
		t.Fatalf("Verify() failed: %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false for a correct password with stale parameters")
	}
	if !needsRehash {
		t.Error("Verify() did not ask for a rehash of a hash with stale parameters")
	}

	// And the upgraded hash must verify with the current parameters.
	upgraded, err := Hash("pw")
	if err != nil {
		t.Fatalf("Hash() failed: %v", err)
	}
	ok, needsRehash, err = Verify(upgraded, "pw")
	if err != nil || !ok || needsRehash {
		t.Errorf("Verify(upgraded) = (%v, %v, %v), want (true, false, nil)", ok, needsRehash, err)
	}
}

func TestPasswordPolicy(t *testing.T) {
	policy := NewPasswordPolicy(12)

	cases := []struct {
		name     string
		account  string
		password string
		wantErr  error
	}{
		{"valid", "teacher001", "a-long-enough-passphrase", nil},
		{"exactly the minimum", "teacher001", strings.Repeat("x", 12), nil},
		{"too short", "teacher001", strings.Repeat("x", 11), ErrPasswordTooShort},
		{"empty", "teacher001", "", ErrPasswordBlank},
		{"only whitespace", "teacher001", "            ", ErrPasswordBlank},
		{"too long", "teacher001", strings.Repeat("x", MaxPasswordLength+1), ErrPasswordTooLong},
		// The account is part of the guess, so a password equal to it is rejected
		// even when it satisfies the length rule.
		{"equals the account", "classwatch-teacher", "classwatch-teacher", ErrPasswordEqualsAccount},
		{"equals the account in another case", "ClassWatch-Teacher", "classwatch-teacher", ErrPasswordEqualsAccount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.Validate(tc.account, tc.password)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want %v", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr.Error()) {
				t.Errorf("Validate() = %v, want it to wrap %v", err, tc.wantErr)
			}
			// The message is shown to an operator and written to a log. It may
			// state the rule; it may never quote the password (§59).
			if tc.password != "" && strings.Contains(err.Error(), tc.password) {
				t.Errorf("policy error leaks the password: %v", err)
			}
		})
	}
}

func TestPasswordPolicyFallsBackToADefaultMinimum(t *testing.T) {
	// A zero-value policy must not mean "no minimum": config.Load validates the
	// configured value, but a policy built directly (tests, a future admin API)
	// still has to protect the account.
	policy := PasswordPolicy{}
	if err := policy.Validate("teacher001", "short"); err == nil {
		t.Error("zero-value policy accepted a 5-character password")
	}
	if err := policy.Validate("teacher001", strings.Repeat("x", DefaultPasswordMinLength)); err != nil {
		t.Errorf("zero-value policy rejected a %d-character password: %v", DefaultPasswordMinLength, err)
	}
}

func TestHashErrorDoesNotContainPassword(t *testing.T) {
	// Hash only fails when the CSPRNG fails, which cannot be forced here; the
	// property under test is the signature-level promise that the password never
	// travels inside an error value. This guards a future refactor that adds
	// wrapping with the input.
	encoded, err := Hash("a-very-secret-password-value")
	if err != nil {
		if strings.Contains(err.Error(), "a-very-secret-password-value") {
			t.Fatalf("Hash() error leaks the password: %v", err)
		}
		return
	}
	if strings.Contains(encoded, "a-very-secret-password-value") {
		t.Fatal("stored hash contains the plaintext")
	}
}
