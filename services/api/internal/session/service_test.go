package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/classroom"
	"github.com/classwatch/classwatch/services/api/internal/media"
)

// The session rules of §43/§45/§49/§50/§51, against in-memory fakes.
//
// Three fakes stand in for the three things this package talks to, and each one
// mirrors the CONTRACT of its real counterpart rather than a convenient
// approximation: the repository reuses active sessions the way the partial unique
// index does, the classroom directory answers through a roster, and the media client
// records the token request it was asked to sign — which is where the permission bits
// of §27/§28 are actually asserted, because a token's grants are the security-relevant
// part of this phase.
//
// The end-to-end version (real PostgreSQL, real sessions) is in
// internal/httpapi/student_sessions_integration_test.go.

// ---------------------------------------------------------------------------
// Fake classroom directory
// ---------------------------------------------------------------------------

type fakeDirectory struct {
	classrooms map[uuid.UUID]*classroom.Classroom
	// rosters is keyed by classroom id and holds authorized student ids.
	rosters map[uuid.UUID]map[uuid.UUID]bool
	runs    map[uuid.UUID]*classroom.Run

	entryErr error
	getErr   error
	runErr   error
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{
		classrooms: map[uuid.UUID]*classroom.Classroom{},
		rosters:    map[uuid.UUID]map[uuid.UUID]bool{},
		runs:       map[uuid.UUID]*classroom.Run{},
	}
}

// seedOpen inserts an OPEN classroom with a run and authorizes the students.
func (f *fakeDirectory) seedOpen(classroomID, teacherID uuid.UUID, runID uuid.UUID, students ...uuid.UUID) *classroom.Classroom {
	run := &classroom.Run{
		ID:              runID,
		ClassroomID:     classroomID,
		Status:          classroom.StatusOpen,
		LiveKitRoomName: classroom.RoomName(runID),
		OpenedAt:        time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC),
	}
	c := &classroom.Classroom{
		ID:             classroomID,
		Name:           "C++ 算法训练",
		OwnerTeacherID: teacherID,
		Status:         classroom.StatusOpen,
		CurrentRunID:   &runID,
		CurrentRun:     run,
		CreatedAt:      time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
	}
	f.classrooms[classroomID] = c
	f.runs[runID] = run
	f.rosters[classroomID] = map[uuid.UUID]bool{}
	for _, studentID := range students {
		f.rosters[classroomID][studentID] = true
	}
	return c
}

// seedClosed inserts a CLOSED classroom, optionally with a roster.
func (f *fakeDirectory) seedClosed(classroomID, teacherID uuid.UUID, students ...uuid.UUID) *classroom.Classroom {
	c := &classroom.Classroom{
		ID:             classroomID,
		Name:           "数据结构练习",
		OwnerTeacherID: teacherID,
		Status:         classroom.StatusClosed,
		CreatedAt:      time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
	}
	f.classrooms[classroomID] = c
	f.rosters[classroomID] = map[uuid.UUID]bool{}
	for _, studentID := range students {
		f.rosters[classroomID][studentID] = true
	}
	return c
}

func (f *fakeDirectory) StudentEntry(_ context.Context, studentID, classroomID uuid.UUID) (*classroom.StudentEntry, error) {
	if f.entryErr != nil {
		return nil, f.entryErr
	}
	c, ok := f.classrooms[classroomID]
	if !ok || !f.rosters[classroomID][studentID] {
		return nil, classroom.ErrStudentNotAssigned
	}
	entry := &classroom.StudentEntry{ClassroomID: c.ID, Status: c.Status}
	if c.CurrentRunID != nil {
		run := *f.runs[*c.CurrentRunID]
		entry.Run = &run
	}
	return entry, nil
}

func (f *fakeDirectory) Get(_ context.Context, classroomID, teacherID uuid.UUID) (*classroom.Classroom, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.classrooms[classroomID]
	if !ok {
		return nil, classroom.ErrNotFound
	}
	if c.OwnerTeacherID != teacherID {
		return nil, classroom.ErrNotOwner
	}
	return c, nil
}

func (f *fakeDirectory) RunByID(_ context.Context, runID uuid.UUID) (*classroom.Run, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	run, ok := f.runs[runID]
	if !ok {
		return nil, classroom.ErrNotFound
	}
	copied := *run
	return &copied, nil
}

// ---------------------------------------------------------------------------
// Fake media plane
// ---------------------------------------------------------------------------

type fakeMedia struct {
	ensureCalls   []string
	ensureErr     error
	observed      map[string]media.ParticipantTracks
	observeErr    error
	observeCalls  []string
	removed       [][2]string
	removeErr     error
	tokenRequests []media.TokenRequest
	signErr       error
	// token is what SignToken returns; it is deliberately identifiable so a test can
	// assert it never reaches a log line.
	token string

	// enforcement is the §26 half of the fake. It mirrors the contract of the real
	// client (revoke, report what was revoked, never fail the caller) without mirroring
	// its bookkeeping: the idempotency of the real reconciler is tested in
	// internal/media, where a fake RoomService can count RPCs.
	enforcementCalls []enforcementCall
	enforcementErr   error
	// revoked is what a pass reports as actually revoked. Tests that care about the log
	// line script it; by default the fake reports nothing.
	revoked []media.PeerSubscriptionRevocation
}

// enforcementCall is one EnforceNoPeerSubscriptions invocation, recorded so a test can
// assert WHICH students were reconciled and WHAT was whitelisted.
type enforcementCall struct {
	room     string
	students []string
	observed map[string]media.ParticipantTracks
	allowed  []string
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{observed: map[string]media.ParticipantTracks{}, token: "header.payload.signature"}
}

func (m *fakeMedia) EnsureRoom(_ context.Context, roomName string) error {
	m.ensureCalls = append(m.ensureCalls, roomName)
	return m.ensureErr
}

func (m *fakeMedia) ObserveRoom(_ context.Context, roomName string) (map[string]media.ParticipantTracks, error) {
	m.observeCalls = append(m.observeCalls, roomName)
	if m.observeErr != nil {
		return nil, m.observeErr
	}
	return m.observed, nil
}

func (m *fakeMedia) RemoveParticipant(_ context.Context, roomName, identity string) error {
	m.removed = append(m.removed, [2]string{roomName, identity})
	return m.removeErr
}

func (m *fakeMedia) EnforceNoPeerSubscriptions(
	_ context.Context,
	roomName string,
	students []string,
	observed map[string]media.ParticipantTracks,
	allowedTrackOwners []string,
) ([]media.PeerSubscriptionRevocation, error) {
	m.enforcementCalls = append(m.enforcementCalls, enforcementCall{
		room: roomName, students: students, observed: observed, allowed: allowedTrackOwners,
	})
	if m.enforcementErr != nil {
		return nil, m.enforcementErr
	}
	return m.revoked, nil
}

func (m *fakeMedia) SignToken(req media.TokenRequest) (string, error) {
	m.tokenRequests = append(m.tokenRequests, req)
	if m.signErr != nil {
		return "", m.signErr
	}
	return m.token, nil
}

// ---------------------------------------------------------------------------
// Fake session repository
// ---------------------------------------------------------------------------

