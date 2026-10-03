package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// This file is the private-talk state machine of §31: IDLE → TALKING(student) → IDLE,
// with exactly one target at a time, implemented as a server-side decision about WHO may
// receive the teacher's microphone.
//
// # The three layers, and why all three are needed
//
//  1. STATE (this file). One target per classroom, remembered for the life of the process.
//     See the note on process-locality below.
//  2. MEDIA (media.EnforcePrivateTalk). The SFU is told that the target subscribes to the
//     teacher's microphone and everybody else does not. §28 gives every student
//     `canSubscribe=true`, so this is the only layer that can actually take the audio
//     away from a student who asked for it.
//  3. EVENTS (§13). TEACHER_TALK_STARTED / TEACHER_TALK_ENDED are written on the TARGET's
//     session, because the question a lesson report asks is "was this student spoken to
//     privately, and when?" — a property of the student's session, not of the classroom.
//
// # Why the state is process-local in Phase 10, and what that costs
//
// The registry below is an in-memory map, matching the single-instance assumption the
// WebSocket hub already makes (§52). A database table was the alternative and it was
// rejected deliberately: the row would have to be written in the same transaction as the
// media-plane call, and the media plane cannot participate in a transaction — so the row
// would be a cache with a TTL nobody can state. A process-local value is honest about what
// it is, and the safety property that matters is not "the selection survives a restart"
// but "a restart cannot leave a ghost SUBSCRIPTION". That is guaranteed from the other
// side: the monitor poll reconciles the media plane to the current target, and with no
// target every student is unsubscribed (Service.enforcePrivateTalk). A restart therefore
// loses the teacher's selection — the teacher presses the button again — and leaks nothing.
//
// Phase 11/12 replaces this map with Redis for the multi-instance deployment (§77/§78);
// the seam is exactly this type, and the media reconciliation stays as it is.

// Private talk errors, mapped to the API error codes of §58 in internal/httpapi.
var (
	// ErrTeacherMicRequired means the teacher asked to talk to a student without having
	// published a microphone (§31). It is a REFUSAL and never a silent success: a teacher
	// who is told "you are talking to 张三" while nothing is published would speak into a
	// room where nobody can hear them.
	ErrTeacherMicRequired = errors.New("session: the teacher has not published a microphone")

	// ErrPrivateTalkUnavailable means the named student cannot be talked to right now:
	// no session in this run, a session that is over, or a participant who is not in the
	// media room.
	ErrPrivateTalkUnavailable = errors.New("session: the student is not available for a private talk")
)

// The reasons a talk ends, recorded in the audit payload so a human reading the event log
// months later can tell "the teacher chose somebody else" from "the student's laptop
// dropped" (§13).
const (
	TalkEndReasonStopped      = "STOPPED"
	TalkEndReasonSwitched     = "SWITCHED"
	TalkEndReasonDisconnected = "DISCONNECTED"
	TalkEndReasonLeft         = "LEFT"
	TalkEndReasonRoomClosed   = "ROOM_CLOSED"
)

// PrivateTalkTarget is who the teacher is talking to, as the API returns it.
type PrivateTalkTarget struct {
	StudentID uuid.UUID
	// SessionID is the target's student session in the current run. It is also the media
	// identity (§44), which is how the frontend finds the participant to listen to.
	SessionID uuid.UUID
	// DisplayName comes from the classroom roster, never from the media plane (§51).
	DisplayName string
}

// PrivateTalkView is the whole response of the private-talk read: a target, or none.
//
// A nil Target is the IDLE state of §31 and is a real, renderable answer ("nobody is
// being talked to"), not missing data — the same discipline as the null tile of §29.
type PrivateTalkView struct {
	Target *PrivateTalkTarget
}

// StartPrivateTalkInput is the request of StartPrivateTalk.
type StartPrivateTalkInput struct {
	ClassroomID uuid.UUID
	// TeacherID is the authenticated owner. Ownership is checked here, server-side (§37).
	TeacherID uuid.UUID
	// TeacherSessionID is the caller's LOGIN session id, which is also their participant
	// identity in the media room (§44). It is needed to find "the teacher's microphone".
	TeacherSessionID uuid.UUID
	// TeacherDisplayName is what the target student is told ("王老师希望与你进行语音沟通",
	// §25). It comes from the authenticated principal and never from the request body.
	TeacherDisplayName string
	// StudentID is the chosen student. It must be on this classroom's roster.
	StudentID uuid.UUID
}

