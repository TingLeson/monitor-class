package session

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// Service implements the session use cases of §43/§45/§49/§51: joining a lesson,
// leaving it, issuing the two kinds of media token, and folding media-plane
// observations into the monitoring state.
//
// It owns no authorization rule of its own. "May this student enter?" is the
// classroom roster (§14), "is this the owner?" is the classroom's owner column
// (§37) — both are answered by internal/classroom, and this service only decides
// what a session IS once the answer is yes.
type Service struct {
	repo       Repository
	classrooms Directory
	media      MediaPlane
	cfg        Config
	// lifecycle is the runtime event path of §74 (the session_events log and the
	// WebSocket messages). It is optional on purpose: the join and leave endpoints are
	// correct without it, and a deployment with no realtime layer must not lose the
	// ability to record a session — it only loses the messages.
	lifecycle LifecycleEvents
}

// NewService wires the service. media may be nil in a degraded deployment (the API
// can run with LiveKit unreachable, see STARTUP_REQUIRE_DEPENDENCIES): the join and
// token paths then answer MEDIA_TOKEN_FAILED instead of pretending to have a media
// plane, which is the honest answer for a process that cannot talk to LiveKit.
func NewService(repo Repository, classrooms Directory, mediaPlane MediaPlane, cfg Config) *Service {
	return &Service{repo: repo, classrooms: classrooms, media: mediaPlane, cfg: cfg}
}

// WithLifecycleEvents attaches the runtime event path (§13/§74).
//
// WHY a setter rather than a constructor argument: every existing caller and test keeps
// working without a hub, and "no event layer" is a state the code has to handle anyway
// (an API booted with STARTUP_REQUIRE_DEPENDENCIES=false has a database but no media
// plane). The alternative — a nil interface argument in every test — would hide that
// state instead of making it visible at the one wiring site that matters.
func (s *Service) WithLifecycleEvents(events LifecycleEvents) *Service {
	s.lifecycle = events
	return s
}

// Capture is the diagnostic block the student frontend submits with a join (§43).
//
// It is DIAGNOSTICS AND NOTHING ELSE. The server cannot verify `displaySurface` —
// that value comes from the browser's own screen-capture API and a modified client
// can send anything (§19) — so the only honest uses for it are a log line and, in a
// later phase, an event row. It is not an authorization input, it is not a gate, and
// it is deliberately absent from CreateOrReuseParams: there is no field to store it
// in, so a future change cannot accidentally start trusting it.
type Capture struct {
	DisplaySurface string
	Width          int
	Height         int
}

// JoinInput is the request of Join.
type JoinInput struct {
	// StudentID comes from the authenticated session, never from the body: a
	// client-supplied student id would let any student create a media session in
	// another student's name, which is the whole of the authorization here.
	StudentID   uuid.UUID
	ClassroomID uuid.UUID
	// Capture is optional diagnostics (§43).
	Capture *Capture
}

// JoinResult is what the join endpoint returns: the session it created or reused,
// the browser-facing media endpoint, and a short-lived token for exactly that
// participant and room.
type JoinResult struct {
	Session *StudentSession
	// LiveKitURL is the wss:// endpoint the browser connects to.
	LiveKitURL string
	// Token is a credential scoped to one room, one identity and one set of publish
	// permissions. It is returned to its owner and never logged (§59).
	Token string
}

