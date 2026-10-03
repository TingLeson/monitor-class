package realtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/session"
)

// The audience rules of §26/§47, against a fake broadcaster and a fake roster.
//
// Every test here answers the same question from a different side: WHO is allowed to
// receive this message? The hub's own tests prove that a connection only receives what is
// addressed to it; these prove that the service addresses the right people in the first
// place — which is where a leak would actually be introduced (a message addressed to a
// classroom instead of to one student).

// delivered is one recorded broadcast.
type delivered struct {
	audience string // "students" | "teacher"
	students []uuid.UUID
	teacher  uuid.UUID
	message  Message
}

// fakeBroadcaster records instead of writing to sockets.
type fakeBroadcaster struct {
	sent []delivered
}

func (f *fakeBroadcaster) ToStudents(students []uuid.UUID, msg Message) {
	copied := append([]uuid.UUID(nil), students...)
	f.sent = append(f.sent, delivered{audience: "students", students: copied, message: msg})
}

func (f *fakeBroadcaster) ToTeacher(teacherID uuid.UUID, msg Message) {
	f.sent = append(f.sent, delivered{audience: "teacher", teacher: teacherID, message: msg})
}

// to reports every message of one type addressed to a student, flattened to student ids.
func (f *fakeBroadcaster) studentsWith(kind MessageType) []uuid.UUID {
	var ids []uuid.UUID
	for _, message := range f.sent {
		if message.audience == "students" && message.message.Type == kind {
			ids = append(ids, message.students...)
		}
	}
	return ids
}

func (f *fakeBroadcaster) teacherMessages(kind MessageType) []Message {
	var messages []Message
	for _, message := range f.sent {
		if message.audience == "teacher" && message.message.Type == kind {
			messages = append(messages, message.message)
		}
	}
	return messages
}

// fakeAudience answers the roster questions from memory.
type fakeAudience struct {
	byClassroom map[uuid.UUID]*AudienceView
	byRun       map[uuid.UUID]*AudienceView
	err         error
}

func newFakeAudience() *fakeAudience {
	return &fakeAudience{byClassroom: map[uuid.UUID]*AudienceView{}, byRun: map[uuid.UUID]*AudienceView{}}
}

func (f *fakeAudience) seed(classroomID uuid.UUID, name string, owner uuid.UUID, students ...StudentRef) *AudienceView {
	view := &AudienceView{ClassroomID: classroomID, ClassroomName: name, OwnerTeacherID: owner, Students: students}
	f.byClassroom[classroomID] = view
	return view
}

func (f *fakeAudience) forRun(runID uuid.UUID, view *AudienceView) {
	f.byRun[runID] = view
}

func (f *fakeAudience) ClassroomAudience(_ context.Context, classroomID uuid.UUID) (*AudienceView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byClassroom[classroomID], nil
}

func (f *fakeAudience) RunAudience(_ context.Context, runID uuid.UUID) (*AudienceView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRun[runID], nil
}

func newServiceUnderTest() (*Service, *fakeBroadcaster, *fakeAudience) {
	hub := &fakeBroadcaster{}
	audience := newFakeAudience()
	return NewService(hub, audience, slog.New(slog.NewTextHandler(io.Discard, nil))), hub, audience
}

// ---------------------------------------------------------------------------
// Classroom-wide messages
// ---------------------------------------------------------------------------

func TestRoomOpenedReachesExactlyTheAuthorizedStudents(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice, bob := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}, StudentRef{StudentID: uuid.New(), DisplayName: "李四"}
	audience.seed(classroomID, "C++ 算法训练", owner, alice, bob)

	if err := service.RoomOpened(ctx, classroomID, runID); err != nil {
		t.Fatalf("RoomOpened(): %v", err)
	}

	got := hub.studentsWith(TypeRoomOpened)
	if len(got) != 2 || got[0] != alice.StudentID || got[1] != bob.StudentID {
		t.Fatalf("recipients = %v, want the two authorized students", got)
	}
	// The teacher learns about the open from the HTTP response; the socket is for the
	// students whose dashboard has to change.
	if len(hub.teacherMessages(TypeRoomOpened)) != 0 {
		t.Fatal("ROOM_OPENED was sent to the teacher")
	}
	message := hub.sent[0].message
	if message.Data["classroomId"] != classroomID.String() ||
		message.Data["classroomName"] != "C++ 算法训练" ||
		message.Data["runId"] != runID.String() {
		t.Fatalf("data = %+v", message.Data)
	}
	openedAt, ok := message.Data["openedAt"].(string)
	if !ok {
		t.Fatalf("openedAt is not a string: %v", message.Data["openedAt"])
	}
	if _, err := time.Parse(time.RFC3339, openedAt); err != nil {
		t.Fatalf("openedAt = %q, want RFC3339", openedAt)
	}
}

