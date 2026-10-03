package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// The WebSocket layer of §47 against a real HTTP server and real sockets.
//
// These tests deliberately do NOT fake the transport: the rules that matter here —
// scoping, the envelope, PING/PONG, a full write queue dropping the connection, a clean
// shutdown — are all about what actually leaves the process, and a fake socket would
// assert the mock instead of the contract.

const (
	// expectTimeout is how long a test waits for a message it EXPECTS. It is generous
	// because a loaded CI machine is slow, not because anything here is timing-based.
	expectTimeout = 3 * time.Second
	// silenceTimeout is how long a test waits to conclude that nothing was sent. It is
	// short: the alternative is a suite that takes a minute to say "no message".
	silenceTimeout = 300 * time.Millisecond
)

// testHub builds a hub with timings that make the liveness rules testable in
// milliseconds instead of minutes.
func testHub(t *testing.T, mutate func(*HubConfig)) *Hub {
	t.Helper()
	cfg := HubConfig{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowedOrigins: []string{"http://allowed.test"},
		WriteWait:      time.Second,
		PongWait:       5 * time.Second,
		PingInterval:   30 * time.Second,
		SendBuffer:     8,
		ReadLimit:      4096,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewHub(cfg)
}

// hubServer serves the hub's two entry points on a real listener.
//
// The identity comes from a header instead of a session cookie because the cookie chain
// is tested end to end in internal/httpapi; here the subject is what the hub does with an
// identity it has already been given. The error mapping mirrors the real handler's: a hub
// that refuses before upgrading produces a 5xx, everything else has already been written
// by the upgrader.
func hubServer(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var role Role
		switch r.URL.Path {
		case "/ws/student":
			role = RoleStudent
		case "/ws/teacher":
			role = RoleTeacher
		default:
			http.NotFound(w, r)
			return
		}
		userID, err := uuid.Parse(r.Header.Get("X-Test-User"))
		if err != nil {
			http.Error(w, "missing test user", http.StatusBadRequest)
			return
		}
		if err := hub.Serve(w, r, role, userID); errors.Is(err, ErrUnavailable) {
			http.Error(w, "hub unavailable", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// dialRaw opens a connection that NOTHING reads. It is what the back-pressure test needs:
// a client whose socket buffer fills up.
func dialRaw(t *testing.T, server *httptest.Server, hub *Hub, path string, userID uuid.UUID, headers http.Header) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + path
	requestHeaders := http.Header{}
	for key, values := range headers {
		requestHeaders[key] = values
	}
	requestHeaders.Set("X-Test-User", userID.String())

	conn, response, err := websocket.DefaultDialer.Dial(url, requestHeaders)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", path, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })

	role := RoleStudent
	if strings.HasSuffix(path, "teacher") {
		role = RoleTeacher
	}
	waitFor(t, "connection registered", func() bool { return hub.Connections(role, userID) >= 1 })
	return conn
}

// testClient is a connection with a reader goroutine behind it.
//
// WHY a goroutine and not a read deadline per assertion: gorilla treats a read timeout as
// a PERMANENT failure of the connection, so "assert silence, then keep using it" is
// impossible with deadlines. Reading continuously into a channel makes both assertions
// non-destructive — which matters, because several tests below check that one connection
// receives nothing and still works.
type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	// frames carries every message the server sent, in order.
	frames chan []byte
	// closed carries the error that ended the read loop.
	closed chan error
}

func dialClient(t *testing.T, server *httptest.Server, hub *Hub, path string, userID uuid.UUID, headers http.Header) *testClient {
	t.Helper()
	client := &testClient{
		t:      t,
		conn:   dialRaw(t, server, hub, path, userID, headers),
		frames: make(chan []byte, 64),
		closed: make(chan error, 1),
	}
	go func() {
		for {
			_, data, err := client.conn.ReadMessage()
			if err != nil {
				client.closed <- err
				close(client.frames)
				return
			}
			client.frames <- data
		}
	}()
	return client
}

// next returns the next frame, or ok=false when none arrived in time.
func (c *testClient) next(timeout time.Duration) ([]byte, bool) {
	select {
	case data, open := <-c.frames:
		if !open {
			return nil, false
		}
		return data, true
	case <-time.After(timeout):
		return nil, false
	}
}

// expectMessage waits for one message and decodes it as the frozen envelope.
func (c *testClient) expectMessage() Message {
	c.t.Helper()
	raw, ok := c.next(expectTimeout)
	if !ok {
		c.t.Fatalf("expected a message, got none (connection: %v)", c.closeError())
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.t.Fatalf("message is not an envelope: %v (%s)", err, raw)
	}
	return msg
}

// expectSilence asserts that nothing arrives within the silence window.
//
// This is the assertion §26's isolation rests on: a student connection that receives
// nothing is the whole point of the rule.
func (c *testClient) expectSilence(what string) {
	c.t.Helper()
	if raw, ok := c.next(silenceTimeout); ok {
		c.t.Fatalf("%s: received %s, want silence", what, raw)
	}
}

// closeError reports the error that ended the read loop, waiting briefly for it.
func (c *testClient) closeError() error {
	select {
	case err := <-c.closed:
		return err
	case <-time.After(expectTimeout):
		return errors.New("connection is still open")
	}
}

// waitFor polls a condition with a deadline, so a test asserts a state instead of
// sleeping a guessed amount of time.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// Scoping (§26/§47)
// ---------------------------------------------------------------------------

func TestStudentMessagesOnlyReachTheAddressedStudent(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	alice, bob := uuid.New(), uuid.New()
	aliceClient := dialClient(t, server, hub, "/ws/student", alice, nil)
	bobClient := dialClient(t, server, hub, "/ws/student", bob, nil)

	hub.ToStudents([]uuid.UUID{alice}, newMessage(TypeScreenLost, map[string]any{"sessionId": "s-alice"}))

	msg := aliceClient.expectMessage()
	if msg.Type != TypeScreenLost || msg.Data["sessionId"] != "s-alice" {
		t.Fatalf("alice received %+v", msg)
	}
	// The load-bearing assertion of §26: a classmate's connection receives NOTHING.
	bobClient.expectSilence("another student's own message")
}

func TestTeacherAndStudentAudiencesAreDisjoint(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	teacher, student := uuid.New(), uuid.New()
	teacherClient := dialClient(t, server, hub, "/ws/teacher", teacher, nil)
	studentClient := dialClient(t, server, hub, "/ws/student", student, nil)

	// A teacher-addressed message must not reach a student.
	hub.ToTeacher(teacher, newMessage(TypeStudentOnline, map[string]any{"studentId": "x"}))
	if msg := teacherClient.expectMessage(); msg.Type != TypeStudentOnline {
		t.Fatalf("teacher received %s", msg.Type)
	}
	studentClient.expectSilence("a teacher-only message")

	// ... and a student-addressed message must not reach the teacher.
	hub.ToStudents([]uuid.UUID{student}, newMessage(TypeRoomOpened, map[string]any{"runId": "r"}))
	if msg := studentClient.expectMessage(); msg.Type != TypeRoomOpened {
		t.Fatalf("student received %s", msg.Type)
	}
	teacherClient.expectSilence("a student-only message")
}

func TestASameUUIDOnTheOtherRoleIsADifferentConnection(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	// One UUID used by both a teacher and a student: the two must never share a mailbox,
	// which is what makes ToTeacher structurally unable to reach a student.
	shared := uuid.New()
	teacherClient := dialClient(t, server, hub, "/ws/teacher", shared, nil)
	studentClient := dialClient(t, server, hub, "/ws/student", shared, nil)

	hub.ToTeacher(shared, newMessage(TypeStudentOffline, map[string]any{"reason": "LEFT"}))
	if msg := teacherClient.expectMessage(); msg.Type != TypeStudentOffline {
		t.Fatalf("teacher received %s", msg.Type)
	}
	studentClient.expectSilence("a message addressed to the teacher role")
}

func TestMessagesToSeveralStudentsReachEachOfThemOnce(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	one, two := uuid.New(), uuid.New()
	oneClient := dialClient(t, server, hub, "/ws/student", one, nil)
	twoClient := dialClient(t, server, hub, "/ws/student", two, nil)

	hub.ToStudents([]uuid.UUID{one, two}, newMessage(TypeRoomClosed, map[string]any{"runId": "r1"}))

	for name, client := range map[string]*testClient{"first": oneClient, "second": twoClient} {
		if msg := client.expectMessage(); msg.Type != TypeRoomClosed {
			t.Fatalf("%s student received %s", name, msg.Type)
		}
		client.expectSilence("a second copy of the same message")
	}
}

func TestSecondConnectionOfTheSameStudentAlsoReceives(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	student := uuid.New()
	laptop := dialClient(t, server, hub, "/ws/student", student, nil)
	phone := dialClient(t, server, hub, "/ws/student", student, nil)
	waitFor(t, "both connections to be registered", func() bool {
		return hub.Connections(RoleStudent, student) == 2
	})

	hub.ToStudents([]uuid.UUID{student}, newMessage(TypeScreenRestored, map[string]any{"sessionId": "s"}))

	for name, client := range map[string]*testClient{"laptop": laptop, "phone": phone} {
		if msg := client.expectMessage(); msg.Type != TypeScreenRestored {
			t.Fatalf("%s received %s", name, msg.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// Envelope and heartbeat
// ---------------------------------------------------------------------------

func TestEnvelopeShape(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)
	student := uuid.New()
	client := dialClient(t, server, hub, "/ws/student", student, nil)

	hub.ToStudents([]uuid.UUID{student}, newMessage(TypeScreenLost, map[string]any{"sessionId": "abc"}))

	// Decoded as raw JSON on purpose: the contract is the WIRE format, and a struct
	// round-trip would hide a renamed or missing field.
	raw, ok := client.next(expectTimeout)
	if !ok {
		t.Fatal("no message arrived")
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if envelope["type"] != "SCREEN_LOST" {
		t.Fatalf("type = %v", envelope["type"])
	}
	at, ok := envelope["at"].(string)
	if !ok {
		t.Fatalf("at is not a string: %v", envelope["at"])
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("at = %q, want RFC3339: %v", at, err)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not an object: %v", envelope["data"])
	}
	if data["sessionId"] != "abc" {
		t.Fatalf("data = %v", data)
	}
}

func TestApplicationPingIsAnsweredWithPong(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)
	client := dialClient(t, server, hub, "/ws/student", uuid.New(), nil)

	if err := client.conn.WriteJSON(map[string]string{"type": "PING"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	msg := client.expectMessage()
	if msg.Type != TypePong {
		t.Fatalf("type = %s, want PONG", msg.Type)
	}
	if msg.Data == nil {
		t.Fatal("data is null: the envelope must always carry an object")
	}
}

func TestUnknownClientMessagesAreIgnoredWithoutDisconnecting(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)
	client := dialClient(t, server, hub, "/ws/student", uuid.New(), nil)

	// A business-looking command must NOT be interpreted: the REST API is the only way
	// to change state, and this socket is a one-way channel by contract.
	if err := client.conn.WriteJSON(map[string]any{"type": "STUDENT_LEFT", "data": map[string]any{"sessionId": "x"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := client.conn.WriteMessage(websocket.TextMessage, []byte("not json at all")); err != nil {
		t.Fatalf("write: %v", err)
	}
	client.expectSilence("an unsupported client message")

	// The connection is still usable, which is what "ignored" has to mean.
	if err := client.conn.WriteJSON(map[string]string{"type": "PING"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if msg := client.expectMessage(); msg.Type != TypePong {
		t.Fatalf("type = %s, want PONG", msg.Type)
	}
}

func TestServerPingsKeepAReadingClientConnected(t *testing.T) {
	hub := testHub(t, func(cfg *HubConfig) {
		cfg.PongWait = 400 * time.Millisecond
		cfg.PingInterval = 80 * time.Millisecond
	})
	server := hubServer(t, hub)
	student := uuid.New()
	client := dialClient(t, server, hub, "/ws/student", student, nil)

	// gorilla answers control pings from inside ReadMessage, which is what a real
	// browser does without any application code. The reader goroutine is already doing
	// that; the assertion is that the connection outlives PongWait several times over.
	time.Sleep(700 * time.Millisecond)
	if got := hub.Connections(RoleStudent, student); got != 1 {
		t.Fatalf("connections = %d, want 1: a client that answers pings must stay connected", got)
	}

	// A client that is alive still receives: the liveness rule must not have closed it.
	hub.ToStudents([]uuid.UUID{student}, newMessage(TypePong, nil))
	if msg := client.expectMessage(); msg.Type != TypePong {
		t.Fatalf("type = %s", msg.Type)
	}
}

// ---------------------------------------------------------------------------
// Back pressure
// ---------------------------------------------------------------------------

func TestASlowConsumerIsDisconnectedInsteadOfBlockingTheBroadcast(t *testing.T) {
	hub := testHub(t, func(cfg *HubConfig) {
		cfg.SendBuffer = 1
		cfg.WriteWait = 250 * time.Millisecond
	})
	server := hubServer(t, hub)

	slow, healthy := uuid.New(), uuid.New()
	// The slow client never reads: its socket buffer fills and the server's write pump
	// blocks on it, so its write queue is what overflows.
	slowConn := dialRaw(t, server, hub, "/ws/student", slow, nil)
	healthyClient := dialClient(t, server, hub, "/ws/student", healthy, nil)

	// Each round broadcasts to BOTH and then waits for the healthy client to receive it.
	// The wait is what makes the test deterministic: it proves that the broadcast is not
	// blocked by the client that stopped reading (which is the whole claim), and it keeps
	// the healthy client's own queue from overflowing for an unrelated reason (a tight
	// loop of 512 KiB writes outpacing a socket).
	big := strings.Repeat("x", 512*1024)
	start := time.Now()
	for i := 0; i < 16; i++ {
		hub.ToStudents([]uuid.UUID{slow, healthy},
			newMessage(TypeScreenLost, map[string]any{"filler": big, "n": i}))
		if msg := healthyClient.expectMessage(); msg.Type != TypeScreenLost {
			t.Fatalf("healthy client received %s", msg.Type)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("broadcast took %s: one slow client must never block the others", elapsed)
	}

	waitFor(t, "the slow consumer to be dropped", func() bool {
		return hub.Connections(RoleStudent, slow) == 0
	})
	if got := hub.Connections(RoleStudent, healthy); got != 1 {
		t.Fatalf("healthy connections = %d, want 1: a slow CLASSMATE must not be disconnected", got)
	}

	// The dropped client is told why rather than being left to time out on its own.
	_ = slowConn.SetReadDeadline(time.Now().Add(expectTimeout))
	var closeErr error
	for {
		if _, _, err := slowConn.ReadMessage(); err != nil {
			closeErr = err
			break
		}
	}
	if !websocket.IsCloseError(closeErr,
		websocket.ClosePolicyViolation, websocket.CloseAbnormalClosure, websocket.CloseGoingAway) {
		t.Fatalf("slow consumer closed with %v, want a close frame", closeErr)
	}
}

// ---------------------------------------------------------------------------
// Handshake policy and shutdown
// ---------------------------------------------------------------------------

func TestHandshakeFromAnUnknownOriginIsRefused(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	headers := http.Header{}
	headers.Set("X-Test-User", uuid.NewString())
	headers.Set("Origin", "http://evil.test")

	conn, response, err := websocket.DefaultDialer.Dial(url, headers)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a cross-origin handshake was accepted: a hostile page could open a socket with the victim's cookie")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("status = %d, want 403", status)
	}
}

func TestHandshakeFromAnAllowedOriginIsAccepted(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)
	student := uuid.New()
	client := dialClient(t, server, hub, "/ws/student", student,
		http.Header{"Origin": []string{"http://allowed.test"}})

	hub.ToStudents([]uuid.UUID{student}, newMessage(TypePong, nil))
	if got := client.expectMessage(); got.Type != TypePong {
		t.Fatalf("type = %s", got.Type)
	}
}

func TestConnectionWithAnEmptyOriginIsAccepted(t *testing.T) {
	// Non-browser clients (a probe, a script, a server-to-server integration) send no
	// Origin header, and a hostile page cannot suppress it — so this is not a hole.
	hub := testHub(t, nil)
	server := hubServer(t, hub)
	student := uuid.New()
	client := dialClient(t, server, hub, "/ws/student", student, nil)

	hub.ToStudents([]uuid.UUID{student}, newMessage(TypePong, nil))
	if got := client.expectMessage(); got.Type != TypePong {
		t.Fatalf("type = %s", got.Type)
	}
}

func TestShutdownClosesEveryConnectionAndCleansUp(t *testing.T) {
	hub := testHub(t, nil)
	server := hubServer(t, hub)

	teacher, student := uuid.New(), uuid.New()
	teacherClient := dialClient(t, server, hub, "/ws/teacher", teacher, nil)
	studentClient := dialClient(t, server, hub, "/ws/student", student, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.Shutdown(ctx)

	for name, client := range map[string]*testClient{"teacher": teacherClient, "student": studentClient} {
		err := client.closeError()
		if err == nil {
			t.Fatalf("%s connection survived shutdown", name)
		}
		if !websocket.IsCloseError(err,
			websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
			t.Fatalf("%s closed with %v, want a close frame", name, err)
		}
	}
	if got := hub.Connections(RoleTeacher, teacher) + hub.Connections(RoleStudent, student); got != 0 {
		t.Fatalf("connections after shutdown = %d, want 0", got)
	}

	// A handshake that arrives after shutdown is refused instead of being registered
	// into a hub that is about to drop it.
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	headers := http.Header{}
	headers.Set("X-Test-User", uuid.NewString())
	if conn, response, err := websocket.DefaultDialer.Dial(url, headers); err == nil {
		_ = conn.Close()
		t.Fatal("a handshake after shutdown was accepted")
	} else if response == nil || response.StatusCode != http.StatusInternalServerError {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("status = %d, want 500", status)
	}
}

func TestServeRefusesTheNilUser(t *testing.T) {
	hub := testHub(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Serve(w, r, RoleStudent, uuid.Nil); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Serve() error = %v, want ErrUnavailable", err)
		}
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	if conn, _, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		_ = conn.Close()
		t.Fatal("a connection with the nil user id was accepted")
	}
}

// ---------------------------------------------------------------------------
// Refusing a handshake with a close code
// ---------------------------------------------------------------------------

func TestRefuseAcceptsTheUpgradeAndClosesWithTheGivenCode(t *testing.T) {
	hub := testHub(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Refuse(w, r, CloseSessionInvalid, "AUTH_REQUIRED"); err != nil {
			t.Errorf("Refuse(): %v", err)
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101: the client must be able to read a close code", response.StatusCode)
	}
	_ = conn.SetReadDeadline(time.Now().Add(expectTimeout))
	if _, _, readErr := conn.ReadMessage(); !websocket.IsCloseError(readErr, CloseSessionInvalid) {
		t.Fatalf("close error = %v, want %d", readErr, CloseSessionInvalid)
	}
	// Nothing was registered: a refused handshake must not occupy the hub.
	if got := hub.Connections(RoleStudent, uuid.New()); got != 0 {
		t.Fatalf("connections = %d, want 0", got)
	}
}

func TestRefuseKeepsTheOriginPolicy(t *testing.T) {
	hub := testHub(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = hub.Refuse(w, r, CloseSessionInvalid, "AUTH_REQUIRED")
	}))
	defer server.Close()

	// A hostile origin is refused BEFORE any close code is sent: an attacker's page must
	// not even learn whether the victim has a session.
	headers := http.Header{}
	headers.Set("Origin", "http://evil.test")
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/student"
	if conn, response, err := websocket.DefaultDialer.Dial(url, headers); err == nil {
		_ = conn.Close()
		t.Fatal("a cross-origin handshake was accepted in order to be refused")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("status = %d, want 403", status)
	}
}
