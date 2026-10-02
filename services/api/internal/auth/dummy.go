package auth

import "sync"

// Timing equalisation for password logins.
//
// WHY this exists: `LoginWithPassword` returns ErrInvalidCredentials both for
// "no such account" and for "wrong password", which closes the *content* channel
// of account enumeration. It would still leave a timing channel wide open — an
// unknown account returns in microseconds while a known one spends tens of
// milliseconds in Argon2id, so an attacker could enumerate accounts with a
// stopwatch. Verifying against a throwaway hash makes both paths pay the same
// cost.
//
// The dummy value is generated once per process and is not a credential: it
// belongs to no account, is never compared for equality anywhere, and knowing it
// grants nothing.
var dummyPasswordHash = sync.OnceValues(func() (string, error) {
	return Hash("classwatch-dummy-password-timing-equaliser")
})

// verifyDummyPassword performs one Argon2id verification whose result is
// discarded, so that the "unknown account" path costs the same as the
// "wrong password" path. Failures are ignored: this call exists for its duration.
func verifyDummyPassword(password string) {
	encoded, err := dummyPasswordHash()
	if err != nil {
		return
	}
	_, _, _ = Verify(encoded, password)
}
