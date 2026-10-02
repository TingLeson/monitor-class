// Package media wraps the LiveKit server SDK.
//
// This package is the entire Media Plane boundary. Everything the Control Plane
// needs to know about LiveKit goes through it, which keeps the two planes
// separable: LiveKit moves media, PostgreSQL decides what is true (§33).
//
// A LiveKit room existing means only that some media infrastructure allocated a
// name. It never means "classroom is open", "this student may join" or "this
// student is being supervised" — those are rows in PostgreSQL, evaluated
// server-side on every request.
package media

import (
	"context"
	"fmt"
	"strings"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Client is a thin wrapper around the LiveKit RoomService API.
//
// Only room lifecycle calls are exposed in Phase 0. Participant administration
// and webhook handling arrive with the phases that actually need them.
type Client struct {
	rooms *lksdk.RoomServiceClient
}

// NewClient builds a RoomService client from the API URL, key and secret.
//
// apiURL is the server-to-server endpoint (http:// or https://, and ws:// is
// accepted). It is a different value from the browser-facing LIVEKIT_URL in
// Docker, where the API talks to http://livekit:7880 while browsers connect to
// ws://localhost:7880.
//
// SECURITY: apiSecret authenticates full room administration. It must exist only
// in the backend process — never in a frontend bundle, never in a mobile build,
// never in a log line, never in an API response (§44/§59). The browser only ever
// receives a short-lived, participant-scoped token minted server-side (Phase 6).
func NewClient(apiURL, apiKey, apiSecret string) (*Client, error) {
	if strings.TrimSpace(apiURL) == "" {
		return nil, fmt.Errorf("livekit: api url is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("livekit: api key is required")
	}
	if strings.TrimSpace(apiSecret) == "" {
		// Report presence, never the value.
		return nil, fmt.Errorf("livekit: api secret is required")
	}
	return &Client{rooms: lksdk.NewRoomServiceClient(apiURL, apiKey, apiSecret)}, nil
}

// HealthCheck verifies the LiveKit server is reachable by listing rooms.
//
// It is used by the readiness probe, so it must be cheap and must not mutate
// anything. The timeout belongs to the caller: readiness needs 2s, a boot check
// may want more.
func (c *Client) HealthCheck(ctx context.Context) error {
	if c == nil || c.rooms == nil {
		return fmt.Errorf("livekit: not connected")
	}
	if _, err := c.rooms.ListRooms(ctx, &livekit.ListRoomsRequest{}); err != nil {
		return fmt.Errorf("livekit: list rooms: %w", err)
	}
	return nil
}

// CreateRoom creates a room with an explicit, caller-supplied opaque name.
//
// The name is opaque by contract. Phase 3 will pass "lk_<classroom_run_uuid>",
// i.e. one room per OPEN period, and one run is never reused for a later lesson.
//
// NEVER use a student name, an account name, a real class name or any other
// human-meaningful string as a LiveKit room name or participant identity
// (§8/§26/§44). Room names and identities reach every participant's client and
// the LiveKit dashboard; a name there would tell a student exactly who else is
// being monitored. All media-layer identities are opaque UUIDs.
func (c *Client) CreateRoom(ctx context.Context, roomName string) error {
	if err := validateOpaqueRoomName(roomName); err != nil {
		return err
	}
	// EmptyRoomTimeout keeps an abandoned room from lingering forever: a classroom
	// closed while LiveKit was unreachable should not leave a room that a late
	// student could still connect to. Room state is bookkept in PostgreSQL, so
	// LiveKit expiring a room is safe.
	_, err := c.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:             roomName,
		EmptyTimeout:     emptyRoomTimeoutSeconds,
		DepartureTimeout: departureTimeoutSeconds,
		MaxParticipants:  maxParticipantsPerRoom,
	})
	if err != nil {
		return fmt.Errorf("livekit: create room: %w", err)
	}
	return nil
}

// TerminateRoom closes a room and disconnects everyone in it.
//
// WHY termination is driven by the Control Plane and not by LiveKit: when a
// teacher closes a classroom, the authoritative state change is the PostgreSQL
// transaction. Terminating the LiveKit room afterwards is cleanup that makes the
// media plane catch up with the database; if it fails, the database is still
// correct and the media plane is merely behind. Never the other way round.
func (c *Client) TerminateRoom(ctx context.Context, roomName string) error {
	if err := validateOpaqueRoomName(roomName); err != nil {
		return err
	}
	if _, err := c.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: roomName}); err != nil {
		return fmt.Errorf("livekit: delete room: %w", err)
	}
	return nil
}

// Room name and participant policy, named so the reasoning is greppable.
const (
	// emptyRoomTimeoutSeconds: a room with no participants is dropped after this
	// many seconds, so a crash between "create room" and "students join" cannot
	// leave a joinable room behind.
	emptyRoomTimeoutSeconds = 300
	// departureTimeoutSeconds: after the last participant leaves, wait a short
	// while before destroying the room. This lets a student who reloaded the page
	// rejoin the same run instead of creating a second media session.
	departureTimeoutSeconds = 20
	// maxParticipantsPerRoom is a safety bound, not an authorization rule: the
	// real gate is classroom assignment, checked in the Control Plane.
	maxParticipantsPerRoom = 200
)

// validateOpaqueRoomName rejects names that would leak human identity into the
// media plane, and empty names, which the LiveKit API would otherwise auto-fill.
func validateOpaqueRoomName(roomName string) error {
	if strings.TrimSpace(roomName) == "" {
		return fmt.Errorf("livekit: room name is required")
	}
	if roomName != strings.TrimSpace(roomName) {
		return fmt.Errorf("livekit: room name must not contain surrounding whitespace")
	}
	// The expected Phase 3 shape is lk_<uuid>. Enforcing a prefix now keeps a
	// future change from quietly switching to "class-3-math-zhangsan".
	if !strings.HasPrefix(roomName, roomNamePrefix) {
		return fmt.Errorf("livekit: room name must start with %q and stay opaque", roomNamePrefix)
	}
	return nil
}

// roomNamePrefix marks every ClassWatch media room so LiveKit-side tooling can
// tell our rooms apart from anything else sharing the deployment.
const roomNamePrefix = "lk_"

// NOTE on token issuance (Phase 6): minting LiveKit join tokens is deliberately
// NOT implemented here. A token's grants are the security-critical part — room
// name, participant identity, and only the publish/subscribe permissions the role
// requires — and shipping a half-configured issuer now would invite a later
// caller to reuse it with the wrong grants. It lands in Phase 6 together with
// session creation, so identity, permissions and expiry are decided in one place.