func TestRoomClosedReachesStudentsAndTheOwner(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	audience.seed(classroomID, "C++ 算法训练", owner, alice)

	if err := service.RoomClosed(ctx, classroomID, runID); err != nil {
		t.Fatalf("RoomClosed(): %v", err)
	}
	if got := hub.studentsWith(TypeRoomClosed); len(got) != 1 || got[0] != alice.StudentID {
		t.Fatalf("student recipients = %v", got)
	}
	teacherMessages := hub.teacherMessages(TypeRoomClosed)
	if len(teacherMessages) != 1 {
		t.Fatalf("teacher messages = %d, want 1", len(teacherMessages))
	}
	if teacherMessages[0].Data["runId"] != runID.String() {
		t.Fatalf("data = %+v", teacherMessages[0].Data)
	}
}

func TestClassroomMessagesToAnEmptyRosterSendNothing(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	classroomID := uuid.New()
	audience.seed(classroomID, "空课堂", uuid.New())

	if err := service.RoomOpened(context.Background(), classroomID, uuid.New()); err != nil {
		t.Fatalf("RoomOpened(): %v", err)
	}
	if len(hub.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing: there is nobody to tell", hub.sent)
	}
}

func TestUnknownClassroomSendsNothingAndIsNotAnError(t *testing.T) {
	service, hub, _ := newServiceUnderTest()
	if err := service.RoomClosed(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("RoomClosed(): %v", err)
	}
	if len(hub.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing", hub.sent)
	}
}

// ---------------------------------------------------------------------------
// Student-scoped messages
// ---------------------------------------------------------------------------

func TestStudentOnlineGoesToTheOwnerOnly(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	view := audience.seed(classroomID, "C++ 算法训练", owner, alice)
	audience.forRun(runID, view)

	ref := session.SessionRef{SessionID: uuid.New(), StudentID: alice.StudentID, RunID: runID}
	if err := service.StudentOnline(ctx, ref); err != nil {
		t.Fatalf("StudentOnline(): %v", err)
	}

	messages := hub.teacherMessages(TypeStudentOnline)
	if len(messages) != 1 {
		t.Fatalf("teacher messages = %d, want 1", len(messages))
	}
	if hub.sent[0].teacher != owner {
		t.Fatalf("sent to %s, want the owner %s", hub.sent[0].teacher, owner)
	}
	data := messages[0].Data
	if data["studentId"] != alice.StudentID.String() ||
		data["displayName"] != "张三" ||
		data["sessionId"] != ref.SessionID.String() {
		t.Fatalf("data = %+v", data)
	}
	// No student gets a message about another student's arrival — including this one.
	if len(hub.studentsWith(TypeStudentOnline)) != 0 {
		t.Fatal("STUDENT_ONLINE reached a student connection")
	}
}

func TestStudentOfflineCarriesTheReasonToTheOwner(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	audience.forRun(runID, audience.seed(classroomID, "C++", owner, alice))

	ref := session.SessionRef{SessionID: uuid.New(), StudentID: alice.StudentID, RunID: runID}
	for _, reason := range []session.OfflineReason{session.OfflineDisconnected, session.OfflineLeft, session.OfflineRoomClosed} {
		if err := service.StudentOffline(ctx, ref, reason); err != nil {
			t.Fatalf("StudentOffline(%s): %v", reason, err)
		}
	}
	messages := hub.teacherMessages(TypeStudentOffline)
	if len(messages) != 3 {
		t.Fatalf("teacher messages = %d, want 3", len(messages))
	}
	reasons := []string{"DISCONNECTED", "LEFT", "ROOM_CLOSED"}
	for i, message := range messages {
		if message.Data["reason"] != reasons[i] {
			t.Fatalf("reason = %v, want %s", message.Data["reason"], reasons[i])
		}
		if message.Data["studentId"] != alice.StudentID.String() {
			t.Fatalf("data = %+v", message.Data)
		}
	}
}

