package media

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/livekit/protocol/livekit"
)

// PeerSubscriptionRevocation is one subscription the Control Plane revoked: student
// ObserverIdentity must not be receiving the track TrackSid published by
// TrackOwnerIdentity (§26).
//
// It is returned instead of merely logged so the caller decides what to do with the
// fact. The media package knows nothing about classrooms, request ids or the project's
// log vocabulary, and the session layer already logs the observation it just made —
// keeping the two in one place is what makes a revocation findable next to the
// classroom it happened in (§59).
type PeerSubscriptionRevocation struct {
	// ObserverIdentity is the participant whose subscription was revoked. In this
	// project that is always a student's session id (§44).
	ObserverIdentity string
	// TrackOwnerIdentity is the participant that PUBLISHED the track the observer may
	// not receive. In Phase 7 it is always another student.
	TrackOwnerIdentity string
	// TrackSid is LiveKit's id of the track that was revoked.
	TrackSid string
}

// revokedSubscription is the bookkeeping key of one revocation that is already in
// effect. See Client.revoked for why it exists.
type revokedSubscription struct {
	// participantSid is the OBSERVER's connection id (ParticipantTracks.ParticipantSid),
	// not its identity: a student who reloads the page comes back with the same session
	// identity but a new connection, and a client that subscribes to its classmates on
	// connect must be revoked once per connection, not once per lesson. An empty value
	// (a media fake that does not report one) simply degrades to "once per lesson".
	participantSid string
	trackSid       string
}

// peerTrackRef is one (owner, track) pair a student must be unsubscribed from.
type peerTrackRef struct{ owner, sid string }