// rosterMember is one authorized student of a classroom, as the roster read returns it.
type rosterMember struct {
	studentID   uuid.UUID
	account     string
	displayName string
}

type fakeRepo struct {
	sessions map[uuid.UUID]*StudentSession
	names    map[uuid.UUID]string
	// roster is keyed by classroom id. The members are stored in insertion order and
	// SORTED by the read, mirroring the production ORDER BY u.account: the fake must sort
	// too, or the stability of §29's tile order would be untested.
	roster map[uuid.UUID][]rosterMember
	clock  time.Time

	createErr  error
	leaveErr   error
	listErr    error
	observeErr error
	// outdated makes ApplyObservation answer "the row moved on" (nil, nil), which is
	// what the real repository does when the compare-and-set guard does not match.
	outdated bool

	creates      int
	reuses       int
	leaves       [][2]uuid.UUID
	observations []ObservationChange
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		sessions: map[uuid.UUID]*StudentSession{},
		names:    map[uuid.UUID]string{},
		roster:   map[uuid.UUID][]rosterMember{},
		clock:    time.Date(2026, 3, 4, 19, 0, 0, 0, time.UTC),
	}
}

// authorize puts a student on a classroom's roster. The account is what the wall orders
// by, which is why every test that cares about tile order has to set it.
func (f *fakeRepo) authorize(classroomID, studentID uuid.UUID, account, displayName string) {
	f.roster[classroomID] = append(f.roster[classroomID], rosterMember{
		studentID: studentID, account: account, displayName: displayName,
	})
	f.names[studentID] = displayName
}

func (f *fakeRepo) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

// seed inserts a session directly, bypassing the rules (like manual SQL would).
func (f *fakeRepo) seed(runID, studentID uuid.UUID, status Status, displayName string) *StudentSession {
	id := uuid.New()
	session := &StudentSession{
		ID:              id,
		ClassroomRunID:  runID,
		StudentID:       studentID,
		LiveKitIdentity: id.String(),
		Status:          status,
		CreatedAt:       f.tick(),
		UpdatedAt:       f.tick(),
	}
	f.sessions[id] = session
	f.names[studentID] = displayName
	return session
}

// CreateOrReuse mirrors the production statement: the active session of (run,
// student) is reused and reset to CONNECTING, otherwise a new row is inserted with
// its identity equal to its id.
func (f *fakeRepo) CreateOrReuse(_ context.Context, params CreateOrReuseParams) (*StudentSession, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	for _, session := range f.sessions {
		if session.ClassroomRunID == params.ClassroomRunID && session.StudentID == params.StudentID && session.Status.Active() {
			f.reuses++
			session.Status = StatusConnecting
			session.LeftAt = nil
			session.UpdatedAt = f.tick()
			copied := *session
			return &copied, nil
		}
	}
	f.creates++
	session := &StudentSession{
		ID:              params.SessionID,
		ClassroomRunID:  params.ClassroomRunID,
		StudentID:       params.StudentID,
		LiveKitIdentity: params.SessionID.String(),
		Status:          StatusConnecting,
		CreatedAt:       f.tick(),
		UpdatedAt:       f.tick(),
	}
	f.sessions[session.ID] = session
	copied := *session
	return &copied, nil
}

func (f *fakeRepo) Leave(_ context.Context, sessionID, studentID uuid.UUID) (*StudentSession, error) {
	f.leaves = append(f.leaves, [2]uuid.UUID{sessionID, studentID})
	if f.leaveErr != nil {
		return nil, f.leaveErr
	}
	session, ok := f.sessions[sessionID]
	if !ok || session.StudentID != studentID {
		return nil, ErrSessionNotFound
	}
	if session.Status != StatusLeft {
		now := f.tick()
		session.Status = StatusLeft
		session.LeftAt = &now
	}
	session.UpdatedAt = f.tick()
	copied := *session
	return &copied, nil
}

// ListRosterByRun mirrors the production read: every authorized student, ordered by
// account, LEFT JOINed onto the one session that answers "what are they doing in this
// run?" — an active session if there is one, otherwise the newest.
func (f *fakeRepo) ListRosterByRun(_ context.Context, classroomID, runID uuid.UUID) ([]RosterEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	members := append([]rosterMember{}, f.roster[classroomID]...)
	sort.Slice(members, func(i, j int) bool {
		if members[i].account != members[j].account {
			return members[i].account < members[j].account
		}
		return members[i].studentID.String() < members[j].studentID.String()
	})

	out := make([]RosterEntry, 0, len(members))
	for _, member := range members {
		entry := RosterEntry{StudentID: member.studentID, DisplayName: member.displayName}
		if session := f.sessionFor(runID, member.studentID); session != nil {
			entry.Session = session
		}
		out = append(out, entry)
	}
	return out, nil
}

// sessionFor is the LATERAL subquery of the production read, in Go.
func (f *fakeRepo) sessionFor(runID, studentID uuid.UUID) *StudentSession {
	var best *StudentSession
	for _, session := range f.sessions {
		if session.ClassroomRunID != runID || session.StudentID != studentID {
			continue
		}
		if best == nil {
			best = session
			continue
		}
		switch {
		case session.Status.Active() != best.Status.Active():
			// An active session beats a terminal one: a student who left and came back
			// must be shown as present, not as LEFT.
			if session.Status.Active() {
				best = session
			}
		case session.CreatedAt.After(best.CreatedAt),
			session.CreatedAt.Equal(best.CreatedAt) && session.ID.String() > best.ID.String():
			best = session
		}
	}
	if best == nil {
		return nil
	}
	copied := *best
	return &copied
}

func (f *fakeRepo) ApplyObservation(_ context.Context, change ObservationChange) (*StudentSession, error) {
	f.observations = append(f.observations, change)
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	session, ok := f.sessions[change.SessionID]
	if !ok || f.outdated || session.Status != change.From {
		return nil, nil
	}
	now := f.tick()
	session.Status = change.To
	if change.MarkConnected && session.ConnectedAt == nil {
		session.ConnectedAt = &now
	}
	if change.MarkScreenStarted && session.ScreenStartedAt == nil {
		session.ScreenStartedAt = &now
	}
	if change.MarkScreenLost {
		session.ScreenLostAt = &now
	}
	session.UpdatedAt = now
	copied := *session
	return &copied, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	service   *Service
	repo      *fakeRepo
	directory *fakeDirectory
	media     *fakeMedia
	logs      *bytes.Buffer

	teacherID   uuid.UUID
	studentID   uuid.UUID
	otherID     uuid.UUID
	classroomID uuid.UUID
	runID       uuid.UUID

	cfg Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := newFakeRepo()
	directory := newFakeDirectory()
	mediaPlane := newFakeMedia()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg := Config{LiveKitURL: "wss://media.example.test", TokenTTL: 2 * time.Hour}
	h := &harness{
		service:   NewService(repo, directory, mediaPlane, cfg),
		repo:      repo,
		directory: directory,
		media:     mediaPlane,
		logs:      logs,
		teacherID: uuid.New(),
		studentID: uuid.New(),
		otherID:   uuid.New(),
		cfg:       cfg,
	}
	h.classroomID = uuid.New()
	h.runID = uuid.New()
	directory.seedOpen(h.classroomID, h.teacherID, h.runID, h.studentID)
	// The roster is one table in production (classroom_students) and two fakes here: the
	// classroom directory is what the JOIN path authorizes against, the session
	// repository is what the monitoring wall lists. Seeding both is what keeps "may this
	// student enter?" and "does the wall show them?" from drifting apart in the tests.
	h.addStudent(h.studentID, "S10086", "学生 A")
	return h
}

