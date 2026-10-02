package media

import (
	"fmt"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// PublishSource is one kind of media a token is allowed to publish.
//
// WHY our own type instead of livekit.TrackSource: the grant is a security decision
// that belongs to the Control Plane (§27/§28), and writing it in the vocabulary of
// the SDK would let an SDK enum change (or a new source being added upstream) silently
// widen what a role may publish. The mapping below is total for the three sources this
// project uses, and anything else is refused.
type PublishSource string

const (
	// PublishScreenShare is what a student publishes in Phase 6 (§21/§28).
	PublishScreenShare PublishSource = "SCREEN_SHARE"
	// PublishCamera and PublishMicrophone are not granted in Phase 6. They exist so
	// the roles of §27/§28 can be expressed as data when their phases arrive, and so
	// that "the student may not publish a camera yet" is a missing value rather than
	// a remembered omission.
	PublishCamera     PublishSource = "CAMERA"
	PublishMicrophone PublishSource = "MICROPHONE"
)

// TokenRequest is one participant-scoped media token.
//
// Every field is required to be set deliberately: there is no zero-value default that
// silently means "publish everything". A token with no grants at all is useless, and
// that is the correct outcome for a caller that forgot to think about permissions.
type TokenRequest struct {
	// Identity is the participant's identity in the room. It MUST be an opaque UUID
	// (§44) — see validateIdentity.
	Identity string
	// RoomName is the room the token is scoped to. It MUST be an `lk_...` room of
	// this project (§8) — a token is valid for exactly one room.
	RoomName string
	// TTL is how long the token stays valid. It is short-lived by policy (§63); the
	// configured value is validated at boot (see config.LiveKitTokenTTL).
	TTL time.Duration
	// CanPublish / CanSubscribe / CanPublishData are the room permission bits.
	//
	// CanPublishData is always false in this project: the data channel is not a
	// business channel (§47 routes business messages over a WebSocket, not over
	// WebRTC), and granting it would hand every participant a side channel the
	// control plane does not observe.
	CanPublish     bool
	CanSubscribe   bool
	CanPublishData bool
	// PublishSources is the exact set of track sources this token may publish. When
	// it is non-empty the LiveKit server uses it INSTEAD of CanPublish, which is what
	// makes "this student may share a screen and nothing else" enforceable in the
	// media plane rather than only in the client (§28).
	PublishSources []PublishSource
}

// SignToken mints a LiveKit join token.
//
// SECURITY. This is the only place a token is created, and it validates everything
// that could turn a convenience into a hole:
//
//   - identity must be a UUID. A name, an account or a phone number here would be
//     published to every participant of the room and to the LiveKit dashboard (§8/§26).
//   - room must be one of our opaque rooms. A token is otherwise valid for whatever
//     room name the caller invents, including somebody else's lesson.
//   - TTL must be positive and bounded. A non-expiring token would be a permanent
//     key to the media plane.
//   - every publish source must be one this project knows.
//
// The returned token is a credential: it must be sent to exactly the participant it
// was minted for and must never be logged (§59).
func (c *Client) SignToken(req TokenRequest) (string, error) {
	if c == nil || c.apiKey == "" || c.apiSecret == "" {
		return "", fmt.Errorf("livekit: not connected")
	}
	if !isOpaqueIdentity(req.Identity) {
		return "", fmt.Errorf("livekit: participant identity must be an opaque uuid")
	}
	if err := validateOpaqueRoomName(req.RoomName); err != nil {
		return "", err
	}
	if req.TTL <= 0 {
		return "", fmt.Errorf("livekit: token ttl must be greater than zero")
	}
	sources, err := trackSources(req.PublishSources)
	if err != nil {
		return "", err
	}

	canPublish := req.CanPublish
	canSubscribe := req.CanSubscribe
	canPublishData := req.CanPublishData
	token := auth.NewAccessToken(c.apiKey, c.apiSecret).
		SetIdentity(req.Identity).
		SetValidFor(req.TTL).
		SetVideoGrant(&auth.VideoGrant{
			RoomJoin: true,
			Room:     req.RoomName,
			// RoomCreate stays false: the room is created by the backend (§43), and a
			// token that could conjure a room would let a participant create media
			// rooms this control plane has never heard of.
			CanPublish:        &canPublish,
			CanSubscribe:      &canSubscribe,
			CanPublishData:    &canPublishData,
			CanPublishSources: sources,
		})
	jwt, err := token.ToJWT()
	if err != nil {
		// The SDK's error text never contains the secret, but the token does not exist
		// yet either way: nothing to redact here, and the caller must not log the value.
		return "", fmt.Errorf("livekit: sign token: %w", err)
	}
	return jwt, nil
}

// trackSources maps our source vocabulary onto LiveKit's, refusing anything unknown.
func trackSources(sources []PublishSource) ([]string, error) {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		switch source {
		case PublishScreenShare:
			out = append(out, sourceString(livekit.TrackSource_SCREEN_SHARE))
		case PublishCamera:
			out = append(out, sourceString(livekit.TrackSource_CAMERA))
		case PublishMicrophone:
			out = append(out, sourceString(livekit.TrackSource_MICROPHONE))
		default:
			return nil, fmt.Errorf("livekit: unknown publish source %q", string(source))
		}
	}
	return out, nil
}

// sourceString renders a track source the way the LiveKit grant format expects it:
// the lowercase enum name, e.g. "screen_share".
func sourceString(source livekit.TrackSource) string {
	return strings.ToLower(source.String())
}