// EnforceNoPeerSubscriptions revokes every subscription the given students still hold
// to tracks published by anyone outside allowedTrackOwners (§26/§73).
//
// # Why this exists at all
//
// §28 requires canSubscribe=true on a student token, because the teacher's private
// audio (§31, Phase 10) has to be able to reach a student. That permission is room
// wide, so "students do not subscribe to each other" cannot be expressed as a token
// bit. A cooperative client is configured with autoSubscribe=false and subscribes to
// nothing; this call is the SERVER side of the same rule — it takes away what such a
// client should never have asked for, with an audit trail of what it asked for.
//
// # What this is NOT
//
// It is not protocol-level isolation, and nothing here may be described as such
// (§26 last paragraph): a deliberately modified client keeps canSubscribe=true and can
// re-subscribe the moment after this call returns. LiveKit's RoomService exposes no
// API that READS subscription state (checked against protocol v1.49: ParticipantInfo
// carries published tracks, never subscriptions), so this control plane cannot even
// observe a violation directly; it reconciles against publications instead. What this
// buys is (a) a cooperative-but-misconfigured client is corrected instead of left
// leaking a classmate's screen, and (b) every correction is a Warn line, so a client
// that keeps doing it is visible in operations.
//
// # What it does, precisely
//
// For every identity in students that is present in observed, every track published by
// a participant that is NOT in allowedTrackOwners and is not the student itself is
// revoked with one UpdateSubscriptions(Subscribe=false) per student. The request names
// the tracks by sid and groups them by owner, which is also the shape Phase 10 needs to
// keep a subset ("the teacher's microphone") instead of everything.
//
// observed is the caller's existing room observation, re-used on purpose: §26
// enforcement must not cost a second ListParticipants, so the poll that advances the
// session states is the same poll that reconciles subscriptions.
//
// allowedTrackOwners is the EXPLICIT whitelist of track owners a student may keep
// receiving. Phase 7 passes the participants of the room that are not students of the
// run — the teacher, whose token is the only other one this control plane mints — and
// the teacher publishes nothing yet, so in practice students are unsubscribed to
// nothing at all. The parameter is what makes that a decision instead of an omission:
// Phase 10 adds the teacher's microphone by narrowing it to (owner, source), and
// ObservedTrack.Source already carries the source it would filter on.
//
// # Idempotency, and why the caller sees a list of revocations
//
// Re-issuing the same UpdateSubscriptions every 10 seconds would be pure RPC noise, so
// a revocation is remembered per (room, observer connection, track) and issued once.
// The remembered set is pruned against the current observation, so a room whose tracks
// are gone costs nothing and leaks no memory. The returned slice contains exactly the
// revocations issued by THIS call; an empty slice means "nothing to do", which is the
// common case.
//
// # Failure discipline (§33)
//
// A failed UpdateSubscriptions is reported, never fatal, and never retried in a loop:
// the caller logs it and moves on, because the teacher's wall must still be served and
// a media-plane hiccup is not a business fact. Partial success is normal — each student
// is one independent RPC — so the error is joined across students and the revocations
// that did land are still returned.
func (c *Client) EnforceNoPeerSubscriptions(
	ctx context.Context,
	roomName string,
	students []string,
	observed map[string]ParticipantTracks,
	allowedTrackOwners []string,
) ([]PeerSubscriptionRevocation, error) {
	if c == nil || c.rooms == nil {
		return nil, fmt.Errorf("livekit: not connected")
	}
	if err := validateOpaqueRoomName(roomName); err != nil {
		return nil, err
	}

	allowed := make(map[string]struct{}, len(allowedTrackOwners))
	for _, owner := range allowedTrackOwners {
		allowed[owner] = struct{}{}
	}

	// The revocations already in effect for this room, read once. The same snapshot is
	// used for the decision ("is this new?") and for the pruning at the end, so a failed
	// call cannot be recorded as done by a concurrent observation.
	inEffect := c.revocationsForRoom(roomName)

	// Only students that are actually in the room can hold a subscription, and only in a
	// deterministic order: the caller passes a slice, but map iteration below is random
	// and a log line (or a test) must not depend on it.
	present := make([]string, 0, len(students))
	seen := make(map[string]struct{}, len(students))
	for _, student := range students {
		if student == "" {
			continue
		}
		if _, inRoom := observed[student]; !inRoom {
			continue
		}
		if _, duplicate := seen[student]; duplicate {
			continue
		}
		seen[student] = struct{}{}
		present = append(present, student)
	}
	sort.Strings(present)

	owners := make([]string, 0, len(observed))
	for owner := range observed {
		owners = append(owners, owner)
	}
	sort.Strings(owners)

	// plan is one RPC per student: every non-allowed peer track that student could be
	// receiving. visible is the whole candidate set, which is what the bookkeeping is
	// pruned against.
	plan := make(map[string][]peerTrackRef, len(present))
	visible := make(map[string]map[revokedSubscription]struct{}, len(present))
	for _, student := range present {
		connectionSid := observed[student].ParticipantSid
		visible[student] = map[revokedSubscription]struct{}{}
		for _, owner := range owners {
			if owner == student {
				// A participant cannot subscribe to itself, and a request that says so is
				// noise at best.
				continue
			}
			if _, whitelisted := allowed[owner]; whitelisted {
				continue
			}
			for _, track := range observed[owner].Tracks {
				if track.Sid == "" {
					continue
				}
				key := revokedSubscription{participantSid: connectionSid, trackSid: track.Sid}
				visible[student][key] = struct{}{}
				if _, already := inEffect[key]; already {
					// Issued on an earlier poll of this same connection: re-issuing it would
					// be the RPC noise this bookkeeping exists to avoid.
					continue
				}
				plan[student] = append(plan[student], peerTrackRef{owner: owner, sid: track.Sid})
			}
		}
	}

	revocations := make([]PeerSubscriptionRevocation, 0, len(plan))
	succeeded := make(map[string]map[revokedSubscription]struct{}, len(plan))
	var failures []error
	for _, student := range present {
		pending := plan[student]
		if len(pending) == 0 {
			continue
		}
		// Sorted so the request (and therefore the revocation list the caller logs) does
		// not depend on the order LiveKit happened to report the tracks in.
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].owner != pending[j].owner {
				return pending[i].owner < pending[j].owner
			}
			return pending[i].sid < pending[j].sid
		})
		request := updateSubscriptionsRequest(roomName, student, pending)
		if _, err := c.rooms.UpdateSubscriptions(ctx, request); err != nil {
			// One student's failure must not stop the others: each request is independent,
			// and the students after this one still deserve to be isolated.
			failures = append(failures, fmt.Errorf("livekit: revoke peer subscriptions for %s: %w", student, err))
			continue
		}
		succeeded[student] = map[revokedSubscription]struct{}{}
		connectionSid := observed[student].ParticipantSid
		for _, track := range pending {
			succeeded[student][revokedSubscription{participantSid: connectionSid, trackSid: track.sid}] = struct{}{}
			revocations = append(revocations, PeerSubscriptionRevocation{
				ObserverIdentity:   student,
				TrackOwnerIdentity: track.owner,
				TrackSid:           track.sid,
			})
		}
	}

	c.rememberRevocations(roomName, inEffect, visible, succeeded)
	return revocations, errors.Join(failures...)
}

