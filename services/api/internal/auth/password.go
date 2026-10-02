// Package auth implements authentication: password hashing, opaque session
// tokens and the session lifecycle that turns them into a Principal.
//
// Everything here assumes the Control Plane is the only authority. A request is
// authenticated because a row in `sessions` joins to an ACTIVE row in `users`,
// never because the client presented a claim about itself (§37/§41).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// WHY these values: Argon2id is memory-hard, so the cost that matters is the
// 64 MiB a single verification must touch — that is what makes GPU/ASIC cracking
// of a stolen `users` table expensive. t=3 and p=2 keep a login around a few tens
// of milliseconds on a small control-plane VM, which is imperceptible for a
// teacher logging in and ruinous for an attacker who must pay it per guess.
//
// They are constants, not configuration: a deployment that could lower them
// would eventually lower them to make a slow test pass, and the hashes in the
// database would silently become cheap. Changing a constant is a code review.
const (
	argon2MemoryKiB  = 64 * 1024 // 64 MiB
	argon2Time       = 3
	argon2Threads    = 2
	argon2SaltLength = 16
	argon2KeyLength  = 32
)

// Parsing guardrails. A stored hash is normally produced by Hash below, but a
// corrupted row, a hand-edited value or a future import must not be able to turn
// one login into an out-of-memory kill: the parameters are checked before any
// memory is allocated.
const (
	argon2MaxMemoryKiB = 1 << 20 // 1 GiB
	argon2MaxTime      = 10
	argon2MaxThreads   = 16
)

// Password policy limits.
const (
	// MaxPasswordLength bounds what a client may send. It exists to bound work,
	// not to be a rule about strong passwords: without it, a request could push
	// an arbitrarily large body through validation and hashing.
	MaxPasswordLength = 128
	// MinPasswordLengthFloor is the smallest minimum an operator may configure.
	MinPasswordLengthFloor = 8
	// DefaultPasswordMinLength matches PASSWORD_MIN_LENGTH's default.
	DefaultPasswordMinLength = 12
)

// Password sentinel errors.
//
// Their text never contains the password — not even its length or a prefix. These
// errors travel into HTTP responses, CLI output and log files, and a password that
// leaks once is compromised forever (§59).
var (
	ErrPasswordTooShort      = errors.New("auth: password is too short")
	ErrPasswordTooLong       = errors.New("auth: password is too long")
	ErrPasswordBlank         = errors.New("auth: password must not be blank")
	ErrPasswordEqualsAccount = errors.New("auth: password must not be the account name")
	// ErrPasswordHashFormat means the stored value is not a PHC Argon2id string.
	// It is a server-side data problem, never something the client caused, so it
	// must not be reported as "wrong password": the login would otherwise look
	// like a user error while every login for that account is broken.
	ErrPasswordHashFormat = errors.New("auth: stored password hash is not a valid argon2id PHC string")
)

// PasswordPolicy is the rule set applied to a new password.
type PasswordPolicy struct {
	MinLength int
}

// NewPasswordPolicy builds a policy, clamping an unusable minimum to the floor.
//
// config.Load already rejects out-of-range values, so this is a second line of
// defence for callers that construct a policy directly (tests, future admin API):
// a zero MinLength must never mean "no minimum".
func NewPasswordPolicy(minLength int) PasswordPolicy {
	if minLength < MinPasswordLengthFloor {
		minLength = DefaultPasswordMinLength
	}
	if minLength > MaxPasswordLength {
		minLength = MaxPasswordLength
	}
	return PasswordPolicy{MinLength: minLength}
}

// Validate applies the policy to a candidate password.
//
// The password itself is never echoed, logged or included in the returned error;
// only the rule that failed is. `account` is compared case-insensitively because
// accounts are citext — "S10086"/"s10086" is the same login, so it is the same
// guess.
func (p PasswordPolicy) Validate(account, password string) error {
	min := p.MinLength
	if min < MinPasswordLengthFloor {
		min = DefaultPasswordMinLength
	}
	if strings.TrimSpace(password) == "" {
		// A password of spaces is not a password, and it is also the shape a
		// hurried `--password-stdin </dev/null` produces. Rejecting it loudly
		// beats creating an account nobody can log into.
		return ErrPasswordBlank
	}
	// Length is measured in bytes: it is a bound on work, and counting runes
	// would let a CJK passphrase exceed the intended byte budget.
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: at most %d characters", ErrPasswordTooLong, MaxPasswordLength)
	}
	if len(password) < min {
		return fmt.Errorf("%w: at least %d characters", ErrPasswordTooShort, min)
	}
	if account != "" && strings.EqualFold(strings.TrimSpace(account), password) {
		return ErrPasswordEqualsAccount
	}
	return nil
}

