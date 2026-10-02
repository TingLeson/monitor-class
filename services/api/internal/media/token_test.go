package media

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/livekit/protocol/livekit"
)

// The token grants are the security-critical part of Phase 6 (§27/§28/§44), and they
// are the reason this package exists rather than the call sites minting their own
// JWTs. These tests decode what was actually signed, so a permission that quietly
// widens (a source added "for convenience", canPublishData left to the SDK's default)
// fails here instead of in production.
//
// No network is involved: SignToken is pure signing.

const (
	testKey    = "test-api-key"
	testSecret = "test-api-secret-value"
	testTTL    = 2 * time.Hour
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient("http://127.0.0.1:7880", testKey, testSecret)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	return client
}

// decodeClaims reads the JWT payload without verifying it (the signature is checked
// by LiveKit, not here) and returns the grants.
func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want a JWT (header.payload.signature)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

// TestSignTokenStudentGrants pins §28: a student token joins the room, publishes only
// a screen share, subscribes (for the teacher's private audio) and never uses the data
// channel.
func TestSignTokenStudentGrants(t *testing.T) {
	client := newTestClient(t)
	identity := "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f"

	token, err := client.SignToken(TokenRequest{
		Identity:       identity,
		RoomName:       "lk_2c81a3e0-1111-2222-3333-444455556666",
		TTL:            testTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		CanPublishData: false,
		PublishSources: []PublishSource{PublishScreenShare},
	})
	if err != nil {
		t.Fatalf("SignToken(): %v", err)
	}

	claims := decodeClaims(t, token)
	if claims["sub"] != identity {
		t.Errorf("sub = %v, want the opaque identity %q", claims["sub"], identity)
	}
	video, ok := claims["video"].(map[string]any)
	if !ok {
		t.Fatalf("claims have no video grant: %v", claims)
	}
	if video["room"] != "lk_2c81a3e0-1111-2222-3333-444455556666" {
		t.Errorf("room = %v, want the run's room", video["room"])
	}
	if video["roomJoin"] != true {
		t.Errorf("roomJoin = %v, want true", video["roomJoin"])
	}
	// RoomCreate must stay absent: the backend creates rooms (§43), and a token that
	// could conjure one would let a participant create media rooms this control plane
	// has never heard of.
	if video["roomCreate"] == true {
		t.Error("roomCreate = true, want absent: rooms are created by the backend, not by tokens")
	}
	if video["canPublish"] != true {
		t.Errorf("canPublish = %v, want true", video["canPublish"])
	}
	if video["canSubscribe"] != true {
		t.Errorf("canSubscribe = %v, want true (§28)", video["canSubscribe"])
	}
	if video["canPublishData"] != false {
		t.Errorf("canPublishData = %v, want false (§47)", video["canPublishData"])
	}
	sources, ok := video["canPublishSources"].([]any)
	if !ok {
		t.Fatalf("canPublishSources = %v, want a list", video["canPublishSources"])
	}
	if len(sources) != 1 || sources[0] != "screen_share" {
		t.Errorf("canPublishSources = %v, want [screen_share] only", sources)
	}

	// The TTL is what §63 calls "short-lived": assert the signed window, not the code
	// path that produced it.
	issuedAt, expiresAt := numericClaim(t, claims, "iat"), numericClaim(t, claims, "exp")
	if got := time.Duration(expiresAt-issuedAt) * time.Second; got != testTTL {
		t.Errorf("token lifetime = %s, want %s", got, testTTL)
	}
	if claims["iss"] != testKey {
		t.Errorf("iss = %v, want the API key", claims["iss"])
	}
}

// TestSignTokenTeacherGrants pins §27: a teacher token may subscribe and publish only
// a microphone.
func TestSignTokenTeacherGrants(t *testing.T) {
	client := newTestClient(t)

	token, err := client.SignToken(TokenRequest{
		Identity:       "11111111-2222-3333-4444-555555555555",
		RoomName:       "lk_2c81a3e0-1111-2222-3333-444455556666",
		TTL:            testTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		CanPublishData: false,
		PublishSources: []PublishSource{PublishMicrophone},
	})
	if err != nil {
		t.Fatalf("SignToken(): %v", err)
	}

	video := decodeClaims(t, token)["video"].(map[string]any)
	sources := video["canPublishSources"].([]any)
	if len(sources) != 1 || sources[0] != "microphone" {
		t.Errorf("canPublishSources = %v, want [microphone] only (§27)", sources)
	}
	if video["canSubscribe"] != true {
		t.Errorf("canSubscribe = %v, want true (§27: the teacher watches the students)", video["canSubscribe"])
	}
}

// TestSignTokenLeaksNothing: the secret must not be recoverable from the token, and
// the token must not carry a name, an account or any other human identifier (§8/§44).
func TestSignTokenLeaksNothing(t *testing.T) {
	client := newTestClient(t)
	token, err := client.SignToken(TokenRequest{
		Identity:       "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f",
		RoomName:       "lk_2c81a3e0-1111-2222-3333-444455556666",
		TTL:            testTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		PublishSources: []PublishSource{PublishScreenShare},
	})
	if err != nil {
		t.Fatalf("SignToken(): %v", err)
	}
	if strings.Contains(token, testSecret) {
		t.Fatal("the API secret appears verbatim inside the token")
	}
	if strings.Contains(token, "classwatch") || strings.Contains(token, "@") {
		t.Errorf("the token carries an unexpected human-readable value: %s", token)
	}
}