// updateSubscriptionsRequest builds the UpdateSubscriptions call for one student.
//
// The tracks are named by sid in one flat list, which is the form the API accepts for
// "these tracks, this subscriber, no". Grouping them per owner is NOT used, and the
// reason is worth writing down: the per-owner form (ParticipantTracks.ParticipantSid)
// is keyed by the PUBLISHER's participant sid, which changes whenever that student
// reloads, so the revocation would be coupled to a connection id that has nothing to do
// with it. Phase 10's "keep the teacher's microphone" is expressed one level up, by
// leaving those sids out of this list, which needs no grouping either.
func updateSubscriptionsRequest(roomName, observer string, pending []peerTrackRef) *livekit.UpdateSubscriptionsRequest {
	sids := make([]string, 0, len(pending))
	for _, track := range pending {
		sids = append(sids, track.sid)
	}
	return &livekit.UpdateSubscriptionsRequest{
		Room:     roomName,
		Identity: observer,
		// This single boolean is the whole point of the call: the student must not
		// RECEIVE a classmate's media (§26).
		Subscribe: false,
		TrackSids: sids,
	}
}

// revocationsForRoom snapshots the revocations already in effect for one room.
func (c *Client) revocationsForRoom(roomName string) map[revokedSubscription]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(map[revokedSubscription]struct{}, len(c.revoked[roomName]))
	for key := range c.revoked[roomName] {
		snapshot[key] = struct{}{}
	}
	return snapshot
}

// rememberRevocations replaces the bookkeeping of one room with "what is still
// relevant": revocations that are still in effect AND still match a published track,
// plus the ones this pass actually issued.
//
// WHY a replacement and not an append: a track that is no longer published can never
// come back under the same sid, so keeping its key would only grow the map for the
// lifetime of the process.
//
// WHY a failed revocation is deliberately NOT remembered: the next poll retries it,
// which is the only self-healing this best-effort control has. That is also why the
// pruning is driven by the SNAPSHOT taken before the calls and not by the candidate
// set: a candidate that was never successfully revoked must stay outstanding.
func (c *Client) rememberRevocations(
	roomName string,
	inEffect map[revokedSubscription]struct{},
	visible map[string]map[revokedSubscription]struct{},
	succeeded map[string]map[revokedSubscription]struct{},
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.revoked == nil {
		c.revoked = make(map[string]map[revokedSubscription]struct{})
	}
	next := make(map[revokedSubscription]struct{})
	for _, keys := range visible {
		for key := range keys {
			if _, already := inEffect[key]; already {
				next[key] = struct{}{}
			}
		}
	}
	for _, keys := range succeeded {
		for key := range keys {
			next[key] = struct{}{}
		}
	}
	if len(next) == 0 {
		// A room with nothing to reconcile must not leave an empty entry behind: the map
		// is keyed by room name for the lifetime of the process.
		delete(c.revoked, roomName)
		return
	}
	c.revoked[roomName] = next
}
