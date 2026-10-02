package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Token sizes.
//
// The session token is 32 bytes (256 bits) of CSPRNG output: the search space is
// what makes an opaque token unguessable, since it carries no signature to verify
// and no claims to forge. The CSRF token is the same size for the same reason —
// it is a shared secret between the cookie jar and the session row.
const (
	sessionTokenBytes = 32
	csrfTokenBytes    = 32
)

// NewSessionToken mints a session token and its storage hash.
//
// The raw token is returned exactly once, to be written into the HttpOnly cookie.
// Only the SHA-256 goes into the database (§41): a database dump, a backup, a
// read-only replica or a log line that leaks `sessions` then contains nothing
// that can be replayed as a login. The token cannot be "recovered" server-side,
// which is the point — logging out destroys the only copy that mattered.
//
// base64url without padding is used because the value travels in a cookie and a
// header: it stays alphanumeric plus `-`/`_`, so no escaping, quoting or
// truncation surprise is possible. 32 bytes encode to exactly 43 characters.
func NewSessionToken() (raw string, hash []byte, err error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: generate session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// NewCSRFToken mints the per-session CSRF token.
//
// It is stored in the session row and mirrored into a non-HttpOnly cookie so the
// frontend can read it and echo it in X-CSRF-Token. That is safe precisely
// because it is not a credential: knowing it is useless without the session
// cookie, which is HttpOnly, and the pair is what a cross-site request cannot
// produce (§63).
func NewCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns SHA-256(raw) as the storage form of a session token.
//
// SHA-256 rather than Argon2 is deliberate: the input is 256 bits of random
// data, so there is no dictionary to try and no need for a slow KDF — the cost
// would only be paid on every authenticated request. Deterministic hashing is
// also what makes the token lookup a single indexed equality test.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
