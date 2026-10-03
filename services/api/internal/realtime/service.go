package realtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/session"
)

// Broadcaster is the fan-out port of the runtime layer.
//
// It exists as an interface so the audience rules can be unit tested against a fake
// that simply records what it was asked to send — the alternative would be a test that
// opens real WebSocket connections to observe a scoping bug that is decided three
// layers before the socket.
//
// The two methods are the whole vocabulary: a recipient is either one teacher or a set
// of students. There is no "everyone" (see Hub), which is what makes §26's isolation
// checkable.
type Broadcaster interface {
	// ToStudents delivers to every live connection of these students (user ids).
	ToStudents(students []uuid.UUID, msg Message)
	// ToTeacher delivers to every live connection of one teacher.
	ToTeacher(teacherID uuid.UUID, msg Message)
}

// AudienceView is who may receive a message about one classroom, plus the names the
// envelope needs.
//
// WHY the roster is resolved at broadcast time and not remembered at connect time: a
// student removed from a classroom must stop receiving its events immediately, and a
// student added mid-lesson must start. The roster is the authorization (§14), so asking
// it every time is asking the same question the REST endpoints ask.
type AudienceView struct {
	ClassroomID   uuid.UUID
	ClassroomName string
	// OwnerTeacherID is the teacher whose console receives this classroom's student
	// events (§29).
	OwnerTeacherID uuid.UUID
	// Students are the authorized students, with the display names the teacher-facing
	// messages carry.
	Students []StudentRef
}

// StudentRef is one authorized student as a message needs them.
type StudentRef struct {
	StudentID   uuid.UUID
	DisplayName string
}

// Student looks one student up in the roster.
//
// The second return value is the point: a message about somebody who is NOT in the
// roster must not be sent at all. That is what stops a stale session (a student removed
// from the classroom while their media session is still open) from producing a message
// about a person the teacher does not supervise.
func (v *AudienceView) Student(studentID uuid.UUID) (StudentRef, bool) {
	for _, candidate := range v.Students {
		if candidate.StudentID == studentID {
			return candidate, true
		}
	}
	return StudentRef{}, false
}

// StudentIDs lists the authorized students, for a classroom-wide message.
func (v *AudienceView) StudentIDs() []uuid.UUID {
	if v == nil {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(v.Students))
	for _, candidate := range v.Students {
		ids = append(ids, candidate.StudentID)
	}
	return ids
}

// Audience answers the two authorization questions a broadcast needs.
//
// It is a read-only projection of the classroom domain (classrooms ⋈ classroom_students
// ⋈ users). It deliberately does not re-decide anything: ownership and roster
// membership are what internal/classroom already enforces, and this port reads the same
// columns rather than inventing a second rule.
type Audience interface {
	// ClassroomAudience resolves a classroom by id.
	ClassroomAudience(ctx context.Context, classroomID uuid.UUID) (*AudienceView, error)
	// RunAudience resolves the classroom a run belongs to.
	RunAudience(ctx context.Context, runID uuid.UUID) (*AudienceView, error)
}

// Service turns domain facts into messages and decides who receives them (§47).
//
// # Where the scoping rules live
//
// This type is the only place that knows an audience:
//
//   - ROOM_OPENED goes to the classroom's authorized students (§48).
//   - ROOM_CLOSED goes to the same students AND the owner teacher (§49).
//   - STUDENT_ONLINE / STUDENT_OFFLINE / CAMERA_CHANGED / MIC_CHANGED go to the owner
//     teacher alone. Camera and microphone are media facts about ONE student, and §26
//     keeps a student's browser from learning anything about a classmate.
//   - SCREEN_LOST / SCREEN_RESTORED go to the owner teacher AND to that one student —
//     the student whose own screen it is, addressed by their user id, so no other
//     student's connection can receive it (§26/§47).
//
// A message about a student therefore always takes this path:
// session id → run → classroom → owner + roster → that student's own connection.
type Service struct {
	hub      Broadcaster
	audience Audience
	logger   *slog.Logger
}

// NewService wires the runtime service. hub may be nil in a deployment without a
// WebSocket layer: every method then resolves its audience (so the misses are still
// logged) and sends nothing.
func NewService(hub Broadcaster, audience Audience, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{hub: hub, audience: audience, logger: logger}
}

// RoomOpened announces a new run to the students it authorizes (§48).
//
// WHY the students and not the teacher: the teacher's own console learns about the open
// from the HTTP response it just received. The students are the ones whose dashboard
// has to change without a reload.
func (s *Service) RoomOpened(ctx context.Context, classroomID, runID uuid.UUID) error {
	view, err := s.classroomAudience(ctx, classroomID)
	if err != nil {
		return err
	}
	if view == nil {
		return nil
	}
	msg := newMessage(TypeRoomOpened, map[string]any{
		"classroomId":   view.ClassroomID.String(),
		"classroomName": view.ClassroomName,
		"runId":         runID.String(),
		"openedAt":      nowRFC3339(),
	})
	s.sendToStudents(ctx, view.StudentIDs(), msg, "room_opened", runID)
	return nil
}

