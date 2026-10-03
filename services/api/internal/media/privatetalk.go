package media

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// This file is the MEDIA-PLANE half of §31: the rule that the teacher's microphone
// reaches exactly one student.
//
// # Why this cannot be a token permission
//
// §28 gives every student `canSubscribe=true`, because the teacher's private audio has to
// be able to reach one of them. The permission is room-wide, and LiveKit's join grant has
// no expression for "may subscribe to THIS publisher's microphone and to nothing else" —
// so the choice of WHO is a control-plane decision made after the fact, with
// RoomService.UpdateSubscriptions. That call is this file.
//
// # What the server is closing, precisely
//
// A cooperative client is configured with `autoSubscribe=false` (§26/§28) and subscribes
// to nothing on its own. But `canSubscribe=true` means a modified, or merely
// misconfigured, client can subscribe to the teacher's microphone at any moment. The
// authoritative set of who may hear the teacher therefore has to be enforced in the SFU:
//
//	teacher mic track ──► selected student   Subscribe=true   (§31: only this one)
//	                  └─► every other student Subscribe=false  (§31: never these)
//
// # Failure discipline (§33)
//
// A failed UpdateSubscriptions is reported, never fatal, and never retried in a loop: the
// caller logs it and decides. Partial success is normal — each student is one independent
// RPC — so the error is joined across students and the changes that did land are still
// returned. Unlike the §26 pass, the caller of EnforcePrivateTalk DOES act on the error
// when it is starting a talk: a teacher who is told "you are talking to 张三" while the
// media plane refused the subscription would be lied to (§31).

// PrivateTalkSubscription is one subscription change of one student to one track.
//
// It is returned (and logged) rather than applied silently, because every entry is a
// product-visible fact: a grant is "this student can hear the teacher now", a revocation
// is "this student cannot any more".
type PrivateTalkSubscription struct {
	// StudentIdentity is the student whose subscription changed, i.e. the observer. It is
	// a session id (an opaque UUID, §44), never a name.
	StudentIdentity string
	// TrackSid is LiveKit's id of the teacher's microphone publication the change is
	// about. A republished microphone gets a new sid, which is why a change is remembered
	// per sid and never per (participant, source) pair.
	TrackSid string
}

// PrivateTalkEnforcement is what one reconciliation pass did: the students it subscribed
// to the teacher's microphone, the students it unsubscribed, and the teacher microphone
// publications it reconciled against.
//
// TrackSids is deliberately part of the result: when it is empty the pass did nothing
// because there was no teacher microphone to reconcile, which is a different fact from
// "everybody was already correct" and is the one the caller reports as
// TEACHER_MIC_REQUIRED (§31).
type PrivateTalkEnforcement struct {
	Granted   []PrivateTalkSubscription
	Revoked   []PrivateTalkSubscription
	TrackSids []string
}

