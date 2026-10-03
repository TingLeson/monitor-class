package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// This file holds the two Phase 11 additions to the realtime layer:
//
//   - the proof that http.Server.WriteTimeout does not cut a business WebSocket,
//     which is what makes an explicit server timeout safe to configure; and
//   - the §77 instrumentation assertions (live connections, messages, slow
//     consumers), including the bounded-label rule for client-chosen message types.

// newRealServer serves the hub from a real http.Server with explicit timeouts.
//
// httptest.NewServer is deliberately not used: it constructs its own server with no
// timeouts, and the whole subject of the test below is the deadline net/http puts on
// the connection of a hijacked request.
func newRealServer(t *testing.T, hub *Hub, timeouts func(*http.Server)) *httptestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	for _, path := range []string{"/ws/student", "/ws/teacher"} {
		path := path
		role := RoleStudent
		if strings.HasSuffix(path, "teacher") {
			role = RoleTeacher
		}
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			userID, err := uuid.Parse(r.Header.Get("X-Test-User"))
			if err != nil {
				http.Error(w, "missing test user", http.StatusBadRequest)
				return
			}
			if err := hub.Serve(w, r, role, userID); errors.Is(err, ErrUnavailable) {
				http.Error(w, "hub unavailable", http.StatusInternalServerError)
			}
		})
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       time.Second,
		WriteTimeout:      250 * time.Millisecond,
		IdleTimeout:       time.Second,
	}
	if timeouts != nil {
		timeouts(server)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()

	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	}
	t.Cleanup(stop)
	return &httptestServer{addr: listener.Addr().String()}
}

// httptestServer is the minimal surface these tests need from a real server.
type httptestServer struct{ addr string }

func (s *httptestServer) wsURL(path string) string { return "ws://" + s.addr + path }