// StopPrivateTalkInput is the request of StopPrivateTalk.
type StopPrivateTalkInput struct {
	ClassroomID      uuid.UUID
	TeacherID        uuid.UUID
	TeacherSessionID uuid.UUID
}

// PrivateTalkEnder is what the runtime event path needs when a talk must end because the
// TARGET went away (a webhook, the leave endpoint) or because the lesson ended — rather
// than because the teacher pressed the button.
//
// It exists as a port so internal/session's Processor can end a talk without holding a
// media client: the state machine (and the revocation it implies) lives in Service, and
// the processor only reports the fact. Both methods are idempotent and succeed silently
// when there is nothing to end.
type PrivateTalkEnder interface {
	// EndPrivateTalkForSession ends a talk whose TARGET is this session, if there is one.
	EndPrivateTalkForSession(ctx context.Context, sessionID uuid.UUID, reason string) error
	// EndPrivateTalkForRun ends the talk of the classroom this run belongs to, if there is
	// one.
	EndPrivateTalkForRun(ctx context.Context, runID uuid.UUID, reason string) error
}

// privateTalkTarget is the registry's record of one active talk.
//
// It carries the run and the teacher's participant identity because the paths that END a
// talk are not the teacher's request: a webhook knows a session id, a close knows a run
// id, and neither can look up "which microphone was this student subscribed to?".
// Re-deriving those at the start is what makes revocation possible later.
type privateTalkTarget struct {
	StudentID   uuid.UUID
	SessionID   uuid.UUID
	DisplayName string
	RunID       uuid.UUID
	ClassroomID uuid.UUID
	// TeacherSessionID is the participant identity the talk was started with.
	TeacherSessionID uuid.UUID
}

// privateTalkRegistry is the process-local map of "which classroom is talking to whom".
//
// One entry per classroom, and the entry's existence IS the §31 invariant ("老师一次只允许
// 选择一个 Private Talk Target"): a second target for the same classroom replaces the
// first instead of being added next to it.
type privateTalkRegistry struct {
	mu      sync.Mutex
	targets map[uuid.UUID]privateTalkTarget
}

func newPrivateTalkRegistry() *privateTalkRegistry {
	return &privateTalkRegistry{targets: map[uuid.UUID]privateTalkTarget{}}
}