// addStudent authorizes one more student for the harness classroom.
func (h *harness) addStudent(studentID uuid.UUID, account, displayName string) {
	h.directory.rosters[h.classroomID][studentID] = true
	h.repo.authorize(h.classroomID, studentID, account, displayName)
}

// statusOf dereferences a tile's session status, failing the test when the tile has no
// session at all: the two cases are asserted separately on purpose, because "no session"
// and "a session in some state" are different product answers (§29).
func statusOf(t *testing.T, student MonitorStudent) Status {
	t.Helper()
	if student.Status == nil {
		t.Fatalf("tile %s has no sessionStatus, want one", student.StudentID)
	}
	return *student.Status
}

// sessionIDOf dereferences a tile's session id.
func sessionIDOf(t *testing.T, student MonitorStudent) uuid.UUID {
	t.Helper()
	if student.SessionID == nil {
		t.Fatalf("tile %s has no sessionId, want one", student.StudentID)
	}
	return *student.SessionID
}

// tileOf finds the tile of one student.
func tileOf(t *testing.T, view *MonitorView, studentID uuid.UUID) MonitorStudent {
	t.Helper()
	for _, student := range view.Students {
		if student.StudentID == studentID {
			return student
		}
	}
	t.Fatalf("no tile for student %s in %+v", studentID, view.Students)
	return MonitorStudent{}
}

// run is the room of the seeded run.
func (h *harness) run() *classroom.Run { return h.directory.runs[h.runID] }

func (h *harness) join(t *testing.T) *JoinResult {
	t.Helper()
	result, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: h.classroomID})
	if err != nil {
		t.Fatalf("Join(): %v", err)
	}
	return result
}

// ---------------------------------------------------------------------------
// Join (§43)
// ---------------------------------------------------------------------------

// TestJoinRejectsUnauthorizedStudent is the §14/§43 rule: a student who is not on the
// roster learns nothing, and no session, room or token is created.
func TestJoinRejectsUnauthorizedStudent(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Join(context.Background(), JoinInput{StudentID: h.otherID, ClassroomID: h.classroomID})
	if !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Fatalf("err = %v, want ErrStudentNotAssigned", err)
	}
	if h.repo.creates != 0 || h.repo.reuses != 0 {
		t.Errorf("a session was created for an unauthorized student")
	}
	if len(h.media.ensureCalls) != 0 || len(h.media.tokenRequests) != 0 {
		t.Errorf("the media plane was contacted for an unauthorized student: %v / %v",
			h.media.ensureCalls, h.media.tokenRequests)
	}
}

// TestJoinRejectsUnknownClassroom collapses "no such classroom" and "not yours" into
// the same answer: the id is the only thing the caller supplied.
func TestJoinRejectsUnknownClassroom(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: uuid.New()})
	if !errors.Is(err, classroom.ErrStudentNotAssigned) {
		t.Fatalf("err = %v, want ErrStudentNotAssigned", err)
	}
}

// TestJoinRejectsClosedClassroom covers both shapes of "not running": a CLOSED
// classroom, and an OPEN one whose run is missing (impossible in the database, but
// this is the code that would have to cope).
func TestJoinRejectsClosedClassroom(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		h := newHarness(t)
		closedID := uuid.New()
		h.directory.seedClosed(closedID, h.teacherID, h.studentID)
		_, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: closedID})
		if !errors.Is(err, ErrClassroomClosed) {
			t.Fatalf("err = %v, want ErrClassroomClosed", err)
		}
		if h.repo.creates != 0 {
			t.Errorf("a session was created for a closed classroom")
		}
	})

	t.Run("open without a run", func(t *testing.T) {
		h := newHarness(t)
		h.directory.classrooms[h.classroomID].CurrentRunID = nil
		_, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: h.classroomID})
		if !errors.Is(err, ErrClassroomClosed) {
			t.Fatalf("err = %v, want ErrClassroomClosed", err)
		}
	})
}

// TestJoinRejectsNilIdentifiers keeps a missing actor from behaving like a wildcard.
func TestJoinRejectsNilIdentifiers(t *testing.T) {
	h := newHarness(t)
	for name, in := range map[string]JoinInput{
		"nil student":   {StudentID: uuid.Nil, ClassroomID: h.classroomID},
		"nil classroom": {StudentID: h.studentID, ClassroomID: uuid.Nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.service.Join(context.Background(), in); !errors.Is(err, classroom.ErrStudentNotAssigned) {
				t.Fatalf("err = %v, want ErrStudentNotAssigned", err)
			}
		})
	}
}

// TestJoinSuccessMintsScreenOnlyToken is the §28 assertion: identity is the session
// id, the room is the run's opaque room, and the only publishable source is the screen.
func TestJoinSuccessMintsScreenOnlyToken(t *testing.T) {
	h := newHarness(t)
	result := h.join(t)

	if result.Session.Status != StatusConnecting {
		t.Errorf("session status = %s, want CONNECTING", result.Session.Status)
	}
	if result.Session.LiveKitIdentity != result.Session.ID.String() {
		t.Errorf("identity = %q, want the session id %s (§44)", result.Session.LiveKitIdentity, result.Session.ID)
	}
	if result.LiveKitURL != h.cfg.LiveKitURL {
		t.Errorf("livekitUrl = %q, want %q", result.LiveKitURL, h.cfg.LiveKitURL)
	}
	if result.Token != h.media.token {
		t.Errorf("token = %q, want the media plane's token", result.Token)
	}

	// The room must exist before the token is usable, so EnsureRoom comes first.
	if len(h.media.ensureCalls) != 1 || h.media.ensureCalls[0] != h.run().LiveKitRoomName {
		t.Fatalf("EnsureRoom calls = %v, want one call with %q", h.media.ensureCalls, h.run().LiveKitRoomName)
	}
	if len(h.media.tokenRequests) != 1 {
		t.Fatalf("SignToken calls = %d, want 1", len(h.media.tokenRequests))
	}
	req := h.media.tokenRequests[0]
	if req.Identity != result.Session.ID.String() {
		t.Errorf("token identity = %q, want %s", req.Identity, result.Session.ID)
	}
	if req.RoomName != h.run().LiveKitRoomName {
		t.Errorf("token room = %q, want %q", req.RoomName, h.run().LiveKitRoomName)
	}
	if req.TTL != h.cfg.TokenTTL {
		t.Errorf("token ttl = %s, want %s", req.TTL, h.cfg.TokenTTL)
	}
	if !req.CanPublish {
		t.Error("canPublish = false, want true (§28: a student publishes their screen)")
	}
	if !req.CanSubscribe {
		t.Error("canSubscribe = false, want true (§28: the teacher's private audio needs it)")
	}
	if req.CanPublishData {
		t.Error("canPublishData = true, want false (§47: business messages are not on the data channel)")
	}
	if len(req.PublishSources) != 1 || req.PublishSources[0] != media.PublishScreenShare {
		t.Errorf("publish sources = %v, want [SCREEN_SHARE] only (§72)", req.PublishSources)
	}
}