func TestScreenLostReachesTheOwnerAndThatStudentOnly(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	bob := StudentRef{StudentID: uuid.New(), DisplayName: "李四"}
	audience.forRun(runID, audience.seed(classroomID, "C++", owner, alice, bob))

	ref := session.SessionRef{SessionID: uuid.New(), StudentID: alice.StudentID, RunID: runID}
	if err := service.ScreenLost(ctx, ref); err != nil {
		t.Fatalf("ScreenLost(): %v", err)
	}

	// The teacher's copy names the student (their wall is a grid of people)...
	messages := hub.teacherMessages(TypeScreenLost)
	if len(messages) != 1 {
		t.Fatalf("teacher messages = %d, want 1", len(messages))
	}
	if messages[0].Data["studentId"] != alice.StudentID.String() ||
		messages[0].Data["sessionId"] != ref.SessionID.String() {
		t.Fatalf("teacher data = %+v", messages[0].Data)
	}

	// ... and the student's own copy names nobody: not themselves, and above all not a
	// classmate (§26).
	studentCopies := hub.studentsWith(TypeScreenLost)
	if len(studentCopies) != 1 || studentCopies[0] != alice.StudentID {
		t.Fatalf("student recipients = %v, want exactly the student whose screen it is", studentCopies)
	}
	var studentData map[string]any
	for _, message := range hub.sent {
		if message.audience == "students" && message.message.Type == TypeScreenLost {
			studentData = message.message.Data
		}
	}
	if len(studentData) != 1 || studentData["sessionId"] != ref.SessionID.String() {
		t.Fatalf("student data = %+v, want only the session id", studentData)
	}
}

func TestScreenRestoredUsesTheSameAudiences(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	audience.forRun(runID, audience.seed(classroomID, "C++", owner, alice))

	ref := session.SessionRef{SessionID: uuid.New(), StudentID: alice.StudentID, RunID: runID}
	if err := service.ScreenRestored(ctx, ref); err != nil {
		t.Fatalf("ScreenRestored(): %v", err)
	}
	if len(hub.teacherMessages(TypeScreenRestored)) != 1 {
		t.Fatal("the owner did not receive SCREEN_RESTORED")
	}
	recipients := hub.studentsWith(TypeScreenRestored)
	if len(recipients) != 1 || recipients[0] != alice.StudentID {
		t.Fatalf("student recipients = %v", recipients)
	}
}

func TestAMessageAboutSomebodyOffTheRosterIsNotSent(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	ctx := context.Background()

	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	audience.forRun(runID, audience.seed(classroomID, "C++", owner, alice))

	// A session can outlive a roster entry (a teacher removes a student while their
	// media session is still open). The teacher's wall must not grow a card that no
	// roster explains.
	stranger := session.SessionRef{SessionID: uuid.New(), StudentID: uuid.New(), RunID: runID}
	for _, call := range []func() error{
		func() error { return service.StudentOnline(ctx, stranger) },
		func() error { return service.StudentOffline(ctx, stranger, session.OfflineDisconnected) },
		func() error { return service.ScreenLost(ctx, stranger) },
		func() error { return service.ScreenRestored(ctx, stranger) },
	} {
		if err := call(); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	if len(hub.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing for a student who is not on the roster", hub.sent)
	}
}

func TestAudienceFailureIsReportedAndNothingIsSent(t *testing.T) {
	service, hub, audience := newServiceUnderTest()
	audience.err = errors.New("database is down")

	ref := session.SessionRef{SessionID: uuid.New(), StudentID: uuid.New(), RunID: uuid.New()}
	if err := service.ScreenLost(context.Background(), ref); err == nil {
		t.Fatal("ScreenLost() returned nil: the caller has to be able to log the failure")
	}
	if len(hub.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing when the audience could not be resolved", hub.sent)
	}
}

func TestIncompleteReferenceIsIgnored(t *testing.T) {
	service, hub, _ := newServiceUnderTest()
	if err := service.StudentOnline(context.Background(), session.SessionRef{}); err != nil {
		t.Fatalf("StudentOnline(): %v", err)
	}
	if len(hub.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing", hub.sent)
	}
}

func TestServiceWithoutAHubStillResolvesTheAudience(t *testing.T) {
	audience := newFakeAudience()
	service := NewService(nil, audience, slog.New(slog.NewTextHandler(io.Discard, nil)))
	classroomID, runID, owner := uuid.New(), uuid.New(), uuid.New()
	alice := StudentRef{StudentID: uuid.New(), DisplayName: "张三"}
	audience.forRun(runID, audience.seed(classroomID, "C++", owner, alice))

	// A deployment without a hub must not fail a state change; it simply has nobody to
	// tell. The call is a no-op, not a panic.
	if err := service.ScreenLost(context.Background(),
		session.SessionRef{SessionID: uuid.New(), StudentID: alice.StudentID, RunID: runID}); err != nil {
		t.Fatalf("ScreenLost(): %v", err)
	}
}