// Hash derives a new Argon2id PHC string with a fresh random salt.
//
// The salt comes from crypto/rand on every call, so two accounts with the same
// password store different hashes and a rainbow table is useless. The returned
// string is the only form of the password that may ever be written down.
func Hash(password string) (string, error) {
	salt := make([]byte, argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		// A failing CSPRNG is not a condition to recover from by reusing a salt.
		return "", fmt.Errorf("auth: generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argon2Time, argon2MemoryKiB, argon2Threads, argon2KeyLength)
	return encodePHC(salt, key, argon2MemoryKiB, argon2Time, argon2Threads), nil
}

// Verify checks a password against a stored PHC string.
//
// Return contract:
//
//	ok          – the password matches
//	needsRehash – the hash is valid but was produced with stale parameters or a
//	              different Argon2 version; the caller should re-hash NOW, while
//	              the plaintext is legitimately in hand, and store the result
//	err         – the STORED value is not parseable, i.e. a data problem
//
// A wrong password is (false, false, nil): it is an expected outcome, not an
// error, and every caller must treat it exactly like an unknown account (§58).
func Verify(encoded, password string) (ok bool, needsRehash bool, err error) {
	params, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memoryKiB, params.threads, uint32(len(want)))
	// Constant-time comparison: a byte-by-byte `==` returns as soon as two bytes
	// differ, which leaks how much of a guessed hash was correct. The value being
	// compared is derived, but the habit is what keeps the next comparison safe.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}

	stale := params.version != argon2.Version ||
		params.memoryKiB != argon2MemoryKiB ||
		params.time != argon2Time ||
		params.threads != argon2Threads ||
		len(want) != argon2KeyLength
	return true, stale, nil
}

// argon2Params is the decoded parameter set of a PHC string.
type argon2Params struct {
	version   int
	memoryKiB uint32
	time      uint32
	threads   uint8
}

// encodePHC renders the standard Argon2 PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<b64salt>$<b64hash>
//
// The format is not invented here; it is what every Argon2 implementation reads,
// which is what allows an operator to verify a hash with an external tool when
// the API is the thing that is broken.
func encodePHC(salt, key []byte, memoryKiB, time uint32, threads uint8) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, time, threads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodePHC parses the subset of the PHC grammar this package produces and
// rejects everything else with an error instead of panicking.
func decodePHC(encoded string) (argon2Params, []byte, []byte, error) {
	var params argon2Params
	fail := func() (argon2Params, []byte, []byte, error) {
		// The offending value is deliberately NOT included: it is a password hash
		// and it ends up in logs.
		return argon2Params{}, nil, nil, ErrPasswordHashFormat
	}

	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return fail()
	}
	if parts[1] != "argon2id" {
		// argon2i/argon2d/scrypt/bcrypt are all rejected explicitly rather than
		// guessed at: accepting a weaker algorithm by accident is a downgrade.
		return fail()
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &params.version); err != nil {
		return fail()
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memoryKiB, &params.time, &params.threads); err != nil {
		return fail()
	}
	// Guardrails BEFORE decoding or deriving anything: these numbers decide how
	// much memory and CPU the verification below will consume.
	if params.version != argon2.Version {
		// A different version changes the algorithm, so we cannot verify it at
		// all; that is a data problem, not a wrong password.
		return fail()
	}
	if params.memoryKiB < 8 || params.memoryKiB > argon2MaxMemoryKiB ||
		params.time < 1 || params.time > argon2MaxTime ||
		params.threads < 1 || params.threads > argon2MaxThreads {
		return fail()
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return fail()
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return fail()
	}
	return params, salt, key, nil
}