// get returns the classroom's current target.
func (r *privateTalkRegistry) get(classroomID uuid.UUID) (privateTalkTarget, bool) {
	if r == nil {
		return privateTalkTarget{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target, ok := r.targets[classroomID]
	return target, ok
}

// set installs a target, returning the one it replaced.
func (r *privateTalkRegistry) set(classroomID uuid.UUID, target privateTalkTarget) (privateTalkTarget, bool) {
	if r == nil {
		return privateTalkTarget{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, had := r.targets[classroomID]
	r.targets[classroomID] = target
	return previous, had
}

// clear removes a classroom's target, returning the one it removed.
func (r *privateTalkRegistry) clear(classroomID uuid.UUID) (privateTalkTarget, bool) {
	if r == nil {
		return privateTalkTarget{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, had := r.targets[classroomID]
	if had {
		delete(r.targets, classroomID)
	}
	return previous, had
}

// clearSession removes the target whose SESSION is this one, reporting the record it
// removed. It exists for the webhook/leave paths, which know a session and nothing about
// which classroom was talking to it.
func (r *privateTalkRegistry) clearSession(sessionID uuid.UUID) (privateTalkTarget, bool) {
	if r == nil || sessionID == uuid.Nil {
		return privateTalkTarget{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for classroomID, target := range r.targets {
		if target.SessionID == sessionID {
			delete(r.targets, classroomID)
			return target, true
		}
	}
	return privateTalkTarget{}, false
}

// snapshot copies every active target. It is what a caller that does not know a classroom
// id iterates — today only the run-level ender, which is handed a run and has to find the
// talk (at most one) that belongs to it.
func (r *privateTalkRegistry) snapshot() []privateTalkTarget {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets := make([]privateTalkTarget, 0, len(r.targets))
	for _, target := range r.targets {
		targets = append(targets, target)
	}
	return targets
}

// StartPrivateTalk begins (or switches, or re-confirms) a private talk (§31).
//
// # The four semantics this method implements, each of which is a test
//
//   - IDLE → TALKING: the target is granted the teacher's microphone, everybody else is
//     revoked, TEACHER_TALK_STARTED is written on the target's session, and the target and
//     the teacher are told.
//   - TALKING(张三) + POST 张三: idempotent success. Nothing is written and nothing is
//     broadcast; the media reconciliation still runs, because it is a no-op when the state
//     is already correct and a repair when it is not.
//   - TALKING(张三) + POST 李四: a SWITCH. §31 spells the path out as "张三 → 结束 → 李四",
//     so it is two events (ENDED on 张三, STARTED on 李四) and ONE media pass in which 李四
//     is granted and 张三 is revoked. There is never a moment in which both receive the
//     teacher's microphone, because the two students are one `target` argument.
//   - anything + a missing precondition: a refusal with a code the teacher can act on
//     (TEACHER_MIC_REQUIRED, PRIVATE_TALK_UNAVAILABLE, CLASSROOM_CLOSED, …), never a
//     silent success.
//
// # Why the media pass runs BEFORE the state is committed
//
// The state ("the teacher is talking to 张三") is a promise about what the media plane is
// doing. If the grant cannot be issued, committing the state first would make the teacher's
// console say something the room does not agree with, and the failure would be invisible.
// Running the pass first means a failed POST leaves the previous state untouched (the
// monitor reconciles back to it within one poll) and answers 502.
func (s *Service) StartPrivateTalk(ctx context.Context, in StartPrivateTalkInput) (*PrivateTalkView, error) {
	current, err := s.ownedOpenClassroom(ctx, in.ClassroomID, in.TeacherID)
	if err != nil {
		return nil, err
	}
	if in.TeacherSessionID == uuid.Nil {
		// Unreachable through HTTP (the principal always has a login session): refused
		// instead of proceeding, because the media pass would otherwise look for the
		// microphone of the nil UUID and answer TEACHER_MIC_REQUIRED for a reason that has
		// nothing to do with the teacher.
		return nil, fmt.Errorf("%w: no login session id for the teacher", ErrMediaUnavailable)
	}
	run := current.CurrentRun

	roster, err := s.repo.ListRosterByRun(ctx, in.ClassroomID, run.ID)
	if err != nil {
		return nil, err
	}
	entry, onRoster := rosterEntryOf(roster, in.StudentID)
	if !onRoster {
		// The same answer as "no such student" (§58): the caller named somebody this
		// classroom does not authorize, and the fix is to add them.
		return nil, classroom.ErrStudentNotAssigned
	}
	if entry.Session == nil || !entry.Session.Status.Active() {
		// "目标必须在线" (§31): a student who never entered, or whose session is over, has
		// no media connection to talk to. LEFT and ROOM_CLOSED are terminal, and a session
		// that does not exist cannot be talked to at all.
		return nil, ErrPrivateTalkUnavailable
	}
	targetSession := *entry.Session

	if s.media == nil {
		return nil, fmt.Errorf("%w: no media client is configured", ErrMediaUnavailable)
	}
	observed, err := s.media.ObserveRoom(ctx, run.LiveKitRoomName)
	if err != nil {
		// Not TEACHER_MIC_REQUIRED: "the media plane did not answer" and "the teacher has
		// no microphone" are different facts, and collapsing them would tell the teacher to
		// check a button that may be perfectly fine (§33).
		return nil, fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}

	teacherIdentity := in.TeacherSessionID.String()
	if len(media.TeacherMicrophoneTracks(observed, teacherIdentity)) == 0 {
		// §31's precondition. The teacher must publish the microphone BEFORE the talk
		// starts, because the media pass can only subscribe students to a track that
		// exists; asking for the subscription first and hoping would produce a "talk" that
		// nobody hears.
		return nil, ErrTeacherMicRequired
	}
	if _, present := observed[targetSession.LiveKitIdentity]; !present {
		// The session is active but the participant is not in the room: a student whose
		// laptop dropped, or one who is still connecting. A grant for an identity LiveKit
		// does not have cannot be honoured, and a teacher who was told otherwise would be
		// lied to — the same reasoning as TEACHER_MIC_REQUIRED.
		return nil, ErrPrivateTalkUnavailable
	}

	// One media pass for the whole room, whatever the previous state was: the target is
	// granted and every other student is revoked. A switch is therefore a single pass in
	// which the old target appears on the revoke side.
	students := privateTalkCandidates(roster)
	if err := s.applyPrivateTalk(ctx, run, students, observed, []string{teacherIdentity}, targetSession.LiveKitIdentity); err != nil {
		return nil, err
	}

	previous, hadPrevious := s.talkState.get(in.ClassroomID)
	sameTarget := hadPrevious && previous.StudentID == in.StudentID && previous.SessionID == targetSession.ID

	target := privateTalkTarget{
		StudentID:        targetSession.StudentID,
		SessionID:        targetSession.ID,
		DisplayName:      entry.DisplayName,
		RunID:            run.ID,
		ClassroomID:      in.ClassroomID,
		TeacherSessionID: in.TeacherSessionID,
	}

	if sameTarget {
		// Idempotent: the same student was already the target. Nothing is written and
		// nothing is broadcast — a double click must not produce two TEACHER_TALK_STARTED
		// rows, and a re-sent request must not re-open the student's "开启麦克风" dialog.
		logging.FromContext(ctx).Debug("private talk already targets this student",
			"action", "private_talk_idempotent",
			logging.FieldClassroomID, in.ClassroomID.String(),
			logging.FieldRunID, run.ID.String(),
			logging.FieldSessionID, target.SessionID.String(),
			"student_id", target.StudentID.String(),
		)
		return &PrivateTalkView{Target: viewOf(target)}, nil
	}

	// From here on the decision is made: state, audit rows and messages. A switch ends the
	// previous talk FIRST, so the two facts are recorded in the order they happened.
	s.talkState.set(in.ClassroomID, target)
	if hadPrevious {
		s.appendTalkEvent(ctx, previous.SessionID, EventTeacherTalkEnded, map[string]any{
			"runId":     run.ID.String(),
			"studentId": previous.StudentID.String(),
			"teacherId": in.TeacherID.String(),
			"reason":    TalkEndReasonSwitched,
		})
	}
	s.appendTalkEvent(ctx, target.SessionID, EventTeacherTalkStarted, map[string]any{
		"runId":     run.ID.String(),
		"studentId": target.StudentID.String(),
		"teacherId": in.TeacherID.String(),
	})

	logging.FromContext(ctx).Info("private talk started",
		"action", "private_talk_started",
		logging.FieldClassroomID, in.ClassroomID.String(),
		logging.FieldRunID, run.ID.String(),
		logging.FieldSessionID, target.SessionID.String(),
		"student_id", target.StudentID.String(),
		"switched_from", previousStudentID(hadPrevious, previous),
		"note", "only this student is subscribed to the teacher's microphone (§31)",
	)

	// A switch ends the previous talk for the audiences FIRST, which is both the order the
	// two facts happened in ("张三 → 结束 → 李四", §31) and the order that leaves a client
	// which sees only the tail of the pair in the state the server is actually in: a client
	// that missed the ENDED would believe the teacher is talking to somebody they are not.
	if hadPrevious {
		s.announceTalkEnded(ctx, previous)
	}
	s.announceTalkStarted(ctx, target, in.TeacherDisplayName, !observed[targetSession.LiveKitIdentity].Microphone)
	return &PrivateTalkView{Target: viewOf(target)}, nil
}

// StopPrivateTalk ends the classroom's private talk, if there is one (§31).
//
// # Why an ending is allowed to be best-effort while a start is not
//
// Starting a talk is a promise ("the teacher can be heard by this student"), so a start
// that cannot be honoured fails loudly. Ending one is a SAFETY action: the desired end
// state is "nobody is subscribed to the teacher's microphone", and if the media plane
// refuses the revocation right now, the monitor's reconciliation pass reaches the same end
// state on its next poll (there is no target, so every student is revoked). Failing the
// teacher's click would only make them press it again.
//
// # Idempotency
//
// No target is a success with no work: a double click, a retry, or a DELETE that races the
// classroom close all end in the same place. Nothing is written and nothing is broadcast,
// so a repeated request cannot produce a second TEACHER_TALK_ENDED.
func (s *Service) StopPrivateTalk(ctx context.Context, in StopPrivateTalkInput) error {
	// Ownership and existence are enforced even when there is nothing to stop: this
	// endpoint must not become an oracle for "does this classroom exist and is it mine?".
	// A CLOSED classroom is NOT an error here — the close already ended the talk (see
	// Processor.closeRun), and answering 409 to a teardown call would leave the frontend
	// showing an error for an end state that already holds.
	current, err := s.ownedClassroom(ctx, in.ClassroomID, in.TeacherID)
	if err != nil {
		return err
	}

	previous, had := s.talkState.clear(in.ClassroomID)
	if !had {
		logging.FromContext(ctx).Debug("private talk stop with no active talk",
			"action", "private_talk_idempotent",
			logging.FieldClassroomID, in.ClassroomID.String(),
		)
		return nil
	}

	// The teacher's own login session is preferred for the revocation; the recorded one is
	// the fallback (a teacher who re-authenticated mid-lesson has a new identity, and the
	// recorded one is the only one whose microphone the target could be subscribed to).
	teacherIdentities := []string{in.TeacherSessionID.String()}
	if previous.TeacherSessionID != uuid.Nil && previous.TeacherSessionID != in.TeacherSessionID {
		teacherIdentities = append(teacherIdentities, previous.TeacherSessionID.String())
	}
	runID := previous.RunID
	if runID == uuid.Nil {
		runID = runIDOf(current)
	}
	s.revokePrivateTalk(ctx, in.ClassroomID, runID, teacherIdentities, TalkEndReasonStopped)

	s.appendTalkEvent(ctx, previous.SessionID, EventTeacherTalkEnded, map[string]any{
		"runId":     runID.String(),
		"studentId": previous.StudentID.String(),
		"teacherId": in.TeacherID.String(),
		"reason":    TalkEndReasonStopped,
	})
	logging.FromContext(ctx).Info("private talk stopped",
		"action", "private_talk_stopped",
		logging.FieldClassroomID, in.ClassroomID.String(),
		logging.FieldSessionID, previous.SessionID.String(),
		"student_id", previous.StudentID.String(),
	)
	s.announceTalkEnded(ctx, previous)
	return nil
}

// PrivateTalk reports the classroom's current target, or IDLE (§31).
//
// A CLOSED classroom answers IDLE rather than 409: "nobody is being talked to" is true of
// a lesson that is over, and the frontend polls this to render the button's state.
func (s *Service) PrivateTalk(ctx context.Context, classroomID, teacherID uuid.UUID) (*PrivateTalkView, error) {
	if _, err := s.ownedClassroom(ctx, classroomID, teacherID); err != nil {
		return nil, err
	}
	target, ok := s.talkState.get(classroomID)
	if !ok {
		return &PrivateTalkView{}, nil
	}
	return &PrivateTalkView{Target: viewOf(target)}, nil
}

// EndPrivateTalkForSession implements PrivateTalkEnder for the paths that know a session
// (a webhook, the leave endpoint): the target is gone, so the talk is over.
func (s *Service) EndPrivateTalkForSession(ctx context.Context, sessionID uuid.UUID, reason string) error {
	target, ok := s.talkState.clearSession(sessionID)
	if !ok {
		return nil
	}
	return s.finishTalk(ctx, target, reason)
}

// EndPrivateTalkForRun implements PrivateTalkEnder for the lesson-level path: the run is
// over, so nothing of it is being talked to any more.
func (s *Service) EndPrivateTalkForRun(ctx context.Context, runID uuid.UUID, reason string) error {
	if s == nil || s.repo == nil || runID == uuid.Nil {
		return nil
	}
	// Every target of this run, and only those: the registry is keyed by classroom, and a
	// run belongs to exactly one classroom, so this is one iteration over a handful of
	// entries.
	for _, target := range s.talkState.snapshot() {
		if target.RunID != runID {
			continue
		}
		if _, ok := s.talkState.clear(target.ClassroomID); !ok {
			continue
		}
		if err := s.finishTalk(ctx, target, reason); err != nil {
			return err
		}
	}
	return nil
}

// finishTalk is the shared tail of every path that ends a talk without the teacher asking:
// revoke the target's subscription (best effort), record the end, and tell the two
// audiences.
func (s *Service) finishTalk(ctx context.Context, target privateTalkTarget, reason string) error {
	teacherIdentities := make([]string, 0, 1)
	if target.TeacherSessionID != uuid.Nil {
		teacherIdentities = append(teacherIdentities, target.TeacherSessionID.String())
	}
	s.revokePrivateTalk(ctx, target.ClassroomID, target.RunID, teacherIdentities, reason)

	payload := map[string]any{
		"studentId": target.StudentID.String(),
		"reason":    reason,
	}
	if target.RunID != uuid.Nil {
		payload["runId"] = target.RunID.String()
	}
	if target.TeacherSessionID != uuid.Nil {
		payload["teacherSessionId"] = target.TeacherSessionID.String()
	}
	s.appendTalkEvent(ctx, target.SessionID, EventTeacherTalkEnded, payload)

	logging.FromContext(ctx).Info("private talk ended",
		"action", "private_talk_ended",
		logging.FieldClassroomID, target.ClassroomID.String(),
		logging.FieldSessionID, target.SessionID.String(),
		"student_id", target.StudentID.String(),
		"reason", reason,
	)
	s.announceTalkEnded(ctx, target)
	return nil
}

// applyPrivateTalk runs one media reconciliation pass for the private-talk rule and turns
// a media-plane failure into the error the POST must answer with.
//
// It exists so the "a start must be able to keep its promise" rule is written once: the
// monitor's convergence path calls the same primitive but only logs.
func (s *Service) applyPrivateTalk(
	ctx context.Context,
	run *classroom.Run,
	students []string,
	observed map[string]media.ParticipantTracks,
	teacherIdentities []string,
	targetIdentity string,
) error {
	enforcement, err := s.media.EnforcePrivateTalk(ctx, run.LiveKitRoomName, students, observed, teacherIdentities, targetIdentity)
	s.logEnforcement(ctx, run, enforcement, "the request asked for this target")
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscriptions could not be applied",
			"action", "private_talk_subscription_failed",
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"target_identity", targetIdentity,
			"error", err,
			"consequence", "the request is refused so the teacher is not told a talk started that the media plane did not accept",
		)
		return fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}
	return nil
}

// revokePrivateTalk converges the room to "nobody is subscribed to the teacher's
// microphone" after a talk ended, best effort.
//
// It re-reads the roster and the room because the caller may be a webhook with no request
// behind it: the honest way to revoke subscriptions is to look at what is actually
// published, which is also what makes the call idempotent.
func (s *Service) revokePrivateTalk(ctx context.Context, classroomID, runID uuid.UUID, teacherIdentities []string, reason string) {
	if s.media == nil || runID == uuid.Nil || len(teacherIdentities) == 0 {
		return
	}
	run, err := s.classrooms.RunByID(ctx, runID)
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscription not revoked: the run could not be read",
			"action", "private_talk_revocation_skipped",
			logging.FieldRunID, runID.String(),
			"error", err,
			"consequence", "the monitor reconciliation revokes it on its next poll (§31)",
		)
		return
	}
	observed, err := s.media.ObserveRoom(ctx, run.LiveKitRoomName)
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscription not revoked: the room could not be observed",
			"action", "private_talk_revocation_skipped",
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"error", err,
			"consequence", "the monitor reconciliation revokes it on its next poll (§31)",
		)
		return
	}
	roster, err := s.repo.ListRosterByRun(ctx, classroomID, run.ID)
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscription not revoked: the roster could not be read",
			"action", "private_talk_revocation_skipped",
			logging.FieldRunID, run.ID.String(),
			"error", err,
			"consequence", "the monitor reconciliation revokes it on its next poll (§31)",
		)
		return
	}
	enforcement, err := s.media.EnforcePrivateTalk(ctx, run.LiveKitRoomName, privateTalkCandidates(roster), observed, teacherIdentities, "")
	s.logEnforcement(ctx, run, enforcement, reason)
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscriptions could not be revoked",
			"action", "private_talk_revocation_failed",
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"error", err,
			"consequence", "the state is already ended; the monitor reconciliation retries",
		)
	}
}

