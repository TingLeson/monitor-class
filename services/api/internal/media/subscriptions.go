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

// revokedSubscription is the bookkeeping key of one subscription change that is already
// in effect. See Client.revoked for why it exists, and Client.talkApplied for the second
// book of the same shape that Phase 10 keeps (§31).
type revokedSubscription struct {
	// participantSid is the OBSERVER's connection id (ParticipantTracks.ParticipantSid),
	// not its identity: a student who reloads the page comes back with the same session
	// identity but a new connection, and a client that subscribes to its classmates on
	// connect must be revoked once per connection, not once per lesson. An empty value
	// (a media fake that does not report one) simply degrades to "once per lesson".
	participantSid string
	trackSid       string
}

// subscriptionBook is the per-room memory of one kind of subscription change that is
// already in effect.
//
// WHY it is a generic type and not three copies of the same two maps in Client: Phase 7
// remembers peer REVOCATIONS and Phase 10 remembers private-talk GRANTS and REVOCATIONS,
// and all of them answer the same question — "was this exact change for this exact
// connection already issued?" — over their own key. One implementation means the pruning
// rule (a change whose track is gone must be forgotten) cannot drift between them.
//
// The key type differs on purpose: a peer key is (connection, track) because a revocation
// has only one direction, while a private-talk key is (connection, track, subscribe)
// because for one track a GRANT and a REVOCATION are different states — collapsing them
// would make a switch a no-op (the old entry would say "already handled").
//
// A book is not goroutine safe on its own: Client.mu guards every book it owns.
type subscriptionBook[K comparable] map[string]map[K]struct{}

// snapshot copies the changes already in effect for one room.
//
// WHY a copy and not the live map: the decision ("is this new?") and the pruning at the
// end of a pass must be taken against the SAME view, or a concurrent pass could record
// this one's work as already done.
func (b subscriptionBook[K]) snapshot(roomName string) map[K]struct{} {
	snapshot := make(map[K]struct{}, len(b[roomName]))
	for key := range b[roomName] {
		snapshot[key] = struct{}{}
	}
	return snapshot
}

// replace sets one room's book to "what is still relevant": the changes that were already
// in effect AND still match a live candidate, plus the ones this pass actually issued.
//
// WHY a replacement and not an append: a track that is no longer published can never come
// back under the same sid, so keeping its key would only grow the map for the lifetime of
// the process.
//
// WHY a failed change is deliberately NOT remembered: the next pass retries it, which is
// the only self-healing this best-effort control has. That is also why the pruning is
// driven by the SNAPSHOT taken before the calls and not by the candidate set: a candidate
// that was never successfully applied must stay outstanding.
func (b *subscriptionBook[K]) replace(
	roomName string,
	inEffect map[K]struct{},
	visible map[K]struct{},
	succeeded map[K]struct{},
) {
	next := make(map[K]struct{})
	for key := range visible {
		if _, already := inEffect[key]; already {
			next[key] = struct{}{}
		}
	}
	for key := range succeeded {
		next[key] = struct{}{}
	}
	if *b == nil {
		*b = subscriptionBook[K]{}
	}
	if len(next) == 0 {
		// A room with nothing to reconcile must not leave an empty entry behind: the map
		// is keyed by room name for the lifetime of the process.
		delete(*b, roomName)
		return
	}
	(*b)[roomName] = next
}

// forget drops one room's book entirely, which is what "there is nothing left to
// reconcile" means when the tracks themselves are gone.
func (b subscriptionBook[K]) forget(roomName string) { delete(b, roomName) }

// talkSubscription is the bookkeeping key of one private-talk change already in effect
// (§31). See subscriptionBook for why the subscribe flag is part of the key.
type talkSubscription struct {
	participantSid string
	trackSid       string
	subscribe      bool
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
// run — the teacher, whose token is the only other one this control plane mints.
//
// WHY the teacher stays whitelisted here AFTER Phase 10, when §31 says only ONE student
// may receive the teacher's microphone: this call is about §26 (students must not see
// each other), and it deliberately has no opinion about which of the teacher's tracks a
// student may keep. Phase 10 expresses the narrower rule in EnforcePrivateTalk, which
// reconciles exactly one source of exactly one publisher for the whole room. The two
// passes do not fight, because they name different tracks: this one never touches a
// track whose owner is whitelisted, and that one only ever touches the teacher's
// microphone. Keeping the owner-level whitelist here is also what keeps a future
// teacher track (a screen, if V1 ever allows one) from being revoked by the §26 rule
// that was never about the teacher.
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
	inEffect := c.snapshotRevocations(roomName)

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
		request := updateSubscriptionsRequest(roomName, student, peerSids(pending), false)
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

	c.replaceRevocations(roomName, inEffect, visible, succeeded)
	return revocations, errors.Join(failures...)
}

// peerSids lists the track sids of one student's pending peer revocations.
func peerSids(pending []peerTrackRef) []string {
	sids := make([]string, 0, len(pending))
	for _, track := range pending {
		sids = append(sids, track.sid)
	}
	return sids
}

// updateSubscriptionsRequest builds the UpdateSubscriptions call for one student.
//
// The tracks are named by sid in one flat list, which is the form the API accepts for
// "these tracks, this subscriber, yes/no". Grouping them per owner is NOT used, and the
// reason is worth writing down: the per-owner form (ParticipantTracks.ParticipantSid)
// is keyed by the PUBLISHER's participant sid, which changes whenever that student
// reloads, so the change would be coupled to a connection id that has nothing to do
// with it. Phase 10's "only the selected student keeps the teacher's microphone" is
// expressed one level up, by deciding the single `subscribe` boolean per call, which
// needs no grouping either.
//
// subscribe is the whole point of the call and is passed explicitly for that reason: a
// forgotten `false` here is the §26 leak, and a forgotten `true` is the §31 teacher who
// speaks into a room where nobody can hear them. Callers must name one or the other.
func updateSubscriptionsRequest(roomName, observer string, sids []string, subscribe bool) *livekit.UpdateSubscriptionsRequest {
	return &livekit.UpdateSubscriptionsRequest{
		Room:      roomName,
		Identity:  observer,
		Subscribe: subscribe,
		TrackSids: sids,
	}
}

// snapshotRevocations copies the peer revocations already in effect for one room.
func (c *Client) snapshotRevocations(roomName string) map[revokedSubscription]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revoked.snapshot(roomName)
}

// replaceRevocations rewrites the peer-revocation book of one room. See
// subscriptionBook.replace for the rule; this method only adds the lock.
func (c *Client) replaceRevocations(
	roomName string,
	inEffect map[revokedSubscription]struct{},
	visible map[string]map[revokedSubscription]struct{},
	succeeded map[string]map[revokedSubscription]struct{},
) {
	flatVisible := make(map[revokedSubscription]struct{})
	for _, keys := range visible {
		for key := range keys {
			flatVisible[key] = struct{}{}
		}
	}
	flatSucceeded := make(map[revokedSubscription]struct{})
	for _, keys := range succeeded {
		for key := range keys {
			flatSucceeded[key] = struct{}{}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked.replace(roomName, inEffect, flatVisible, flatSucceeded)
}