// RoomClosed announces the end of a run to its students and its owner (§49).
func (s *Service) RoomClosed(ctx context.Context, classroomID, runID uuid.UUID) error {
	view, err := s.classroomAudience(ctx, classroomID)
	if err != nil {
		return err
	}
	if view == nil {
		return nil
	}
	msg := newMessage(TypeRoomClosed, map[string]any{
		"classroomId": view.ClassroomID.String(),
		"runId":       runID.String(),
		"closedAt":    nowRFC3339(),
	})
	s.sendToStudents(ctx, view.StudentIDs(), msg, "room_closed", runID)
	s.sendToTeacher(ctx, view.OwnerTeacherID, msg, "room_closed", runID)
	return nil
}

// StudentOnline tells the owner that a student is now being supervised (§47).
//
// It is sent on the ONLINE transition — the one caused by an observed SCREEN_SHARE
// track — and not on "the participant connected", because the teacher's wall answers
// "is this student being supervised?" and CONNECTING does not mean yes (§45).
func (s *Service) StudentOnline(ctx context.Context, ref session.SessionRef) error {
	view, student, err := s.resolveStudent(ctx, ref, "student_online")
	if err != nil || view == nil || !student {
		return err
	}
	msg := newMessage(TypeStudentOnline, map[string]any{
		"studentId":   ref.StudentID.String(),
		"displayName": s.displayName(view, ref.StudentID),
		"sessionId":   ref.SessionID.String(),
	})
	s.sendToTeacher(ctx, view.OwnerTeacherID, msg, "student_online", ref.RunID)
	return nil
}

// StudentOffline tells the owner that a student is not being supervised any more, and
// why (DISCONNECTED, LEFT or ROOM_CLOSED §47).
func (s *Service) StudentOffline(ctx context.Context, ref session.SessionRef, reason session.OfflineReason) error {
	view, student, err := s.resolveStudent(ctx, ref, "student_offline")
	if err != nil || view == nil || !student {
		return err
	}
	msg := newMessage(TypeStudentOffline, map[string]any{
		"studentId": ref.StudentID.String(),
		"sessionId": ref.SessionID.String(),
		"reason":    string(reason),
	})
	s.sendToTeacher(ctx, view.OwnerTeacherID, msg, "student_offline", ref.RunID)
	return nil
}

// ScreenLost tells the owner AND the student themself that a screen share stopped
// (§22/§46/§47).
//
// The two audiences get DIFFERENT data on purpose: the teacher needs to know WHICH
// student the tile belongs to, while the student's own page needs nothing but the fact,
// and a student message that named other students would be a §26 leak by construction.
func (s *Service) ScreenLost(ctx context.Context, ref session.SessionRef) error {
	return s.screenChanged(ctx, ref, false)
}

// ScreenRestored tells the owner AND the student themself that the screen is back.
func (s *Service) ScreenRestored(ctx context.Context, ref session.SessionRef) error {
	return s.screenChanged(ctx, ref, true)
}

// CameraChanged tells the owner that a student turned their camera on or off
// (§24/§47/§75).
//
// # Why the audience is the owner and NOT the classroom
//
// §26 is a rule about students, and the camera is the clearest case for it: a classroom
// broadcast would tell every student's browser who in the room has a camera on right now,
// which is a fact about their classmates that this product exists to hide. "Who may see
// this" is answered here and nowhere else — the session domain names the fact
// (Processor.notifyCamera) and cannot widen the audience, because CameraChanged takes a
// ref and no recipient list.
//
// # Why the student themself is not told
//
// They pressed the button: their own page already renders the local camera track, and the
// authoritative answer for their own screen/connection state arrives as SCREEN_LOST /
// SCREEN_RESTORED, which are theirs too. Sending a CAMERA_CHANGED echo would create a
// second, later source of truth for a UI state the browser owns, and a stale echo (delayed
// by a retry, say) could flip the button back. The teacher's wall has the opposite
// problem — it cannot see anybody's button — so it is the one audience that needs the
// message.
func (s *Service) CameraChanged(ctx context.Context, ref session.SessionRef, active bool) error {
	view, student, err := s.resolveStudent(ctx, ref, "camera_changed")
	if err != nil || view == nil || !student {
		return err
	}
	msg := newMessage(TypeCameraChanged, map[string]any{
		"studentId": ref.StudentID.String(),
		"sessionId": ref.SessionID.String(),
		"active":    active,
	})
	s.sendToTeacher(ctx, view.OwnerTeacherID, msg, "camera_changed", ref.RunID)
	return nil
}