// logEnforcement reports what one pass changed. Grants are Info (the teacher's decision
// taking effect), revocations of OTHER students are Info too — during a start or a switch
// they are the expected half of the rule, not evidence of a misbehaving client.
func (s *Service) logEnforcement(ctx context.Context, run *classroom.Run, enforcement media.PrivateTalkEnforcement, reason string) {
	for _, granted := range enforcement.Granted {
		logging.FromContext(ctx).Info("private talk subscription granted",
			"action", "private_talk_subscription_granted",
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"student_identity", granted.StudentIdentity,
			"track_sid", granted.TrackSid,
		)
	}
	for _, revoked := range enforcement.Revoked {
		logging.FromContext(ctx).Info("private talk subscription revoked",
			"action", "private_talk_subscription_revoked",
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"student_identity", revoked.StudentIdentity,
			"track_sid", revoked.TrackSid,
			"reason", reason,
		)
	}
}

// enforcePrivateTalk is the monitor's reconciliation pass (§31/§51).
//
// # Why the monitor is the loop that guarantees "no ghost subscriptions"
//
// The registry is process-local, so it can be lost (a restart) or miss an ending (a
// webhook this process never received). The monitor is the one place that already polls
// the media plane with the teacher watching, so it is where the media plane is converged
// on what this process believes: with a live target, exactly that student is subscribed;
// with no target, EVERY student is unsubscribed. That second half is the property that
// matters after a restart — the media plane may still be delivering the teacher's audio to
// whoever was selected before, and it must stop.
//
// # Why a stale target is dropped here
//
// If the recorded target's session is no longer a live session of this run, the talk is
// over whether or not anything told this process. The entry is cleared (so the read
// endpoint stops reporting it) and the media plane is converged to "no target". No message
// is sent from here: the paths that end a talk for a reason (a leave, a close, a
// disconnect) announce it themselves, and reaching this branch means one of them did not
// run — a log line is the honest artefact, not a second messaging pathway.
func (s *Service) enforcePrivateTalk(
	ctx context.Context,
	classroomID uuid.UUID,
	run *classroom.Run,
	roster []RosterEntry,
	observed map[string]media.ParticipantTracks,
	teacherIdentities []string,
) {
	if s.media == nil || len(observed) == 0 {
		return
	}
	targetIdentity := ""
	if stored, ok := s.talkState.get(classroomID); ok {
		entry, found := rosterEntryOf(roster, stored.StudentID)
		if found && entry.Session != nil && entry.Session.ID == stored.SessionID && !entry.Session.Status.Terminal() {
			targetIdentity = stored.SessionID.String()
		} else {
			s.talkState.clear(classroomID)
			logging.FromContext(ctx).Info("private talk target is no longer a live session; converging to no target",
				"action", "private_talk_stale_target",
				logging.FieldClassroomID, classroomID.String(),
				logging.FieldRunID, run.ID.String(),
				logging.FieldSessionID, stored.SessionID.String(),
				"student_id", stored.StudentID.String(),
			)
		}
	}

	enforcement, err := s.media.EnforcePrivateTalk(ctx, run.LiveKitRoomName, privateTalkCandidates(roster), observed, teacherIdentities, targetIdentity)
	if targetIdentity == "" {
		// No target means the pass may have REVOKED students that were previously
		// subscribed. That is the convergence rule doing its job after a restart, so it is
		// reported; with a target, the revocations are the normal half of a start/switch
		// and are already logged by the request that caused them.
		s.logEnforcement(ctx, run, enforcement, "no target: nobody may receive the teacher's microphone (§31)")
	}
	if err != nil {
		logging.FromContext(ctx).Warn("private talk subscriptions could not be reconciled",
			"action", "private_talk_reconciliation_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"error", err,
			"consequence", "the monitor response is unaffected and the next poll retries",
		)
	}
}