// TestWriteTimeoutDoesNotCutWebSockets is the proof behind §63's WriteTimeout.
//
// WHY this needs a test and not a comment: an explicit WriteTimeout is mandatory for
// hardening (it stops a stuck client from holding a response forever), and it is
// widely believed that it also kills WebSockets — which would break §47's sockets
// after 30 seconds of a lesson. It does not, for two independent reasons, and this
// test exercises both on a real socket:
//
//  1. net/http CLEARS the connection deadline when a handler hijacks the connection,
//     so the server's WriteTimeout stops applying the moment the upgrade succeeds.
//  2. The hub then manages its own deadlines around every frame it writes
//     (HubConfig.WriteWait), which is what keeps a stalled client from blocking the
//     write pump regardless of the server settings.
//
// The test waits past the timeout (250ms) on purpose: a test that exchanged a message
// immediately would pass even if the deadline were inherited.
func TestWriteTimeoutDoesNotCutWebSockets(t *testing.T) {
	hub := testHub(t, func(cfg *HubConfig) {
		// Keep the hub's own liveness rules out of the way: the subject is the
		// SERVER's timeout, not the pong deadline.
		cfg.PongWait = 30 * time.Second
		cfg.PingInterval = 30 * time.Second
	})
	server := newRealServer(t, hub, nil)
	userID := uuid.New()

	headers := http.Header{}
	headers.Set("X-Test-User", userID.String())
	conn, _, err := websocket.DefaultDialer.Dial(server.wsURL("/ws/student"), headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	waitFor(t, "connection registered", func() bool { return hub.Connections(RoleStudent, userID) == 1 })

	// Sleep past ReadTimeout and WriteTimeout (both 1s/250ms above): the socket must
	// still be usable afterwards.
	time.Sleep(1500 * time.Millisecond)

	if err := conn.WriteJSON(Message{Type: TypePing}); err != nil {
		t.Fatalf("client write after the server timeouts: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(expectTimeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("client read after the server timeouts: %v (the write timeout cut the socket)", err)
	}
	var pong Message
	if err := jsonUnmarshal(raw, &pong); err != nil {
		t.Fatalf("response is not an envelope: %v (%s)", err, raw)
	}
	if pong.Type != TypePong {
		t.Fatalf("response type = %q, want %q", pong.Type, TypePong)
	}

	// The server's own direction must work too, which is what WriteTimeout would
	// break: enforce it by pushing a message and reading it.
	hub.ToStudents([]uuid.UUID{userID}, newMessage(TypeScreenLost, map[string]any{"reason": "test"}))
	_ = conn.SetReadDeadline(time.Now().Add(expectTimeout))
	_, raw, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("server write after the write timeout: %v", err)
	}
	var pushed Message
	if err := jsonUnmarshal(raw, &pushed); err != nil {
		t.Fatalf("pushed message is not an envelope: %v (%s)", err, raw)
	}
	if pushed.Type != TypeScreenLost {
		t.Fatalf("pushed type = %q, want %q", pushed.Type, TypeScreenLost)
	}
}

// TestHubRecordsConnectionAndMessageMetrics covers the §77 instrumentation of the
// socket layer.
func TestHubRecordsConnectionAndMessageMetrics(t *testing.T) {
	m := metrics.New()
	hub := testHub(t, func(cfg *HubConfig) { cfg.Metrics = m })
	server := hubServer(t, hub)

	student := uuid.New()
	headers := http.Header{}
	headers.Set("Origin", "http://allowed.test")
	client := dialClient(t, server, hub, "/ws/student", student, headers)

	if got := m.WSConnections.With(metrics.WSRoleStudent).Value(); got != 1 {
		t.Fatalf("ws_connections{student} = %d, want 1", got)
	}

	// Inbound: a PING keeps its own label, so the protocol's one accepted message is
	// visible on a dashboard instead of being buried in "other".
	if err := client.conn.WriteJSON(Message{Type: TypePing}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if msg := client.expectMessage(); msg.Type != TypePong {
		t.Fatalf("message type = %q, want PONG", msg.Type)
	}
	waitFor(t, "the inbound PING to be counted", func() bool {
		return m.WSMessagesTotal.With(metrics.DirectionIn, string(TypePing)).Value() == 1
	})
	waitFor(t, "the outbound PONG to be counted", func() bool {
		return m.WSMessagesTotal.With(metrics.DirectionOut, string(TypePong)).Value() == 1
	})

	// A client-chosen type must not become a label value: the label set has to stay
	// closed no matter what a browser sends.
	for _, bogus := range []string{"a" + uuid.NewString(), "b" + uuid.NewString()} {
		if err := client.conn.WriteJSON(map[string]string{"type": bogus}); err != nil {
			t.Fatalf("write bogus: %v", err)
		}
	}
	waitFor(t, "bogus types to be collapsed", func() bool {
		return m.WSMessagesTotal.With(metrics.DirectionIn, "other").Value() == 2
	})
	if got := m.WSMessagesTotal.With(metrics.DirectionIn, "a"+uuid.Nil.String()).Value(); got != 0 {
		t.Fatalf("a client-chosen type produced a series: %d", got)
	}

	// Closing the socket must return the gauge to zero, whatever ended it.
	_ = client.conn.Close()
	waitFor(t, "the connection gauge to return to 0", func() bool {
		return m.WSConnections.With(metrics.WSRoleStudent).Value() == 0
	})
}

// TestSlowConsumerIsCounted pins the §47 back-pressure decision to a number an
// operator can alert on: a dropped connection is intentional, and it must be visible.
func TestSlowConsumerIsCounted(t *testing.T) {
	m := metrics.New()
	hub := testHub(t, func(cfg *HubConfig) {
		cfg.Metrics = m
		// A queue of one, and a short write deadline so the write pump gives up on the
		// dead socket quickly.
		cfg.SendBuffer = 1
		cfg.WriteWait = 250 * time.Millisecond
		cfg.PongWait = 30 * time.Second
		cfg.PingInterval = 30 * time.Second
	})
	server := hubServer(t, hub)

	student := uuid.New()
	headers := http.Header{}
	headers.Set("Origin", "http://allowed.test")
	// dialRaw is the connection NOTHING reads: its socket buffer fills up, the write
	// pump blocks on it and the (size-1) queue is what overflows. Small messages would
	// be absorbed by the kernel buffers and never reach the queue, so the filler is a
	// few hundred kilobytes per message.
	conn := dialRaw(t, server, hub, "/ws/student", student, headers)

	big := strings.Repeat("x", 512*1024)
	for i := 0; i < 16 && m.WSSlowConsumerDisconnectsTotal.Value() == 0; i++ {
		hub.ToStudents([]uuid.UUID{student}, newMessage(TypeScreenLost, map[string]any{"filler": big, "n": i}))
	}
	waitFor(t, "the slow consumer to be counted", func() bool {
		return m.WSSlowConsumerDisconnectsTotal.Value() >= 1
	})
	// The drop is a real disconnect, not only a counter: the connection is gone, which
	// is what tells the browser to reconnect and re-read the snapshot.
	waitFor(t, "the slow consumer to be unregistered", func() bool {
		return hub.Connections(RoleStudent, student) == 0
	})
	_ = conn.Close()
}

// jsonUnmarshal keeps the assertions above readable.
func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}