// TestJoinReusesActiveSession is §50: a refresh must not create a second session, and
// the reused row keeps the identity LiveKit kicks the old connection with.
func TestJoinReusesActiveSession(t *testing.T) {
	h := newHarness(t)
	first := h.join(t)
	second := h.join(t)

	if first.Session.ID != second.Session.ID {
		t.Fatalf("second join created session %s, want the first one %s", second.Session.ID, first.Session.ID)
	}
	if second.Session.LiveKitIdentity != first.Session.LiveKitIdentity {
		t.Errorf("identity changed across rejoin: %q → %q", first.Session.LiveKitIdentity, second.Session.LiveKitIdentity)
	}
	if h.repo.creates != 1 || h.repo.reuses != 1 {
		t.Errorf("creates = %d, reuses = %d, want 1 and 1", h.repo.creates, h.repo.reuses)
	}
	if len(h.media.tokenRequests) != 2 {
		t.Errorf("SignToken calls = %d, want one per join (each browser needs its own token)", len(h.media.tokenRequests))
	}
}

// TestJoinResetsAReusedSession covers the state a reconnecting student must land in:
// CONNECTING with left_at cleared, so a session that was DISCONNECTED or LEFT is not
// silently reported as finished.
func TestJoinResetsAReusedSession(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusDisconnected, "学生 A")
	disconnectedAt := h.repo.tick()
	session.ScreenStartedAt = &disconnectedAt

	result := h.join(t)
	if result.Session.ID != session.ID {
		t.Fatalf("session = %s, want the existing %s", result.Session.ID, session.ID)
	}
	if result.Session.Status != StatusConnecting {
		t.Errorf("status = %s, want CONNECTING", result.Session.Status)
	}
	if result.Session.LeftAt != nil {
		t.Errorf("leftAt = %v, want nil after a rejoin", result.Session.LeftAt)
	}
	// The first-time stamps survive a reconnect: they answer "when did this student's
	// screen first come up?", which a refresh does not change.
	if result.Session.ScreenStartedAt == nil {
		t.Error("screenStartedAt was erased by the rejoin")
	}
}

// TestJoinCaptureIsDiagnosticsOnly is §43/§19 in one test: the diagnostics are logged,
// and they are nowhere in what the control plane records.
func TestJoinCaptureIsDiagnosticsOnly(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Join(context.Background(), JoinInput{
		StudentID:   h.studentID,
		ClassroomID: h.classroomID,
		Capture:     &Capture{DisplaySurface: "monitor", Width: 1920, Height: 1080},
	})
	if err != nil {
		t.Fatalf("Join(): %v", err)
	}
	logged := h.logs.String()
	for _, want := range []string{"display_surface=monitor", "width=1920", "height=1080"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log is missing %q:\n%s", want, logged)
		}
	}
	// The session the service created carries no trace of the capture block: the
	// repository interface has no field for it, which is the structural version of
	// "diagnostics must never become state".
	for _, session := range h.repo.sessions {
		if strings.Contains(session.LiveKitIdentity, "monitor") {
			t.Errorf("the media identity leaked the capture diagnostics: %q", session.LiveKitIdentity)
		}
	}
}

// TestJoinSurvivesWithoutAMediaPlane: a degraded process must answer MEDIA_TOKEN_FAILED
// rather than mint a token it cannot honour.
func TestJoinSurvivesWithoutAMediaPlane(t *testing.T) {
	h := newHarness(t)
	h.service = NewService(h.repo, h.directory, nil, h.cfg)

	_, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: h.classroomID})
	if !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("err = %v, want ErrMediaUnavailable", err)
	}
	// The row still exists: the student tried to enter, and a retry reuses it.
	if h.repo.creates != 1 {
		t.Errorf("creates = %d, want 1 (the attempt is recorded)", h.repo.creates)
	}
}

// TestJoinReportsMediaFailureWhenTheRoomCannotBeCreated: EnsureRoom failing is a media
// plane failure (502), not a 400 or a 409.
func TestJoinReportsMediaFailureWhenTheRoomCannotBeCreated(t *testing.T) {
	h := newHarness(t)
	h.media.ensureErr = errors.New("livekit: unreachable")
	if _, err := h.service.Join(context.Background(), JoinInput{StudentID: h.studentID, ClassroomID: h.classroomID}); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("err = %v, want ErrMediaUnavailable", err)
	}
	if len(h.media.tokenRequests) != 0 {
		t.Errorf("a token was minted for a room that could not be created")
	}
}