// appendTalkEvent appends one private-talk row, logging a failure instead of returning it.
//
// WHY a failure is not fatal: the audit row is not the decision. The state is committed
// and the media plane already agrees with it, so failing the teacher's request over an
// insert would report a failure that did not happen (§33). The line is a Warn because a
// missing TEACHER_TALK_STARTED is a fact a lesson report would be missing.
func (s *Service) appendTalkEvent(ctx context.Context, sessionID uuid.UUID, event EventType, payload map[string]any) {
	if s.talkAudit == nil || sessionID == uuid.Nil {
		return
	}
	if err := s.talkAudit.AppendEvent(ctx, sessionID, event, payload); err != nil {
		logging.FromContext(ctx).Warn("private talk event could not be recorded",
			"action", "talk_event_failed",
			logging.FieldSessionID, sessionID.String(),
			"event_type", string(event),
			"error", err,
			"consequence", "the talk itself is unaffected; only the event log is incomplete",
		)
	}
}

// announceTalkStarted tells the target and the owner that the talk began, and asks the
// target to open their microphone when they have not (§25/§31).
func (s *Service) announceTalkStarted(ctx context.Context, target privateTalkTarget, teacherDisplayName string, microphoneOff bool) {
	if s.talk == nil {
		return
	}
	ref := SessionRef{SessionID: target.SessionID, StudentID: target.StudentID, RunID: target.RunID}
	if err := s.talk.PrivateTalkStarted(ctx, ref, teacherDisplayName); err != nil {
		logging.FromContext(ctx).Warn("private talk started was not broadcast",
			"action", "private_talk_broadcast_failed",
			logging.FieldSessionID, target.SessionID.String(), "error", err)
	}
	if !microphoneOff {
		return
	}
	// §25: the student's dialog ("王老师希望与你进行语音沟通。[开启麦克风][暂不开启]") is only
	// useful when there is something to open. A student who is already publishing does not
	// get asked to publish.
	if err := s.talk.PrivateTalkRequested(ctx, ref, teacherDisplayName); err != nil {
		logging.FromContext(ctx).Warn("private talk request was not broadcast",
			"action", "private_talk_broadcast_failed",
			logging.FieldSessionID, target.SessionID.String(), "error", err)
	}
}