// Join creates or reuses the student's media session for the classroom currently
// open, and mints a screen-share token for it (§43).
//
// The order of operations is the security-relevant part:
//
//  1. The classroom is read through the roster JOIN, so an unauthorized student
//     learns nothing (404 STUDENT_NOT_ASSIGNED, the same answer as "no such
//     classroom").
//  2. The classroom must be OPEN with a current run. A token is only ever minted for
//     the room of a run that is open RIGHT NOW (§49: a closed lesson must not be
//     enterable, no matter what the student's page still shows).
//  3. The session row is created or reused, and the token's identity is that row's
//     id (§44) — never the account, never the display name.
//  4. The room is created if needed BEFORE the token is handed over, so the token can
//     be used immediately.
//
// A failure at step 4 leaves the session row behind in CONNECTING. That is
// deliberate: the row is the record that this student tried to enter, and the retry
// reuses it instead of creating a second one.
func (s *Service) Join(ctx context.Context, in JoinInput) (*JoinResult, error) {
	if in.StudentID == uuid.Nil || in.ClassroomID == uuid.Nil {
		// A nil uuid can never match a grant row; answering "not assigned" keeps it
		// from behaving like a wildcard if a future query loses its WHERE clause.
		return nil, classroom.ErrStudentNotAssigned
	}

	entry, err := s.classrooms.StudentEntry(ctx, in.StudentID, in.ClassroomID)
	if err != nil {
		return nil, err
	}
	if entry.Status != classroom.StatusOpen || entry.Run == nil {
		return nil, ErrClassroomClosed
	}
	run := entry.Run

	// The capture block is logged and dropped. WHY it is logged at all: when a
	// student says "I shared my whole screen but the teacher saw a tab", this line is
	// the only evidence of what their browser claimed, and the classroom id plus the
	// request id are what make it findable. WHY it is not stored: §43 makes it
	// diagnostics, and a column would make it look like evidence.
	if in.Capture != nil {
		logging.FromContext(ctx).Info("student join capture diagnostics",
			"action", "student_join_capture",
			logging.FieldUserID, in.StudentID.String(),
			logging.FieldClassroomID, in.ClassroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"display_surface", in.Capture.DisplaySurface,
			"width", in.Capture.Width,
			"height", in.Capture.Height,
			"note", "client-reported diagnostics; never an authorization input",
		)
	}

	created, err := s.repo.CreateOrReuse(ctx, CreateOrReuseParams{
		// Minted here, not by the database: this value becomes the media identity, and
		// the service logs it even if the INSERT fails.
		SessionID:      uuid.New(),
		ClassroomRunID: run.ID,
		StudentID:      in.StudentID,
	})
	if err != nil {
		return nil, err
	}

	if s.media == nil {
		return nil, fmt.Errorf("%w: no media client is configured", ErrMediaUnavailable)
	}
	if err := s.media.EnsureRoom(ctx, run.LiveKitRoomName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}

	token, err := s.media.SignToken(media.TokenRequest{
		Identity: created.LiveKitIdentity,
		RoomName: run.LiveKitRoomName,
		TTL:      s.cfg.TokenTTL,
		// §28: a student joins the room, may publish their screen, and may subscribe
		// (the teacher's private audio arrives in a later phase and needs the
		// subscription right). The CLIENT keeps autoSubscribe=false, so this
		// permission is not the same thing as subscribing to the whole classroom.
		CanPublish:   true,
		CanSubscribe: true,
		// §47: business messages go over a WebSocket, so the WebRTC data channel is
		// switched off rather than left as an unobserved side channel.
		CanPublishData: false,
		// §28: a student may publish their screen and — from Phase 9 — their camera.
		//
		// WHY the camera joins the grant now: §75 makes the camera the student's
		// OPTIONAL second track, and the publish permission has to be in the TOKEN
		// (LiveKit enforces sources, so a camera track published without this entry is
		// refused by the media plane before any webhook could describe it). The order
		// mirrors §28: screen first because it is mandatory, camera second because it
		// is not.
		//
		// WHY the microphone is still absent (Phase 10, §76): the grant is not the
		// missing piece — the CONTROL PLANE is. A microphone published today would
		// produce no MIC_STARTED/MIC_STOPPED event, no MIC_CHANGED message, and no
		// answer to the §31 question "who is the teacher allowed to talk to?". §33
		// keeps the media plane behind the control plane, never ahead of it, so the
		// permission is granted in the same phase as its event path.
		PublishSources: []media.PublishSource{media.PublishScreenShare, media.PublishCamera},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}

	// The token is deliberately NOT logged, not even truncated: a log line is copied
	// into tickets and dashboards, and a media token is a credential (§59).
	logging.FromContext(ctx).Info("student joined classroom",
		"action", "student_join",
		logging.FieldUserID, in.StudentID.String(),
		logging.FieldClassroomID, in.ClassroomID.String(),
		logging.FieldRunID, run.ID.String(),
		logging.FieldSessionID, created.ID.String(),
		"session_status", string(created.Status),
		"token_ttl_seconds", int(s.cfg.TokenTTL.Seconds()),
	)

	// §13: entering a lesson is an event. It is recorded once per session row, so a
	// double-clicked join or a reload that reused the row does not grow the log. A
	// failure here is logged and ignored: the student is already in (the row and the
	// token exist), and failing the endpoint over an audit row would be the wrong trade.
	if s.lifecycle != nil {
		if err := s.lifecycle.SessionCreated(ctx, created); err != nil {
			logging.FromContext(ctx).Warn("session created event could not be recorded",
				"action", "session_event_failed",
				logging.FieldSessionID, created.ID.String(),
				"event_type", string(EventSessionCreated),
				"error", err,
				"consequence", "the join succeeded; only the event log is incomplete",
			)
		}
	}

	return &JoinResult{Session: created, LiveKitURL: s.cfg.LiveKitURL, Token: token}, nil
}

// Leave marks the caller's own session LEFT (§43/§50).
//
// Two properties are load-bearing:
//
//   - IDEMPOTENT. A student who closes the tab and comes back to a stale page may
//     send this twice, and the second call must be a 204, not an error. The
//     repository keeps the first left_at, so the lesson's history does not move.
//   - SCOPED TO THE CALLER. "Not my session" and "no such session" are the same
//     error, so one student cannot probe another's session ids (§58).
//
// Afterwards the participant is disconnected from the room, best effort. WHY that
// does not affect the control-plane truth: the row is already LEFT, and being removed
// from a media room is not what "left the lesson" means — a failing LiveKit call can
// only leave a participant in a room the teacher is no longer watching, and LiveKit's
// own timeouts clear it. Reporting an error would tell the student their leave failed
// when the database says it succeeded (§33).
func (s *Service) Leave(ctx context.Context, sessionID, studentID uuid.UUID) (*StudentSession, error) {
	if sessionID == uuid.Nil || studentID == uuid.Nil {
		return nil, ErrSessionNotFound
	}
	left, err := s.repo.Leave(ctx, sessionID, studentID)
	if err != nil {
		return nil, err
	}

	logging.FromContext(ctx).Info("student left classroom",
		"action", "student_leave",
		logging.FieldUserID, studentID.String(),
		logging.FieldRunID, left.ClassroomRunID.String(),
		logging.FieldSessionID, left.ID.String(),
		"session_status", string(left.Status),
	)

	// §13/§47: leaving is an event, and the teacher's console must learn about it
	// without waiting for the next poll. The event row is the idempotency key of the
	// message (see Processor.SessionLeft), so a retried leave produces neither a second
	// row nor a second STUDENT_OFFLINE.
	if s.lifecycle != nil {
		if err := s.lifecycle.SessionLeft(ctx, left); err != nil {
			logging.FromContext(ctx).Warn("student left event could not be recorded",
				"action", "session_event_failed",
				logging.FieldSessionID, left.ID.String(),
				"event_type", string(EventStudentLeft),
				"error", err,
				"consequence", "the leave succeeded; only the event log and the console message are missing",
			)
		}
	}

	s.removeParticipant(ctx, left)
	return left, nil
}

// removeParticipant disconnects a participant whose session just became terminal.
//
// Both lookups are best effort and log-only: the control plane has already recorded
// the decision, and the media plane is allowed to be behind (§33).
func (s *Service) removeParticipant(ctx context.Context, session *StudentSession) {
	if s.media == nil || session == nil {
		return
	}
	run, err := s.classrooms.RunByID(ctx, session.ClassroomRunID)
	if err != nil {
		logging.FromContext(ctx).Warn("media participant not removed: run could not be read",
			"action", "remove_participant_skipped",
			logging.FieldRunID, session.ClassroomRunID.String(),
			logging.FieldSessionID, session.ID.String(),
			"error", err,
			"consequence", "the participant is dropped by LiveKit when they disconnect",
		)
		return
	}
	if err := s.media.RemoveParticipant(ctx, run.LiveKitRoomName, session.LiveKitIdentity); err != nil {
		logging.FromContext(ctx).Warn("media participant not removed after leave",
			"action", "remove_participant_failed",
			logging.FieldRunID, session.ClassroomRunID.String(),
			logging.FieldSessionID, session.ID.String(),
			"room", run.LiveKitRoomName,
			"error", err,
			"consequence", "the session is already LEFT in the control plane",
		)
	}
}

// TeacherTokenInput is the request of TeacherToken.
type TeacherTokenInput struct {
	ClassroomID uuid.UUID
	// TeacherID is the authenticated owner; ownership is checked here.
	TeacherID uuid.UUID
	// SessionID is the caller's LOGIN session id (§44). It is the participant
	// identity: a teacher's media identity must be at least as opaque as a student's,
	// and the login session already is a UUID nobody can map back to a person without
	// the database.
	SessionID uuid.UUID
}

// TeacherTokenResult is the response of the teacher media-token endpoint.
type TeacherTokenResult struct {
	LiveKitURL string
	Token      string
}

// TeacherToken mints the teacher's media token for the current run (§27/§44).
//
// The permissions are the narrowest set the phase needs: subscribe (to see the
// students' screens), publish, and among the publishable sources ONLY the microphone —
// §27 gives the teacher no camera and no screen share in V1, and the grant makes that
// a media-plane fact rather than a UI convention.
func (s *Service) TeacherToken(ctx context.Context, in TeacherTokenInput) (*TeacherTokenResult, error) {
	current, err := s.ownedOpenClassroom(ctx, in.ClassroomID, in.TeacherID)
	if err != nil {
		return nil, err
	}
	if in.SessionID == uuid.Nil {
		// Unreachable through the HTTP layer (the principal always has a session id);
		// refused instead of minted so a programming error cannot produce a token
		// whose identity is the nil UUID.
		return nil, fmt.Errorf("%w: no login session id for the teacher token", ErrMediaUnavailable)
	}
	run := current.CurrentRun

	if s.media == nil {
		return nil, fmt.Errorf("%w: no media client is configured", ErrMediaUnavailable)
	}
	if err := s.media.EnsureRoom(ctx, run.LiveKitRoomName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}
	token, err := s.media.SignToken(media.TokenRequest{
		Identity:       in.SessionID.String(),
		RoomName:       run.LiveKitRoomName,
		TTL:            s.cfg.TokenTTL,
		CanPublish:     true,
		CanSubscribe:   true,
		CanPublishData: false,
		// §27 + §76: the teacher's private audio is Phase 10; the microphone source is
		// granted now (the phase-6 token always carried it) but the client publishes
		// nothing yet. §27 also gives the teacher NO camera and NO screen share in V1,
		// and Phase 9 does not change that: the camera is the student's track, and a
		// teacher who published one would put a tile of the teacher on the teacher's
		// own wall.
		PublishSources: []media.PublishSource{media.PublishMicrophone},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaUnavailable, err)
	}

	logging.FromContext(ctx).Info("teacher media token issued",
		"action", "teacher_media_token",
		logging.FieldUserID, in.TeacherID.String(),
		logging.FieldClassroomID, in.ClassroomID.String(),
		logging.FieldRunID, run.ID.String(),
		logging.FieldSessionID, in.SessionID.String(),
		"token_ttl_seconds", int(s.cfg.TokenTTL.Seconds()),
	)
	return &TeacherTokenResult{LiveKitURL: s.cfg.LiveKitURL, Token: token}, nil
}

// Monitor folds the media plane's current state into the monitoring view of §51.
//
// # Why the list is the ROSTER and not the session table
//
// Phase 6 returned one tile per session row, which meant the wall could only show the
// students who had already entered. §29's console header is "18 / 25" and its grid
// contains the students who have NOT come in yet, so the read starts from
// `classroom_students` and LEFT JOINs the run's sessions (see
// Repository.ListRosterByRun). A student who was authorized and never pressed
// "进入课堂" is a tile with sessionId=null, sessionStatus=null, three inactive media
// blocks and connection=UNKNOWN — not a missing tile and not an invented seventh
// status. The order is by account, so the cards never move between two polls.
//
// # Why this endpoint writes
//
// The session states are not decoration: they are the teacher's answer to "is this
// student being supervised right now?", and they must survive a page reload, a
// different teacher's browser and Phase 8's event log. So the observation is
// persisted (guarded by the status it was computed from) and the response is built
// from the stored rows. Only rows that HAVE a session are advanced: a student who never
// entered has no state to move, and inventing one (CONNECTING, say) would make the wall
// claim a media connection nobody attempted.
//
// # Why it also enforces §26
//
// The teacher polls this endpoint every ten seconds, which makes it the natural
// reconciliation loop for the media-plane half of "students must not see each other":
// the SAME observation that advances the states is handed to the media plane to revoke
// peer subscriptions (see enforceStudentIsolation and media.EnforceNoPeerSubscriptions).
// A student whose client subscribed to a classmate therefore loses that subscription and
// leaves a log line, without a second room query and without a background worker.
//
// # Why a media-plane failure is NOT a business fact
//
// If ListParticipants fails, the honest state of every session is "unknown". The
// tempting alternative — treat a failed query as "nobody is connected" and write
// DISCONNECTED for the whole class — would turn a LiveKit hiccup into a permanent
// business record that a student was not being supervised, and the lesson report
// would be wrong forever. So on failure this returns 200 with connection=UNKNOWN for
// every student, advances NOTHING, revokes NOTHING (there is no observation to act on),
// and logs a warning (§33).
func (s *Service) Monitor(ctx context.Context, classroomID, teacherID uuid.UUID) (*MonitorView, error) {
	current, err := s.ownedOpenClassroom(ctx, classroomID, teacherID)
	if err != nil {
		return nil, err
	}
	run := current.CurrentRun

	roster, err := s.repo.ListRosterByRun(ctx, classroomID, run.ID)
	if err != nil {
		return nil, err
	}

	if s.media == nil {
		logging.FromContext(ctx).Warn("monitor without a media plane",
			"action", "monitor_unobserved",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"roster", len(roster),
		)
		return unobservedView(roster), nil
	}

	observed, err := s.media.ObserveRoom(ctx, run.LiveKitRoomName)
	if err != nil {
		logging.FromContext(ctx).Warn("media room could not be observed; session states are not advanced",
			"action", "monitor_unobserved",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"roster", len(roster),
			"error", err,
			"consequence", "the wall reports connection=UNKNOWN instead of inventing a disconnect",
		)
		return unobservedView(roster), nil
	}

	// §26: the same observation, one reconciliation pass. Deliberately before the
	// response is built and deliberately not fatal — see the method.
	s.enforceStudentIsolation(ctx, classroomID, run, roster, observed)

	// One warning per poll that skipped work: a session whose status changed under us
	// (a leave that landed while we were observing) is normal, but a burst of them is
	// the signal that something is writing sessions concurrently.
	var stale int
	view := make([]MonitorStudent, 0, len(roster))
	for i := range roster {
		entry := roster[i]
		if entry.Session == nil {
			// Authorized but not in this lesson (yet). Nothing to observe, nothing to
			// advance: the tile says exactly that with its nulls.
			view = append(view, monitorStudentOf(entry, false, media.ParticipantTracks{}))
			continue
		}
		session := *entry.Session
		tracks, present := observed[session.LiveKitIdentity]

		if !session.Status.Terminal() {
			if next, changed := nextStatus(session.Status, present, tracks.ScreenShare); changed {
				updated, err := s.repo.ApplyObservation(ctx, changeFor(session, next, tracks.ScreenShare))
				if err != nil {
					// The observation could not be recorded. Failing the read keeps the
					// wall from showing a transition the database does not have: the
					// teacher would see ONLINE, reload, and see CONNECTING again.
					return nil, fmt.Errorf("session: persist observation for %s: %w", session.ID, err)
				}
				if updated == nil {
					stale++
				} else {
					// The write returns the ROW it stored, and a row carries no display
					// name: that column comes from the roster's JOIN on users. The entry
					// already holds it, which is what stops a transitioned tile from
					// rendering as an unnamed card.
					logging.FromContext(ctx).Info("student session advanced",
						"action", "session_status_changed",
						logging.FieldUserID, session.StudentID.String(),
						logging.FieldClassroomID, classroomID.String(),
						logging.FieldRunID, run.ID.String(),
						logging.FieldSessionID, session.ID.String(),
						"from", string(session.Status),
						"to", string(updated.Status),
						"participant_present", present,
						"screen_shared", tracks.ScreenShare,
					)
					session = *updated
					entry.Session = &session
				}
			}
		}
		view = append(view, monitorStudentOf(entry, present, tracks))
	}
	if stale > 0 {
		logging.FromContext(ctx).Info("monitor skipped stale session states",
			"action", "monitor_stale",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"stale", stale,
		)
	}
	return &MonitorView{Students: view, MediaObserved: true}, nil
}

// enforceStudentIsolation is the server side of §26: it revokes the subscriptions
// students still hold to each other's tracks, and reports every revocation as a Warn.
//
// # What it is, and what it is not
//
// §28 requires `canSubscribe=true` on a student token, because the teacher's private
// audio (§31, Phase 10) must be able to reach a student. That bit is room-wide, so
// "students do not subscribe to each other" cannot be expressed in the token; the
// client is configured with autoSubscribe=false, and this is the SERVER side of the same
// rule. It is NOT protocol-level isolation: a deliberately modified client keeps
// canSubscribe=true and can re-subscribe the moment after this returns. §26's last
// paragraph says exactly that, and nothing in this project may claim otherwise. What the
// pass buys is that a cooperative-but-misconfigured client is corrected, and that a
// client which keeps doing it is visible in the logs.
//
// # The whitelist
//
// A student may keep receiving tracks from a participant that is NOT a student of this
// run. The only other token this control plane mints for a run belongs to the teacher
// (§27), who publishes nothing in Phase 7 — so in practice students are unsubscribed to
// nothing at all. The whitelist is an explicit argument and not an assumption, which is
// where Phase 10 attaches: it will narrow the allowance to (the teacher, MICROPHONE),
// and the observation already carries each track's source. Deriving it as "not a student
// of this run" also means a student who LEFT but whose participant is still lingering
// stays in the student set, so a classmate is still unsubscribed from their tracks.
//
// # Failure discipline
//
// Nothing here may affect the response: a failed revocation is logged as a Warn and the
// monitor still answers 200 with the states it observed (§33).
func (s *Service) enforceStudentIsolation(
	ctx context.Context,
	classroomID uuid.UUID,
	run *classroom.Run,
	roster []RosterEntry,
	observed map[string]media.ParticipantTracks,
) {
	if s.media == nil || len(observed) == 0 {
		return
	}

	// Every identity that belongs to a student of this run, terminal sessions included:
	// this is what makes "not a student" mean "the teacher" and not "somebody we forgot".
	studentIdentities := make(map[string]struct{}, len(roster))
	for _, entry := range roster {
		if entry.Session != nil {
			studentIdentities[entry.Session.LiveKitIdentity] = struct{}{}
		}
	}
	allowed := make([]string, 0, len(observed))
	for identity := range observed {
		if _, isStudent := studentIdentities[identity]; !isStudent {
			allowed = append(allowed, identity)
		}
	}
	sort.Strings(allowed)

	// Only sessions that are still open are reconciled: a terminal session's participant
	// has already been disconnected (§50), and asking LiveKit to update subscriptions for
	// somebody who is gone can only produce a NotFound.
	students := make([]string, 0, len(roster))
	for _, entry := range roster {
		if entry.Session == nil || entry.Session.Status.Terminal() {
			continue
		}
		students = append(students, entry.Session.LiveKitIdentity)
	}

	revoked, err := s.media.EnforceNoPeerSubscriptions(ctx, run.LiveKitRoomName, students, observed, allowed)
	for _, revocation := range revoked {
		// Warn, not Info: this line is the evidence that a client tried to receive a
		// classmate's media. It is the only trace there is — LiveKit offers no API to read
		// subscription state — so it is what an operator greps for when the §26 rule is
		// suspected of being broken.
		logging.FromContext(ctx).Warn("peer subscription revoked",
			"action", "peer_subscription_revoked",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"observer_identity", revocation.ObserverIdentity,
			"subscribed_track_owner", revocation.TrackOwnerIdentity,
			"track_sid", revocation.TrackSid,
			"reason", "students must not receive each other's media (§26)",
		)
	}
	if err != nil {
		// The media plane refused (or could not be reached). The wall is unaffected: this
		// is a media-plane fact, and the next poll retries.
		logging.FromContext(ctx).Warn("peer subscriptions could not be revoked",
			"action", "peer_subscription_revocation_failed",
			logging.FieldClassroomID, classroomID.String(),
			logging.FieldRunID, run.ID.String(),
			"room", run.LiveKitRoomName,
			"revoked_before_failure", len(revoked),
			"error", err,
			"consequence", "the monitor response is unaffected and the next poll retries",
		)
	}
}

// ownedOpenClassroom loads a classroom for its owner and requires it to be OPEN with
// a current run.
//
// The two teacher endpoints share it because they share the same precondition, and
// §43/§51 want the same answer for "not yours" (403 CLASSROOM_NOT_OWNER, from the
// repository's owner column) and "not open" (409 CLASSROOM_CLOSED).
func (s *Service) ownedOpenClassroom(ctx context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error) {
	if classroomID == uuid.Nil || teacherID == uuid.Nil {
		return nil, classroom.ErrNotFound
	}
	current, err := s.classrooms.Get(ctx, classroomID, teacherID)
	if err != nil {
		return nil, err
	}
	if current.Status != classroom.StatusOpen || current.CurrentRun == nil {
		return nil, ErrClassroomClosed
	}
	return current, nil
}

// nextStatus computes the state a session should be in, given one observation.
//
// It is the whole state machine of §51 in one pure function, which is why it has no
// I/O and no logging: every rule below is a test case, and the caller decides what to
// persist.
//
//	current state       participant      screen track     next state
//	------------------------------------------------------------------
//	CONNECTING          present          yes              ONLINE
//	DISCONNECTED        present          yes              ONLINE
//	SCREEN_LOST         present          yes              ONLINE   (§22: restored)
//	ONLINE              present          yes              (unchanged)
//	ONLINE              present          no               SCREEN_LOST
//	CONNECTING          present          no               (unchanged: the student is
//	                                                        connected but has not shared
//	                                                        yet — SCREEN_LOST means a
//	                                                        screen went AWAY, §22)
//	DISCONNECTED        present          no               CONNECTING
//	CONNECTING/ONLINE/SCREEN_LOST  absent               DISCONNECTED
//	DISCONNECTED        absent          -                (unchanged)
//	LEFT / ROOM_CLOSED  anything        -                (terminal, never changes)
//
// A note on the "absent" row: a session that was never connected (CONNECTING) also
// becomes DISCONNECTED. That looks harsh for a browser that is still negotiating, but
// it is the honest reading of "the media plane does not have this participant", and
// it converges: the moment the participant appears, the state moves to ONLINE.
func nextStatus(current Status, present, screen bool) (Status, bool) {
	if current.Terminal() {
		return current, false
	}
	switch {
	case present && screen:
		if current == StatusOnline {
			return current, false
		}
		return StatusOnline, true
	case present && !screen:
		switch current {
		case StatusOnline:
			return StatusScreenLost, true
		case StatusDisconnected:
			// Back in the room, screen not up yet: the connection is re-established,
			// the §21 invariant is not. CONNECTING says exactly that.
			return StatusConnecting, true
		default:
			return current, false
		}
	default:
		if current == StatusDisconnected {
			return current, false
		}
		return StatusDisconnected, true
	}
}

// changeFor turns a decided transition into the write that persists it.
//
// It takes the whole session, not only its status: the write is a compare-and-set on
// (id, from), and passing those two separately is how a change ends up applying to the
// nil UUID (which matches nothing, silently).
func changeFor(session StudentSession, to Status, screen bool) ObservationChange {
	change := ObservationChange{SessionID: session.ID, From: session.Status, To: to}
	if to == StatusOnline {
		// Both stamps describe the FIRST time: connected_at answers "was this student
		// ever in the room?", screen_started_at answers "when did the sharing start?".
		change.MarkConnected = true
		if screen {
			change.MarkScreenStarted = true
		}
	}
	if to == StatusScreenLost {
		change.MarkScreenLost = true
	}
	return change
}

// monitorStudentOf renders one tile.
//
// Two shapes come out of it, and the difference is the whole point of Phase 7:
//
//   - A student with NO session in this run renders as the null tile of §29: no
//     sessionId, no sessionStatus, no media, connection=UNKNOWN. The teacher reads it as
//     "authorized, not here yet", which is exactly what is true.
//   - A student with a session renders as Phase 6 does, except that a terminal session
//     reports no active media and no connection quality. WHY: the teacher must not see a
//     live screen on a session that is over. A participant can linger in the room for a
//     few seconds after a leave (the removal is best effort), and rendering that would
//     make the wall contradict its own status.
func monitorStudentOf(entry RosterEntry, present bool, tracks media.ParticipantTracks) MonitorStudent {
	student := MonitorStudent{
		StudentID:   entry.StudentID,
		DisplayName: entry.DisplayName,
		// UNKNOWN is the starting point, not the fallback: the control plane only
		// claims GOOD for a participant it actually saw publishing a screen, so every
		// other combination (no session, absent, connecting, screen gone, terminal) is
		// honestly "no usable observation".
		Connection: ConnectionUnknown,
	}
	if entry.Session == nil {
		return student
	}

	session := *entry.Session
	sessionID, status := session.ID, session.Status
	student.SessionID = &sessionID
	student.Status = &status
	student.JoinedAt = session.ConnectedAt
	student.LastEventAt = lastEventAt(entry, session)
	if session.Status.Terminal() || !present {
		return student
	}
	student.ScreenActive = tracks.ScreenShare
	student.CameraActive = tracks.Camera
	student.MicrophoneActive = tracks.Microphone
	if tracks.ScreenShare {
		student.Connection = ConnectionGood
	}
	return student
}

// lastEventAt is the timestamp of the most recent RECORDED event of a session.
//
// Phase 8 answers this from `session_events` (the newest row, read by the roster query),
// which is what the field was always named after. `updated_at` remains the fallback for
// a session that has no event rows yet — a session created by an older build, or one
// whose join happened while the event layer was unavailable. Falling back is right:
// `updated_at` moves exactly when the session changes, so it is never a WRONG answer,
// only a coarser one, and a null here would make the wall render "never" for a student
// who is plainly online.
func lastEventAt(entry RosterEntry, session StudentSession) *time.Time {
	if entry.LastEventAt != nil {
		return entry.LastEventAt
	}
	updated := session.UpdatedAt
	if updated.IsZero() {
		return nil
	}
	return &updated
}

// unobservedView renders a wall when the media plane could not be queried.
//
// Nothing is advanced and every connection is UNKNOWN (§33). The screen flag is
// derived from the STORED status instead of the media plane: §21 guarantees
// ONLINE ⇒ a screen track exists, so reporting the last thing the control plane knew
// is more useful than reporting false, and the UNKNOWN connection is what tells the
// frontend that this is a stale picture rather than a fresh observation.
//
// A roster entry without a session renders exactly like the observed case: nulls. There
// is no stored screen to be stale about, so the outage changes nothing for that tile —
// which is the honest answer, not a coincidence.
func unobservedView(roster []RosterEntry) *MonitorView {
	view := make([]MonitorStudent, 0, len(roster))
	for _, entry := range roster {
		student := MonitorStudent{
			StudentID:   entry.StudentID,
			DisplayName: entry.DisplayName,
			Connection:  ConnectionUnknown,
		}
		if entry.Session != nil {
			session := *entry.Session
			sessionID, status := session.ID, session.Status
			student.SessionID = &sessionID
			student.Status = &status
			student.JoinedAt = session.ConnectedAt
			student.LastEventAt = lastEventAt(entry, session)
			student.ScreenActive = !session.Status.Terminal() && session.Status == StatusOnline
		}
		view = append(view, student)
	}
	return &MonitorView{Students: view, MediaObserved: false}
}
