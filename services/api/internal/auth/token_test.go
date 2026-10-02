package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewSessionTokenShape(t *testing.T) {
	raw, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() failed: %v", err)
	}

	// 32 bytes of base64url without padding is exactly 43 characters. Anything
	// shorter means fewer than 256 bits of entropy, which is the entire security
	// argument for an opaque token.
	if len(raw) != 43 {
		t.Errorf("token length = %d, want 43", len(raw))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("token is not base64url without padding: %v", err)
	}
	if len(decoded) != sessionTokenBytes {
		t.Errorf("decoded token length = %d bytes, want %d", len(decoded), sessionTokenBytes)
	}
	// Characters that would need escaping in a cookie or a header are a practical
	// problem, not a theoretical one.
	if strings.ContainsAny(raw, "+/= ") {
		t.Errorf("token %q contains characters that are unsafe in a cookie", raw)
	}

	if !bytes.Equal(hash, HashToken(raw)) {
		t.Error("returned hash does not match HashToken(raw)")
	}
	if !bytes.Equal(hash, func() []byte { sum := sha256.Sum256([]byte(raw)); return sum[:] }()) {
		t.Error("hash is not SHA-256(raw)")
	}
	if strings.Contains(string(hash), raw) {
		t.Error("the stored hash contains the raw token")
	}
}

func TestNewSessionTokenIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		raw, _, err := NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken() failed: %v", err)
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("NewSessionToken() repeated a value after %d calls", i)
		}
		seen[raw] = struct{}{}
	}
}

func TestHashTokenIsStable(t *testing.T) {
	const raw = "a-fixed-token-value"
	first := HashToken(raw)
	second := HashToken(raw)
	if !bytes.Equal(first, second) {
		t.Fatal("HashToken is not deterministic; session lookup could never match")
	}
	if len(first) != sha256.Size {
		t.Errorf("hash length = %d, want %d", len(first), sha256.Size)
	}
	// Different inputs must not collide (this is a smoke test of the wiring, not
	// of SHA-256).
	if bytes.Equal(HashToken(raw), HashToken(raw+"x")) {
		t.Error("HashToken returned the same value for different inputs")
	}
}

func TestNewCSRFToken(t *testing.T) {
	first, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken() failed: %v", err)
	}
	second, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken() failed: %v", err)
	}
	if first == second {
		t.Error("two CSRF tokens are identical")
	}
	if len(first) != 43 {
		t.Errorf("csrf token length = %d, want 43", len(first))
	}
	if _, err := base64.RawURLEncoding.DecodeString(first); err != nil {
		t.Errorf("csrf token is not base64url: %v", err)
	}
}