// announceTalkEnded tells the target and the owner that the talk is over (§31).
func (s *Service) announceTalkEnded(ctx context.Context, target privateTalkTarget) {
	if s.talk == nil {
		return
	}
	if err := s.talk.PrivateTalkEnded(ctx, SessionRef{
		SessionID: target.SessionID, StudentID: target.StudentID, RunID: target.RunID,
	}); err != nil {
		logging.FromContext(ctx).Warn("private talk ended was not broadcast",
			"action", "private_talk_broadcast_failed",
			logging.FieldSessionID, target.SessionID.String(), "error", err)
	}
}

// ownedClassroom loads a classroom for its owner without requiring it to be OPEN.
//
// WHY the private-talk reads and the stop use this instead of ownedOpenClassroom: "this
// classroom exists and is yours" and "this lesson is running" are different questions, and
// a lesson that just ended still has an owner. The operations that cannot work on a closed
// classroom (starting a talk) keep using the stricter helper.
func (s *Service) ownedClassroom(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error) {
	if classroomID == uuid.Nil || teacherID == uuid.Nil {
		return nil, classroom.ErrNotFound
	}
	return s.classrooms.Get(ctx, classroomID, teacherID)
}

// viewOf renders the public shape of a target. It never exposes the teacher's media
// identity: that is a media-plane handle the teacher's own browser already has (§44/§33).
func viewOf(target privateTalkTarget) *PrivateTalkTarget {
	return &PrivateTalkTarget{
		StudentID:   target.StudentID,
		SessionID:   target.SessionID,
		DisplayName: target.DisplayName,
	}
}