func (s *Service) screenChanged(ctx context.Context, ref session.SessionRef, restored bool) error {
	action := "screen_lost"
	kind := TypeScreenLost
	if restored {
		action = "screen_restored"
		kind = TypeScreenRestored
	}
	view, student, err := s.resolveStudent(ctx, ref, action)
	if err != nil || view == nil || !student {
		return err
	}
	s.sendToTeacher(ctx, view.OwnerTeacherID,
		newMessage(kind, map[string]any{
			"studentId": ref.StudentID.String(),
			"sessionId": ref.SessionID.String(),
		}), action, ref.RunID)

	// The student's own copy. It carries the session id and nothing else: not the
	// student's name (they know it), not the teacher's id, and certainly not anything
	// about a classmate.
	s.sendToStudents(ctx, []uuid.UUID{ref.StudentID},
		newMessage(kind, map[string]any{
			"sessionId": ref.SessionID.String(),
		}), action, ref.RunID)
	return nil
}

// resolveStudent loads the classroom a session belongs to and checks that the student
// is on its roster.
//
// The roster check is not decoration: a session can outlive a roster entry (a teacher
// removes a student while their media session is open), and a message about a student
// who is no longer supervised would put a card on the teacher's wall that no roster
// explains. Refusing here is how that stays impossible.
func (s *Service) resolveStudent(ctx context.Context, ref session.SessionRef, action string) (*AudienceView, bool, error) {
	if ref.RunID == uuid.Nil || ref.StudentID == uuid.Nil {
		// A programming error rather than a data condition. Logged, not returned as a
		// business error: the caller has already committed a state change.
		s.logger.Warn("runtime message without a run or student id",
			"action", action, "reason", "incomplete session reference")
		return nil, false, nil
	}
	if s.audience == nil {
		return nil, false, nil
	}
	view, err := s.audience.RunAudience(ctx, ref.RunID)
	if err != nil {
		return nil, false, fmt.Errorf("realtime: resolve audience of run %s: %w", ref.RunID, err)
	}
	if view == nil {
		s.logger.Info("runtime message for a run whose classroom is gone",
			"action", action, "run_id", ref.RunID.String(), "student_id", ref.StudentID.String())
		return nil, false, nil
	}
	if _, ok := view.Student(ref.StudentID); !ok {
		s.logger.Info("runtime message for a student who is not on the classroom roster",
			"action", action,
			"run_id", ref.RunID.String(),
			"classroom_id", view.ClassroomID.String(),
			"student_id", ref.StudentID.String(),
		)
		return nil, false, nil
	}
	return view, true, nil
}

func (s *Service) displayName(view *AudienceView, studentID uuid.UUID) string {
	if ref, ok := view.Student(studentID); ok {
		return ref.DisplayName
	}
	return ""
}

func (s *Service) classroomAudience(ctx context.Context, classroomID uuid.UUID) (*AudienceView, error) {
	if s.audience == nil || classroomID == uuid.Nil {
		return nil, nil
	}
	view, err := s.audience.ClassroomAudience(ctx, classroomID)
	if err != nil {
		return nil, fmt.Errorf("realtime: resolve audience of classroom %s: %w", classroomID, err)
	}
	if view == nil {
		s.logger.Warn("runtime message for an unknown classroom", "classroom_id", classroomID.String())
	}
	return view, nil
}

// sendToStudents delivers to a set of students, logging the size of the fan-out.
//
// No error is returned: a broadcast with no live connections is normal (nobody is
// looking at their dashboard), and the Hub cannot fail — a per-connection problem is
// handled where it happens (slow consumer → that connection is dropped).
func (s *Service) sendToStudents(ctx context.Context, students []uuid.UUID, msg Message, action string, runID uuid.UUID) {
	if s.hub == nil || len(students) == 0 {
		return
	}
	s.hub.ToStudents(students, msg)
	s.logger.Debug("runtime message published",
		"action", action,
		"audience", "students",
		"recipients", len(students),
		"message_type", string(msg.Type),
		"run_id", runID.String(),
	)
}

func (s *Service) sendToTeacher(ctx context.Context, teacherID uuid.UUID, msg Message, action string, runID uuid.UUID) {
	if s.hub == nil || teacherID == uuid.Nil {
		return
	}
	s.hub.ToTeacher(teacherID, msg)
	s.logger.Debug("runtime message published",
		"action", action,
		"audience", "teacher",
		"message_type", string(msg.Type),
		"run_id", runID.String(),
	)
}

// Compile-time assertion: the runtime service is what the session domain talks to.
var _ session.SessionEvents = (*Service)(nil)