// EnforcePrivateTalk converges one room's private-talk subscriptions on `targetIdentity`.
//
// # The rule, in one sentence
//
// When the room has a target, that student is subscribed to every microphone track
// published by `teacherIdentities`, and every OTHER student of the room is unsubscribed
// from those same tracks. When targetIdentity is empty there is no target, so EVERY
// student is unsubscribed — that is the convergence rule of §31 as implemented for a
// process-local state machine: after a restart, or after the target went away, the media
// plane must end up with nobody subscribed to the teacher's microphone.
//
// # Why the caller passes the identities instead of the teacher being looked up here
//
// Two callers ask the same question from different knowledge:
//
//   - the private-talk endpoints (POST/DELETE) know the caller's LOGIN SESSION id, which
//     is that teacher's participant identity (§44) and the only one whose microphone may
//     currently exist;
//   - the monitor reconciliation (internal/session) has no teacher id at hand but HAS the
//     room observation, where the teacher is "the participant that is not a student of
//     this run" — the same derivation the §26 whitelist already uses.
//
// Passing the list keeps this function free of either lookup, which is what lets it be
// tested with no database and no classroom.
//
// # Why the grant is remembered per (connection, track) like a revocation
//
// The monitor polls every few seconds, so "the target is subscribed" would otherwise be
// one UpdateSubscriptions per poll for the whole lesson. The book of applied changes is
// keyed by (target's connection id, track sid) with the Subscribe flag as part of the key:
// a target who reloads gets a new connection and is granted again, and a target whose
// lesson moved on is revoked once, even though the earlier entry says "granted" — the two
// are different keys, so the switch cannot be swallowed by the memory.
func (c *Client) EnforcePrivateTalk(
	ctx context.Context,
	roomName string,
	students []string,
	observed map[string]ParticipantTracks,
	teacherIdentities []string,
	targetIdentity string,
) (PrivateTalkEnforcement, error) {
	if c == nil || c.rooms == nil {
		return PrivateTalkEnforcement{}, fmt.Errorf("livekit: not connected")
	}
	if err := validateOpaqueRoomName(roomName); err != nil {
		return PrivateTalkEnforcement{}, err
	}

	micSids := teacherMicrophoneTracks(observed, teacherIdentities)
	if len(micSids) == 0 {
		// Nothing to reconcile: a student cannot be subscribed to a track that does not
		// exist. The book of this room is dropped rather than kept, because every entry in
		// it names a sid the media plane no longer publishes — and if the teacher turns the
		// microphone back on, the new sid must produce a fresh grant (see the key comment).
		c.mu.Lock()
		c.talkApplied.forget(roomName)
		c.mu.Unlock()
		return PrivateTalkEnforcement{TrackSids: micSids}, nil
	}

	present := presentStudents(students, observed)

	// The changes this pass wants, per student, plus the candidate set the book is pruned
	// against. Both are built before any RPC so a failure cannot shrink the pruning set.
	want := make(map[string]bool, len(present))
	visible := make(map[talkSubscription]struct{}, len(present)*len(micSids))
	for _, student := range present {
		subscribe := student == targetIdentity
		want[student] = subscribe
		connectionSid := observed[student].ParticipantSid
		for _, sid := range micSids {
			visible[talkSubscription{participantSid: connectionSid, trackSid: sid, subscribe: subscribe}] = struct{}{}
		}
	}

	c.mu.Lock()
	inEffect := c.talkApplied.snapshot(roomName)
	c.mu.Unlock()

	// One RPC per student: all of the teacher's microphone publications share the same
	// Subscribe value for that student, so a per-track call would be noise.
	pending := make([]string, 0, len(present))
	for _, student := range present {
		connectionSid := observed[student].ParticipantSid
		already := true
		for _, sid := range micSids {
			key := talkSubscription{participantSid: connectionSid, trackSid: sid, subscribe: want[student]}
			if _, done := inEffect[key]; !done {
				already = false
				break
			}
		}
		if !already {
			pending = append(pending, student)
		}
	}

	// ORDER MATTERS on a switch (§31: "先撤销旧目标，再授权新目标"). Each
	// UpdateSubscriptions call is per-observer, so a switch is never one atomic operation:
	// issuing every revocation BEFORE the grant is what keeps the two listeners from
	// overlapping in the moment between the RPCs. The target is the only student whose
	// desired state is `subscribe=true`, so the ordering is "everybody else first".
	sort.Slice(pending, func(i, j int) bool {
		if want[pending[i]] != want[pending[j]] {
			return !want[pending[i]]
		}
		return pending[i] < pending[j]
	})

	enforcement := PrivateTalkEnforcement{TrackSids: micSids}
	succeeded := make(map[talkSubscription]struct{}, len(pending)*len(micSids))
	var failures []error
	for _, student := range pending {
		subscribe := want[student]
		if _, err := c.rooms.UpdateSubscriptions(ctx, updateSubscriptionsRequest(roomName, student, micSids, subscribe)); err != nil {
			// One student's failure must not stop the others: each request is independent,
			// and a switch that granted the new target must still revoke the old one.
			failures = append(failures, fmt.Errorf("livekit: set private talk subscription for %s: %w", student, err))
			continue
		}
		connectionSid := observed[student].ParticipantSid
		for _, sid := range micSids {
			succeeded[talkSubscription{participantSid: connectionSid, trackSid: sid, subscribe: subscribe}] = struct{}{}
			change := PrivateTalkSubscription{StudentIdentity: student, TrackSid: sid}
			if subscribe {
				enforcement.Granted = append(enforcement.Granted, change)
			} else {
				enforcement.Revoked = append(enforcement.Revoked, change)
			}
		}
	}

	c.mu.Lock()
	c.talkApplied.replace(roomName, inEffect, visible, succeeded)
	c.mu.Unlock()

	sortPrivateTalkSubscriptions(enforcement.Granted)
	sortPrivateTalkSubscriptions(enforcement.Revoked)
	return enforcement, errors.Join(failures...)
}

// TeacherMicrophoneTracks returns the microphone publications of one participant in a
// stable order; an empty result is "this participant has no microphone published".
//
// It exists so the private-talk state machine can answer §31's precondition ("has the
// teacher published a microphone?") from the SAME observation it already took, instead of
// asking the media plane a second time. It is exported because that question is asked
// outside this package (internal/session) and the answer must be derived with exactly the
// same rule as the enforcement below — two implementations would eventually disagree
// about which track counts as "the teacher's microphone".
func TeacherMicrophoneTracks(observed map[string]ParticipantTracks, identity string) []string {
	return teacherMicrophoneTracks(observed, []string{identity})
}

// teacherMicrophoneTracks collects the microphone publications of a set of participants.
func teacherMicrophoneTracks(observed map[string]ParticipantTracks, identities []string) []string {
	sids := make([]string, 0, len(identities))
	seen := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if identity == "" {
			continue
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		for _, track := range observed[identity].Tracks {
			// Source, not the Microphone boolean: the boolean says "media is flowing right
			// now" (a muted microphone is `false`), while a subscription is about a
			// PUBLICATION. A muted microphone the teacher is about to unmute is still the
			// track a student will hear, and §25 says the student may be told before that.
			if track.Source == PublishMicrophone && track.Sid != "" {
				sids = append(sids, track.Sid)
			}
		}
	}
	sort.Strings(sids)
	return sids
}

// presentStudents lists the students that are actually in the room, deduplicated and
// sorted.
//
// A student who is not in `observed` cannot hold a subscription, and asking LiveKit to
// update subscriptions for somebody who is gone can only produce a NotFound. The sort
// exists because map iteration is random and neither a request nor a log line may depend
// on it.
func presentStudents(students []string, observed map[string]ParticipantTracks) []string {
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
	return present
}

// sortPrivateTalkSubscriptions makes the reported changes deterministic.
func sortPrivateTalkSubscriptions(changes []PrivateTalkSubscription) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].StudentIdentity != changes[j].StudentIdentity {
			return changes[i].StudentIdentity < changes[j].StudentIdentity
		}
		return changes[i].TrackSid < changes[j].TrackSid
	})
}