// rosterEntryOf finds one student's roster line.
func rosterEntryOf(roster []RosterEntry, studentID uuid.UUID) (RosterEntry, bool) {
	for _, entry := range roster {
		if entry.StudentID == studentID {
			return entry, true
		}
	}
	return RosterEntry{}, false
}

// privateTalkCandidates lists the media identities of every student of the run who has a
// session at all.
//
// # Why terminal sessions ARE included here, unlike in the §26 pass
//
// The §26 rule asks "which classmate's track may this student still receive?" and a student
// whose session is over has been disconnected (§50), so naming them would only produce a
// NotFound. This rule asks the opposite question — "who must NOT be receiving the teacher's
// microphone?" — and the answer must be "everybody except the target", including a student
// whose session just ended: §50's participant removal is best effort, so for a few seconds
// a LEFT student can still be a live participant in the room, and leaving them out would be
// exactly the ghost subscription §31 forbids.
//
// A student who is genuinely gone costs nothing: the media layer only acts on identities it
// observed in the room, so a departed participant is filtered out before any RPC.
func privateTalkCandidates(roster []RosterEntry) []string {
	identities := make([]string, 0, len(roster))
	for _, entry := range roster {
		if entry.Session == nil {
			continue
		}
		identities = append(identities, entry.Session.LiveKitIdentity)
	}
	return identities
}

// previousStudentID renders a switch's origin for a log line, tolerating "there was none".
func previousStudentID(had bool, previous privateTalkTarget) string {
	if !had || previous.StudentID == uuid.Nil {
		return ""
	}
	return previous.StudentID.String()
}

// runIDOf is the current run id of a classroom, or the nil UUID when it has none.
func runIDOf(current *classroom.Classroom) uuid.UUID {
	if current == nil || current.CurrentRun == nil {
		return uuid.Nil
	}
	return current.CurrentRun.ID
}