// TestJoinNeverLogsTheToken is the §59 assertion. The token is a credential: it may
// exist in the response and nowhere else.
func TestJoinNeverLogsTheToken(t *testing.T) {
	h := newHarness(t)
	result := h.join(t)
	if strings.Contains(h.logs.String(), result.Token) {
		t.Fatalf("the media token was written to the log:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// Leave (§43/§50)
// ---------------------------------------------------------------------------

// TestLeaveMarksTheSessionAndRemovesTheParticipant covers the happy path: LEFT with
// left_at set, and the participant disconnected from the room.
func TestLeaveMarksTheSessionAndRemovesTheParticipant(t *testing.T) {
	h := newHarness(t)
	joined := h.join(t)

	left, err := h.service.Leave(context.Background(), joined.Session.ID, h.studentID)
	if err != nil {
		t.Fatalf("Leave(): %v", err)
	}
	if left.Status != StatusLeft {
		t.Errorf("status = %s, want LEFT", left.Status)
	}
	if left.LeftAt == nil {
		t.Error("leftAt is nil, want the time the student left")
	}
	if len(h.media.removed) != 1 {
		t.Fatalf("RemoveParticipant calls = %v, want 1", h.media.removed)
	}
	if h.media.removed[0][1] != joined.Session.LiveKitIdentity {
		t.Errorf("removed identity = %q, want %q", h.media.removed[0][1], joined.Session.LiveKitIdentity)
	}
	if h.media.removed[0][0] != h.run().LiveKitRoomName {
		t.Errorf("removed from room %q, want %q", h.media.removed[0][0], h.run().LiveKitRoomName)
	}
}

// TestLeaveIsIdempotent: a second leave is a success and keeps the first timestamp.
func TestLeaveIsIdempotent(t *testing.T) {
	h := newHarness(t)
	joined := h.join(t)

	first, err := h.service.Leave(context.Background(), joined.Session.ID, h.studentID)
	if err != nil {
		t.Fatalf("first Leave(): %v", err)
	}
	second, err := h.service.Leave(context.Background(), joined.Session.ID, h.studentID)
	if err != nil {
		t.Fatalf("second Leave(): %v", err)
	}
	if second.Status != StatusLeft {
		t.Errorf("status = %s, want LEFT", second.Status)
	}
	if !first.LeftAt.Equal(*second.LeftAt) {
		t.Errorf("leftAt moved on a repeated leave: %s → %s", first.LeftAt, second.LeftAt)
	}
}

// TestLeaveRejectsAnotherStudentsSession is the §58 rule: "not yours" and "not there"
// are one answer, so session ids cannot be probed.
func TestLeaveRejectsAnotherStudentsSession(t *testing.T) {
	h := newHarness(t)
	joined := h.join(t)

	if _, err := h.service.Leave(context.Background(), joined.Session.ID, h.otherID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.service.Leave(context.Background(), uuid.New(), h.studentID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown session: err = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.service.Leave(context.Background(), uuid.Nil, h.studentID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("nil session: err = %v, want ErrSessionNotFound", err)
	}
	if len(h.media.removed) != 0 {
		t.Errorf("a participant was removed for a session that was never left: %v", h.media.removed)
	}
}

// TestLeaveSucceedsWhenTheMediaPlaneFails is §33: the control plane already decided,
// and a media-plane failure must not be reported as a failed leave.
func TestLeaveSucceedsWhenTheMediaPlaneFails(t *testing.T) {
	t.Run("remove fails", func(t *testing.T) {
		h := newHarness(t)
		joined := h.join(t)
		h.media.removeErr = errors.New("livekit: not found")

		left, err := h.service.Leave(context.Background(), joined.Session.ID, h.studentID)
		if err != nil {
			t.Fatalf("Leave(): %v", err)
		}
		if left.Status != StatusLeft {
			t.Errorf("status = %s, want LEFT", left.Status)
		}
		if !strings.Contains(h.logs.String(), "media participant not removed") {
			t.Errorf("the media failure was not logged:\n%s", h.logs.String())
		}
	})

	t.Run("run lookup fails", func(t *testing.T) {
		h := newHarness(t)
		joined := h.join(t)
		h.directory.runErr = errors.New("postgres: down")

		left, err := h.service.Leave(context.Background(), joined.Session.ID, h.studentID)
		if err != nil {
			t.Fatalf("Leave(): %v", err)
		}
		if left.Status != StatusLeft {
			t.Errorf("status = %s, want LEFT", left.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// Teacher media token (§27/§44)
// ---------------------------------------------------------------------------

// TestTeacherTokenGrantsMicrophoneOnly pins §27: a teacher token may subscribe to the
// students and publish a microphone, and nothing else.
func TestTeacherTokenGrantsMicrophoneOnly(t *testing.T) {
	h := newHarness(t)
	loginSessionID := uuid.New()

	result, err := h.service.TeacherToken(context.Background(), TeacherTokenInput{
		ClassroomID: h.classroomID,
		TeacherID:   h.teacherID,
		SessionID:   loginSessionID,
	})
	if err != nil {
		t.Fatalf("TeacherToken(): %v", err)
	}
	if result.LiveKitURL != h.cfg.LiveKitURL || result.Token != h.media.token {
		t.Errorf("result = %+v, want the configured url and the signed token", result)
	}
	if len(h.media.tokenRequests) != 1 {
		t.Fatalf("SignToken calls = %d, want 1", len(h.media.tokenRequests))
	}
	req := h.media.tokenRequests[0]
	// §44: the identity is the teacher's LOGIN session, not the account and not the
	// user id — a stable per-lesson handle that reveals nothing.
	if req.Identity != loginSessionID.String() {
		t.Errorf("identity = %q, want the login session id %s", req.Identity, loginSessionID)
	}
	if req.RoomName != h.run().LiveKitRoomName {
		t.Errorf("room = %q, want %q", req.RoomName, h.run().LiveKitRoomName)
	}
	if !req.CanSubscribe {
		t.Error("canSubscribe = false, want true (§27: the teacher watches the students)")
	}
	if !req.CanPublish {
		t.Error("canPublish = false, want true (§27: private audio is Phase 10)")
	}
	if req.CanPublishData {
		t.Error("canPublishData = true, want false")
	}
	if len(req.PublishSources) != 1 || req.PublishSources[0] != media.PublishMicrophone {
		t.Errorf("publish sources = %v, want [MICROPHONE] only (§27)", req.PublishSources)
	}
	// The room must exist before the teacher connects.
	if len(h.media.ensureCalls) != 1 {
		t.Errorf("EnsureRoom calls = %v, want 1", h.media.ensureCalls)
	}
}

// TestTeacherTokenRejectsNonOwner keeps ownership a service rule and not a route
// prefix (§37).
func TestTeacherTokenRejectsNonOwner(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.TeacherToken(context.Background(), TeacherTokenInput{
		ClassroomID: h.classroomID,
		TeacherID:   h.otherID,
		SessionID:   uuid.New(),
	})
	if !errors.Is(err, classroom.ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	if len(h.media.tokenRequests) != 0 {
		t.Errorf("a token was minted for a non-owner")
	}
}

// TestTeacherTokenRejectsClosedClassroom: no media session exists outside an open
// lesson, whatever the console still shows.
func TestTeacherTokenRejectsClosedClassroom(t *testing.T) {
	h := newHarness(t)
	closedID := uuid.New()
	h.directory.seedClosed(closedID, h.teacherID)

	_, err := h.service.TeacherToken(context.Background(), TeacherTokenInput{
		ClassroomID: closedID,
		TeacherID:   h.teacherID,
		SessionID:   uuid.New(),
	})
	if !errors.Is(err, ErrClassroomClosed) {
		t.Fatalf("err = %v, want ErrClassroomClosed", err)
	}
}

// TestTeacherTokenNeverLogsTheToken is §59 again, on the teacher path.
func TestTeacherTokenNeverLogsTheToken(t *testing.T) {
	h := newHarness(t)
	if _, err := h.service.TeacherToken(context.Background(), TeacherTokenInput{
		ClassroomID: h.classroomID, TeacherID: h.teacherID, SessionID: uuid.New(),
	}); err != nil {
		t.Fatalf("TeacherToken(): %v", err)
	}
	if strings.Contains(h.logs.String(), h.media.token) {
		t.Fatalf("the teacher media token was written to the log:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// Monitor (§51)
// ---------------------------------------------------------------------------

// TestMonitorTransitions is the state table of §51, one case per row.
func TestMonitorTransitions(t *testing.T) {
	cases := []struct {
		name    string
		current Status
		present bool
		screen  bool
		want    Status
		// advanced is whether the observation must be persisted at all.
		advanced bool
	}{
		{name: "screen up from CONNECTING", current: StatusConnecting, present: true, screen: true, want: StatusOnline, advanced: true},
		{name: "screen up after a reconnect", current: StatusDisconnected, present: true, screen: true, want: StatusOnline, advanced: true},
		{name: "screen restored after SCREEN_LOST", current: StatusScreenLost, present: true, screen: true, want: StatusOnline, advanced: true},
		{name: "already ONLINE with a screen", current: StatusOnline, present: true, screen: true, want: StatusOnline},
		{name: "screen disappeared", current: StatusOnline, present: true, screen: false, want: StatusScreenLost, advanced: true},
		{name: "connected but never shared", current: StatusConnecting, present: true, screen: false, want: StatusConnecting},
		{name: "reconnected, screen not up yet", current: StatusDisconnected, present: true, screen: false, want: StatusConnecting, advanced: true},
		{name: "gone while CONNECTING", current: StatusConnecting, present: false, want: StatusDisconnected, advanced: true},
		{name: "gone while ONLINE", current: StatusOnline, present: false, want: StatusDisconnected, advanced: true},
		{name: "gone while SCREEN_LOST", current: StatusScreenLost, present: false, want: StatusDisconnected, advanced: true},
		{name: "still gone", current: StatusDisconnected, present: false, want: StatusDisconnected},
		{name: "LEFT is terminal", current: StatusLeft, present: true, screen: true, want: StatusLeft},
		{name: "ROOM_CLOSED is terminal", current: StatusRoomClosed, present: false, want: StatusRoomClosed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			session := h.repo.seed(h.runID, h.studentID, tc.current, "学生 A")
			if tc.present {
				h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: tc.screen}
			}

			view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
			if err != nil {
				t.Fatalf("Monitor(): %v", err)
			}
			if len(view.Students) != 1 {
				t.Fatalf("students = %d, want 1", len(view.Students))
			}
			if got := statusOf(t, view.Students[0]); got != tc.want {
				t.Errorf("status = %s, want %s", got, tc.want)
			}
			if view.Students[0].SessionID == nil {
				t.Error("sessionId = null, want the session the transition belongs to")
			}
			// A tile that just transitioned must still render the person: the display
			// name comes from the monitoring JOIN, not from the row the write returned.
			if view.Students[0].DisplayName != "学生 A" {
				t.Errorf("displayName = %q, want the joined account name", view.Students[0].DisplayName)
			}
			if advanced := len(h.repo.observations) > 0; advanced != tc.advanced {
				t.Errorf("persisted = %v, want %v (observations: %+v)", advanced, tc.advanced, h.repo.observations)
			}
			if !view.MediaObserved {
				t.Error("MediaObserved = false, want true")
			}
		})
	}
}

// TestMonitorWritesTheFirstTimestamps checks what the transition persists: the first
// connect and the first screen, and the most recent loss.
func TestMonitorWritesTheFirstTimestamps(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	change := h.repo.observations[0]
	if !change.MarkConnected || !change.MarkScreenStarted || change.MarkScreenLost {
		t.Fatalf("change = %+v, want connected+screenStarted and no lost", change)
	}
	stored := h.repo.sessions[session.ID]
	if stored.ConnectedAt == nil || stored.ScreenStartedAt == nil {
		t.Fatalf("timestamps not stored: %+v", stored)
	}

	// The screen goes away: screen_lost_at is written.
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{}
	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("second Monitor(): %v", err)
	}
	last := h.repo.observations[len(h.repo.observations)-1]
	if last.From != StatusOnline || last.To != StatusScreenLost || !last.MarkScreenLost {
		t.Fatalf("loss change = %+v, want ONLINE → SCREEN_LOST with MarkScreenLost", last)
	}
	if h.repo.sessions[session.ID].ScreenLostAt == nil {
		t.Error("screen_lost_at was not stored")
	}
}

// TestMonitorReportsTheObservedMedia is the §51 DTO assertion: screen/camera/mic come
// from the media plane, and Phase 6 only ever sets screen (camera and microphone are
// observed so the fields cannot go stale).
func TestMonitorReportsTheObservedMedia(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusOnline, "学生 A")
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{
		ScreenShare: true, Camera: true, Microphone: true,
	}

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	student := view.Students[0]
	if !student.ScreenActive || !student.CameraActive || !student.MicrophoneActive {
		t.Errorf("media flags = %+v, want all true from the observation", student)
	}
	if student.Connection != ConnectionGood {
		t.Errorf("connection = %s, want GOOD", student.Connection)
	}
	if student.DisplayName != "学生 A" {
		t.Errorf("displayName = %q, want the joined account name", student.DisplayName)
	}
	if sessionIDOf(t, student) != session.ID || student.StudentID != session.StudentID {
		t.Errorf("identifiers = %s/%s, want %s/%s", student.SessionID, student.StudentID, session.ID, session.StudentID)
	}
	if student.LastEventAt == nil {
		t.Error("lastEventAt is nil, want the session's updated_at")
	}
}

// TestMonitorWithoutAParticipantIsNotOnline is the "no fake participant" acceptance
// rule: a session whose participant was never seen is CONNECTING or DISCONNECTED, and
// never ONLINE.
func TestMonitorWithoutAParticipantIsNotOnline(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if statusOf(t, view.Students[0]) == StatusOnline {
		t.Fatalf("status = ONLINE with no participant in the room")
	}
	if view.Students[0].ScreenActive {
		t.Error("screen.active = true with no participant in the room")
	}
	if view.Students[0].Connection != ConnectionUnknown {
		t.Errorf("connection = %s, want UNKNOWN without an observation", view.Students[0].Connection)
	}
	if h.repo.sessions[session.ID].Status != StatusDisconnected {
		t.Errorf("stored status = %s, want DISCONNECTED", h.repo.sessions[session.ID].Status)
	}
}

// TestMonitorMediaFailureAdvancesNothing is the §33 rule this phase is built around:
// a media-plane outage must not be written down as a business fact.
func TestMonitorMediaFailureAdvancesNothing(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	online := h.repo.seed(h.runID, h.studentID, StatusOnline, "学生 A")
	connecting := h.repo.seed(h.runID, h.otherID, StatusConnecting, "学生 B")
	h.media.observeErr = errors.New("livekit: list participants: context deadline exceeded")

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor() must not fail when the media plane is down: %v", err)
	}
	if view.MediaObserved {
		t.Error("MediaObserved = true, want false")
	}
	if len(h.repo.observations) != 0 {
		t.Fatalf("observations were persisted during an outage: %+v", h.repo.observations)
	}
	if h.repo.sessions[online.ID].Status != StatusOnline || h.repo.sessions[connecting.ID].Status != StatusConnecting {
		t.Error("a session status changed while the media plane was unreachable")
	}
	for _, student := range view.Students {
		if student.Connection != ConnectionUnknown {
			t.Errorf("connection = %s, want UNKNOWN for every student", student.Connection)
		}
	}
	// The stale picture is derived from the stored status (the §21 invariant), so a
	// student who was ONLINE does not blink out of the wall during an outage.
	for _, student := range view.Students {
		if student.StudentID == online.StudentID && !student.ScreenActive {
			t.Error("a stored-ONLINE session reported screen.active = false during an outage")
		}
		if student.StudentID == connecting.StudentID && student.ScreenActive {
			t.Error("a stored-CONNECTING session reported screen.active = true")
		}
	}
	if !strings.Contains(h.logs.String(), "session states are not advanced") {
		t.Errorf("the outage was not logged:\n%s", h.logs.String())
	}
}

// TestMonitorTerminalSessionsAreNotAdvanced pins §22/§50: LEFT and ROOM_CLOSED are
// final, and a lingering participant must not revive them.
func TestMonitorTerminalSessionsAreNotAdvanced(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	left := h.repo.seed(h.runID, h.studentID, StatusLeft, "学生 A")
	closed := h.repo.seed(h.runID, h.otherID, StatusRoomClosed, "学生 B")
	h.media.observed[left.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}
	h.media.observed[closed.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if len(h.repo.observations) != 0 {
		t.Fatalf("a terminal session was advanced: %+v", h.repo.observations)
	}
	for _, student := range view.Students {
		if student.Connection != ConnectionUnknown {
			t.Errorf("terminal tile %+v reports connection %s, want UNKNOWN", student.Status, student.Connection)
		}
		if student.ScreenActive {
			t.Errorf("terminal tile %+v reports an active screen", student.Status)
		}
	}
}

// TestMonitorKeepsTheStoredRowWhenTheObservationIsStale: the compare-and-set in the
// repository refuses to overwrite a newer decision, and the wall reports what is
// actually stored.
func TestMonitorKeepsTheStoredRowWhenTheObservationIsStale(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.repo.outdated = true
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if got := statusOf(t, view.Students[0]); got != StatusConnecting {
		t.Errorf("status = %s, want the stored CONNECTING (the observation was stale)", got)
	}
	if !strings.Contains(h.logs.String(), "skipped stale session states") {
		t.Errorf("the stale observation was not logged:\n%s", h.logs.String())
	}
}

// TestMonitorFailsWhenTheObservationCannotBeStored: unlike a media outage, a database
// failure IS an error — the wall must not show a transition that was not recorded.
func TestMonitorFailsWhenTheObservationCannotBeStored(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.repo.observeErr = errors.New("postgres: connection refused")
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err == nil {
		t.Fatal("Monitor() = nil error, want the persistence failure")
	}
}

// TestMonitorRequiresAnOpenOwnedClassroom covers the two 4xx paths of §51.
func TestMonitorRequiresAnOpenOwnedClassroom(t *testing.T) {
	t.Run("not open", func(t *testing.T) {
		h := newHarness(t)
		closedID := uuid.New()
		h.directory.seedClosed(closedID, h.teacherID)
		if _, err := h.service.Monitor(context.Background(), closedID, h.teacherID); !errors.Is(err, ErrClassroomClosed) {
			t.Fatalf("err = %v, want ErrClassroomClosed", err)
		}
	})

	t.Run("not the owner", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.service.Monitor(context.Background(), h.classroomID, h.otherID); !errors.Is(err, classroom.ErrNotOwner) {
			t.Fatalf("err = %v, want ErrNotOwner", err)
		}
	})

	t.Run("unknown classroom", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.service.Monitor(context.Background(), uuid.New(), h.teacherID); !errors.Is(err, classroom.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestMonitorDoesNotReachTheMediaPlaneForOtherTeachersClassrooms: the ownership check
// runs before anything is observed, so one teacher cannot learn whether another
// teacher's room is alive.
func TestMonitorDoesNotReachTheMediaPlaneForOtherTeachersClassrooms(t *testing.T) {
	h := newHarness(t)
	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.otherID); err == nil {
		t.Fatal("Monitor() as a non-owner succeeded")
	}
	if len(h.media.observeCalls) != 0 {
		t.Errorf("the media plane was queried for a non-owner: %v", h.media.observeCalls)
	}
}

// ---------------------------------------------------------------------------
// Monitor: the whole roster (§29/§73)
// ---------------------------------------------------------------------------

// TestMonitorListsAuthorizedStudentsWhoNeverEntered is the Phase 7 contract change: the
// wall is the ROSTER, so a student who was authorized and never pressed "进入课堂" is a
// tile — with a null session rather than an invented state — and §29's "18 / 25" has a
// denominator at all.
func TestMonitorListsAuthorizedStudentsWhoNeverEntered(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	entered := h.repo.seed(h.runID, h.studentID, StatusOnline, "学生 A")
	h.media.observed[entered.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if len(view.Students) != 2 {
		t.Fatalf("students = %d, want the whole roster (2): %+v", len(view.Students), view.Students)
	}

	// The tile that HAS a session still behaves exactly like Phase 6.
	joined := tileOf(t, view, h.studentID)
	if sessionIDOf(t, joined) != entered.ID || statusOf(t, joined) != StatusOnline {
		t.Errorf("joined tile = %+v, want the ONLINE session", joined)
	}
	if !joined.ScreenActive || joined.Connection != ConnectionGood {
		t.Errorf("joined tile media = %+v, want an observed screen", joined)
	}

	// The tile that never entered is the null shape of §29, and it is NOT a seventh
	// status: sessionStatus is null, not "NOT_JOINED".
	waiting := tileOf(t, view, h.otherID)
	if waiting.DisplayName != "学生 B" {
		t.Errorf("displayName = %q, want the roster's name", waiting.DisplayName)
	}
	if waiting.SessionID != nil || waiting.Status != nil {
		t.Errorf("session = %v/%v, want null for a student with no session", waiting.SessionID, waiting.Status)
	}
	if waiting.ScreenActive || waiting.CameraActive || waiting.MicrophoneActive {
		t.Errorf("media = %+v, want all inactive without a session", waiting)
	}
	if waiting.Connection != ConnectionUnknown {
		t.Errorf("connection = %s, want UNKNOWN without a session", waiting.Connection)
	}
	if waiting.JoinedAt != nil || waiting.LastEventAt != nil {
		t.Errorf("timestamps = %v/%v, want null without a session", waiting.JoinedAt, waiting.LastEventAt)
	}
}

// TestMonitorOrdersTilesByAccount: §29 is a grid a teacher scans while teaching, so the
// order must be the roster's and must not depend on who joined first, on map iteration,
// or on the display name.
func TestMonitorOrdersTilesByAccount(t *testing.T) {
	h := newHarness(t)
	second, third := uuid.New(), uuid.New()
	// Accounts are deliberately added out of order, and the display names would sort
	// differently from the accounts.
	h.addStudent(third, "S10003", "阿一")
	h.addStudent(h.otherID, "S10001", "最後")
	h.addStudent(second, "S10002", "中间")
	// Sessions are created in yet another order.
	h.repo.seed(h.runID, third, StatusConnecting, "阿一")
	h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	want := []uuid.UUID{h.otherID, second, third, h.studentID} // S10001, S10002, S10003, S10086
	if len(view.Students) != len(want) {
		t.Fatalf("students = %d, want %d", len(view.Students), len(want))
	}
	for i, studentID := range want {
		if view.Students[i].StudentID != studentID {
			t.Errorf("tile %d = %s, want %s (sorted by account)", i, view.Students[i].StudentID, studentID)
		}
	}
}

// TestMonitorDoesNotAdvanceAStudentWithoutASession: the state machine only ever runs on
// a session row. A student who never entered must not acquire CONNECTING (or any other
// state) because a poll happened to see somebody else in the room.
func TestMonitorDoesNotAdvanceAStudentWithoutASession(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	entered := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.media.observed[entered.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}
	// Somebody else is in the room and observable; the roster entry without a session has
	// no identity to match, so nothing may be folded into it.
	h.media.observed[uuid.New().String()] = media.ParticipantTracks{ScreenShare: true}

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	waiting := tileOf(t, view, h.otherID)
	if waiting.SessionID != nil || waiting.Status != nil {
		t.Errorf("a student with no session was given one: %+v", waiting)
	}
	for _, change := range h.repo.observations {
		if change.SessionID == uuid.Nil {
			t.Errorf("an observation was persisted for a session that does not exist: %+v", change)
		}
	}
}

// TestMonitorKeepsNullSessionsDuringAMediaOutage: an unobserved room changes nothing for
// a student who has no session — there is no stale state to report, so the tile keeps its
// nulls instead of inventing a DISCONNECTED.
func TestMonitorKeepsNullSessionsDuringAMediaOutage(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	h.media.observeErr = errors.New("livekit: list participants: context deadline exceeded")

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor() must not fail when the media plane is down: %v", err)
	}
	if view.MediaObserved {
		t.Error("MediaObserved = true, want false")
	}
	waiting := tileOf(t, view, h.otherID)
	if waiting.SessionID != nil || waiting.Status != nil {
		t.Errorf("tile = %+v, want nulls during an outage too", waiting)
	}
	if waiting.Connection != ConnectionUnknown {
		t.Errorf("connection = %s, want UNKNOWN", waiting.Connection)
	}
}

// ---------------------------------------------------------------------------
// Monitor: §26 media-plane isolation
// ---------------------------------------------------------------------------

// TestMonitorEnforcesStudentIsolation is the Phase 7 server-side rule: the observation
// that advances the states is also handed to the media plane to revoke whatever a
// student might still be subscribed to, with the teacher as the explicit whitelist.
func TestMonitorEnforcesStudentIsolation(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	first := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.repo.seed(h.runID, h.otherID, StatusConnecting, "学生 B")

	teacherIdentity := uuid.New().String()
	h.media.observed[first.LiveKitIdentity] = media.ParticipantTracks{
		Tracks: []media.ObservedTrack{{Sid: "TR_A", Source: media.PublishScreenShare}},
	}
	h.media.observed[teacherIdentity] = media.ParticipantTracks{
		Tracks: []media.ObservedTrack{{Sid: "TR_T", Source: media.PublishMicrophone}},
	}
	// The media plane reports what it actually revoked, which the service turns into the
	// Warn lines operations greps for.
	h.media.revoked = []media.PeerSubscriptionRevocation{{
		ObserverIdentity: first.LiveKitIdentity, TrackOwnerIdentity: teacherIdentity, TrackSid: "TR_T",
	}}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}

	if len(h.media.enforcementCalls) != 1 {
		t.Fatalf("enforcement calls = %d, want exactly one per observation", len(h.media.enforcementCalls))
	}
	call := h.media.enforcementCalls[0]
	if call.room != h.run().LiveKitRoomName {
		t.Errorf("room = %q, want the run's room", call.room)
	}
	if len(call.students) != 2 || call.students[0] != first.LiveKitIdentity {
		t.Errorf("students = %v, want the run's student identities", call.students)
	}
	if len(call.allowed) != 1 || call.allowed[0] != teacherIdentity {
		t.Errorf("allowed = %v, want only the non-student identity", call.allowed)
	}
	if len(call.observed) != 2 {
		t.Errorf("observed = %v, want the SAME observation the states were advanced from", call.observed)
	}

	logs := h.logs.String()
	for _, want := range []string{
		"action=peer_subscription_revoked",
		"room=" + h.run().LiveKitRoomName,
		"observer_identity=" + first.LiveKitIdentity,
		"subscribed_track_owner=" + teacherIdentity,
		"track_sid=TR_T",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the revocation log line is missing %q:\n%s", want, logs)
		}
	}
}

// TestMonitorEnforcementDoesNotTouchTerminalOrAbsentStudents pins what the reconciliation
// asks LiveKit for: a session that is over has already been disconnected, and a student
// who never entered has no identity at all.
func TestMonitorEnforcementDoesNotTouchTerminalOrAbsentStudents(t *testing.T) {
	h := newHarness(t)
	h.addStudent(h.otherID, "S10087", "学生 B")
	left := h.repo.seed(h.runID, h.studentID, StatusLeft, "学生 A")
	teacherIdentity := uuid.New().String()
	// The LEFT student's participant is still lingering in the room (the removal is best
	// effort, §50), and the teacher is connected.
	h.media.observed[left.LiveKitIdentity] = media.ParticipantTracks{
		Tracks: []media.ObservedTrack{{Sid: "TR_A", Source: media.PublishScreenShare}},
	}
	h.media.observed[teacherIdentity] = media.ParticipantTracks{}

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if len(h.media.enforcementCalls) != 1 {
		t.Fatalf("enforcement calls = %d, want one", len(h.media.enforcementCalls))
	}
	call := h.media.enforcementCalls[0]
	if len(call.students) != 0 {
		t.Errorf("students = %v, want none (a LEFT session is not reconciled and a student without a session has no identity)", call.students)
	}
	// The LEFT student still counts as a STUDENT for the whitelist: their lingering
	// participant must not be mistaken for the teacher, or a classmate would keep
	// receiving whatever they left behind.
	if len(call.allowed) != 1 || call.allowed[0] != teacherIdentity {
		t.Errorf("allowed = %v, want only the teacher's identity", call.allowed)
	}
}

// TestMonitorSkipsEnforcementWhenTheRoomCannotBeObserved: §26 enforcement acts on an
// observation, and an outage produces none. Asking LiveKit to revoke "something" without
// an observation would be guessing.
func TestMonitorSkipsEnforcementWhenTheRoomCannotBeObserved(t *testing.T) {
	h := newHarness(t)
	h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.media.observeErr = errors.New("livekit: list participants: context deadline exceeded")

	if _, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID); err != nil {
		t.Fatalf("Monitor(): %v", err)
	}
	if len(h.media.enforcementCalls) != 0 {
		t.Errorf("enforcement ran without an observation: %+v", h.media.enforcementCalls)
	}
}

// TestMonitorEnforcementFailureIsOnlyAWarn is the §33 discipline applied to §26: the
// teacher's wall must be served even when the media plane refuses to revoke a
// subscription.
func TestMonitorEnforcementFailureIsOnlyAWarn(t *testing.T) {
	h := newHarness(t)
	session := h.repo.seed(h.runID, h.studentID, StatusConnecting, "学生 A")
	h.media.observed[session.LiveKitIdentity] = media.ParticipantTracks{ScreenShare: true}
	h.media.enforcementErr = errors.New("livekit: revoke peer subscriptions for x: unavailable")

	view, err := h.service.Monitor(context.Background(), h.classroomID, h.teacherID)
	if err != nil {
		t.Fatalf("Monitor() must survive a failed revocation: %v", err)
	}
	if !view.MediaObserved || len(view.Students) != 1 {
		t.Fatalf("view = %+v, want a normal wall", view)
	}
	if got := statusOf(t, view.Students[0]); got != StatusOnline {
		t.Errorf("status = %s, want ONLINE: the state machine is independent of the revocation", got)
	}
	if !strings.Contains(h.logs.String(), "action=peer_subscription_revocation_failed") {
		t.Errorf("the failed revocation was not logged:\n%s", h.logs.String())
	}
}