// TestSignTokenValidation covers every input this package refuses. Each of them is a
// §44/§63 rule, and each would otherwise be a hole: a name as an identity, a token for
// a room of another lesson, a token that never expires, or a source nobody defined.
func TestSignTokenValidation(t *testing.T) {
	client := newTestClient(t)
	valid := TokenRequest{
		Identity:       "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f",
		RoomName:       "lk_2c81a3e0-1111-2222-3333-444455556666",
		TTL:            testTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		PublishSources: []PublishSource{PublishScreenShare},
	}

	tests := []struct {
		name   string
		mutate func(*TokenRequest)
	}{
		{name: "identity is a name", mutate: func(r *TokenRequest) { r.Identity = "zhangsan" }},
		{name: "identity is an account", mutate: func(r *TokenRequest) { r.Identity = "S10086" }},
		{name: "identity is empty", mutate: func(r *TokenRequest) { r.Identity = "" }},
		{name: "room is not ours", mutate: func(r *TokenRequest) { r.RoomName = "class-3-math" }},
		{name: "room is empty", mutate: func(r *TokenRequest) { r.RoomName = "" }},
		{name: "ttl is zero", mutate: func(r *TokenRequest) { r.TTL = 0 }},
		{name: "ttl is negative", mutate: func(r *TokenRequest) { r.TTL = -time.Minute }},
		{name: "unknown publish source", mutate: func(r *TokenRequest) { r.PublishSources = []PublishSource{"SCREENSHOT"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			if _, err := client.SignToken(req); err == nil {
				t.Fatal("SignToken() succeeded, want a refusal")
			}
		})
	}
}

// TestSignTokenWithoutCredentials: a client that was never configured must not be able
// to sign anything.
func TestSignTokenWithoutCredentials(t *testing.T) {
	client := &Client{}
	if _, err := client.SignToken(TokenRequest{Identity: "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f", RoomName: "lk_x", TTL: time.Hour}); err == nil {
		t.Fatal("SignToken() on an unconfigured client succeeded")
	}
}

// TestSignTokenVerifiesWithTheSecret: the token must be verifiable with the shared
// secret, so a mistake in the signing key would be caught here rather than by LiveKit
// rejecting every participant.
func TestSignTokenVerifiesWithTheSecret(t *testing.T) {
	client := newTestClient(t)
	token, err := client.SignToken(TokenRequest{
		Identity:       "6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f",
		RoomName:       "lk_2c81a3e0-1111-2222-3333-444455556666",
		TTL:            testTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		PublishSources: []PublishSource{PublishScreenShare},
	})
	if err != nil {
		t.Fatalf("SignToken(): %v", err)
	}
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return []byte(testSecret), nil })
	if err != nil || !parsed.Valid {
		t.Fatalf("token does not verify with the API secret: %v", err)
	}
}

// TestIdentityAndRoomValidation covers the two guards directly: they are what keeps a
// human-meaningful string out of the media plane (§8/§44).
func TestIdentityAndRoomValidation(t *testing.T) {
	for _, identity := range []string{"zhangsan", "student001", "13800138000", "", "not-a-uuid"} {
		if isOpaqueIdentity(identity) {
			t.Errorf("isOpaqueIdentity(%q) = true, want false", identity)
		}
	}
	if !isOpaqueIdentity("6c1e6a6a-4a5a-4a5a-8a5a-1a2b3c4d5e6f") {
		t.Error("a UUID was rejected as a participant identity")
	}

	for _, room := range []string{"", " class-3-math", "lk_", "math-3"} {
		if err := validateOpaqueRoomName(room); err == nil {
			t.Errorf("validateOpaqueRoomName(%q) = nil, want a refusal", room)
		}
	}
	if err := validateOpaqueRoomName("lk_2c81a3e0-1111-2222-3333-444455556666"); err != nil {
		t.Errorf("a valid room name was refused: %v", err)
	}
}

// TestIsConnectedState pins which LiveKit participant states count as "in the room".
// A disconnected participant that is still listed must not keep a student ONLINE.
func TestIsConnectedState(t *testing.T) {
	tests := map[string]struct {
		state livekit.ParticipantInfo_State
		want  bool
	}{
		"joining":      {state: livekit.ParticipantInfo_JOINING, want: true},
		"joined":       {state: livekit.ParticipantInfo_JOINED, want: true},
		"active":       {state: livekit.ParticipantInfo_ACTIVE, want: true},
		"disconnected": {state: livekit.ParticipantInfo_DISCONNECTED, want: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isConnectedState(tc.state); got != tc.want {
				t.Errorf("isConnectedState(%v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// numericClaim reads a JWT numeric date claim as seconds since the epoch.
func numericClaim(t *testing.T, claims map[string]any, name string) int64 {
	t.Helper()
	value, ok := claims[name].(float64)
	if !ok {
		t.Fatalf("claim %q = %v, want a number", name, claims[name])
	}
	return int64(value)
}
