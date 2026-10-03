package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// Role is which entry point a connection came in through (§5). It is part of a
// connection's identity, so a student cookie can never produce a teacher connection.
type Role string

const (
	RoleStudent Role = "student"
	RoleTeacher Role = "teacher"
)

// HubConfig carries the hub's timing and policy knobs.
//
// They are configuration and not constants because the tests need short timeouts: a
// liveness rule that can only be exercised by waiting 60 seconds is a rule that is
// never tested.
type HubConfig struct {
	// Logger receives connection lifecycle lines. A nil logger falls back to
	// slog.Default.
	Logger *slog.Logger
	// AllowedOrigins is the CORS allowlist of the API (same list, one source of truth).
	// It is the defense against cross-site WebSocket hijacking — see Hub.Serve.
	AllowedOrigins []string
	// WriteWait bounds one write to one slow client. Without it a single stalled TCP
	// connection could hold a broadcast goroutine forever.
	WriteWait time.Duration
	// PongWait is how long a connection may stay silent before it is considered dead.
	// It must be larger than PingInterval, or every healthy client is disconnected.
	PongWait time.Duration
	// PingInterval is how often the server sends a control PING.
	PingInterval time.Duration
	// SendBuffer is the per-connection write queue. It is small on purpose: the queue
	// exists to absorb a burst, not to buffer a client that stopped reading.
	SendBuffer int
	// ReadLimit bounds one client message. The contract has one message type of a few
	// bytes, so the limit is a safety net against a client that streams megabytes into
	// a supervision server.
	ReadLimit int64
	// Metrics is the §77 instrumentation: live connection gauge, message counters and
	// the slow-consumer counter. It is optional — a nil value makes every recording a
	// no-op — and it is recorded at the hub rather than in the HTTP handler because the
	// hub is what owns connection lifetime and the write queue.
	Metrics *metrics.Metrics
}

// defaultHubConfig returns the production timings, named so the reasoning is greppable.
func defaultHubConfig() HubConfig {
	return HubConfig{
		WriteWait: 5 * time.Second,
		// PongWait/PingInterval of 60s/25s: a browser tab that is backgrounded still
		// answers control pings, so this detects a dead TCP connection (a laptop that
		// slept, a NAT that dropped the flow) within a minute without generating
		// noticeable traffic (2 frames a minute per connection).
		PongWait:     60 * time.Second,
		PingInterval: 25 * time.Second,
		// 32 messages: a classroom close produces one message per student, and a burst
		// of a few dozen must not disconnect a healthy client that is merely a moment
		// behind.
		SendBuffer: 32,
		ReadLimit:  4096,
	}
}

// Hub is the process-local registry of live WebSocket connections (§47).
//
// # The scoping rule is the API
//
// There is deliberately no `Broadcast` method. A connection is registered under
// (role, userID), and the only ways to send are ToStudents(ids) and ToTeacher(id) —
// so "this student's screen was lost" cannot be delivered to another student without
// first lying about which student it is about. §26's isolation is therefore a property
// of the type, not a filter somebody has to remember to apply, and the tests assert
// exactly that.
//
// # What the hub does not know
//
// It knows nothing about classrooms, rosters or ownership; it only delivers to
// connections that are already open. Deciding WHO should receive a message is
// Service's job (it queries the roster), which keeps this type free of I/O and
// trivially testable.
type Hub struct {
	cfg      HubConfig
	logger   *slog.Logger
	metrics  *metrics.Metrics
	upgrader websocket.Upgrader

	mu       sync.RWMutex
	students map[uuid.UUID]map[*conn]struct{}
	teachers map[uuid.UUID]map[*conn]struct{}
	// closing is set by Shutdown. A handshake that arrives after it is refused instead
	// of being registered into a hub that is about to drop it.
	closing bool
	// wg tracks the write pumps, so Shutdown can wait for the close frames to leave.
	wg sync.WaitGroup
}

// NewHub builds a hub.
func NewHub(cfg HubConfig) *Hub {
	defaults := defaultHubConfig()
	if cfg.WriteWait <= 0 {
		cfg.WriteWait = defaults.WriteWait
	}
	if cfg.PongWait <= 0 {
		cfg.PongWait = defaults.PongWait
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaults.PingInterval
	}
	if cfg.SendBuffer <= 0 {
		cfg.SendBuffer = defaults.SendBuffer
	}
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = defaults.ReadLimit
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		allowed[origin] = struct{}{}
	}
	return &Hub{
		cfg:      cfg,
		logger:   logger,
		metrics:  cfg.Metrics,
		students: make(map[uuid.UUID]map[*conn]struct{}),
		teachers: make(map[uuid.UUID]map[*conn]struct{}),
		upgrader: websocket.Upgrader{
			// The browser-facing default (4096) would reject nothing we send: a
			// SCREEN_LOST envelope is a few hundred bytes. ReadBufferSize is left at
			// the library default, which accepts the tiny messages the contract allows.
			WriteBufferSize: 4096,
			// CheckOrigin replaces gorilla's default (which accepts any Origin when the
			// request has no Host check to make) with the API's own origin allowlist.
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// No Origin header: not a browser. Browsers ALWAYS send Origin on a
					// WebSocket handshake, so this is curl, a test client or a
					// server-to-server integration — none of which can be hijacked by a
					// hostile page, because a hostile page cannot suppress the header.
					return true
				}
				_, ok := allowed[origin]
				return ok
			},
		},
	}
}

// conn is one live WebSocket connection.
//
// The zero value is not usable: a connection is created by Serve, registered under
// (role, userID) and then owned by two goroutines — the read loop and the write pump.
type conn struct {
	hub    *Hub
	id     uuid.UUID
	role   Role
	userID uuid.UUID
	ws     *websocket.Conn
	// send is the per-connection write queue. Only the write pump reads it; every
	// other goroutine (a broadcast, the read loop's PONG reply) only enqueues.
	send chan Message
	// done is closed exactly once, when the connection must stop. The write pump turns
	// it into a close frame.
	done      chan struct{}
	closeOnce sync.Once
	// closeMu guards the code/text pair: a close reason is written once by whichever
	// goroutine ends the connection and read by the write pump.
	closeMu sync.Mutex
	code    int
	text    string
	logger  *slog.Logger
}

// ErrUnavailable is returned when a handshake cannot be served at all — the hub is
// shutting down, or it was given a principal it cannot register. It is distinguished from
// an upgrade failure because the HTTP layer must answer differently: here nothing has
// been written yet (so a clean 503 is still possible), while a failed upgrade has already
// produced its own status line.
var ErrUnavailable = errors.New("realtime: websocket hub is unavailable")

// Close codes this endpoint sends to a browser, in the private-use range (4000-4999,
// RFC 6455 §7.4.2).
//
// WHY a close code and not an HTTP status: a browser CANNOT read the status of a failed
// WebSocket handshake — it reports only "the connection failed", identically for 401, 403
// and a typo in the URL. The frontend therefore has no way to tell "your session expired,
// go to the login page" from "the network blipped, retry" unless the server accepts the
// upgrade and then closes with a code. That is what Refuse does, and these two constants
// are the contract (documented in docs/architecture/realtime-flow.md §5.6).
const (
	// CloseSessionInvalid means there is no live session for this entry point's cookie.
	// The client should stop reconnecting and send the user to the login page.
	CloseSessionInvalid = 4401
	// CloseRoleForbidden means the caller IS authenticated, but on another entry point
	// (§5/§37). Reconnecting cannot help; the frontend has a routing bug.
	CloseRoleForbidden = 4403
)

// Supervisor is the hub as the HTTP layer sees it: it upgrades ONE already
// authenticated request and serves it until the client goes away, or refuses a request
// it must not serve.
//
// WHY the hub does not do the authentication itself: the three entry points, their
// cookies, their role checks and their CSRF rules all live in internal/httpapi, and a
// second implementation here would be a second place for the rule to be wrong (§37).
// The hub receives a role and a user id it can trust because the only caller is a
// handler that ran the same decision as every other route.
type Supervisor interface {
	Serve(w http.ResponseWriter, r *http.Request, role Role, userID uuid.UUID) error
	// Refuse ends a handshake that must not become a session, using a close code the
	// browser can read. It upgrades (with the same Origin policy as Serve) and closes
	// immediately; the connection is never registered and never receives a message.
	Refuse(w http.ResponseWriter, r *http.Request, code int, reason string) error
}

// Serve upgrades an authenticated request into a hub connection and runs it until the
// client goes away.
//
// The caller (internal/httpapi) has already established WHO the connection belongs to:
// the role comes from the entry point's cookie and the user id from the session row, so
// nothing here trusts a query parameter. That is why the WebSocket URLs carry no token
// (§47): a token in a URL ends up in access logs, proxy logs and browser history.
//
// It blocks for the lifetime of the connection, which is what an http.Handler is for.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, role Role, userID uuid.UUID) error {
	if h == nil {
		return ErrUnavailable
	}
	if userID == uuid.Nil {
		// Unreachable through the HTTP layer (an authenticated principal always has an
		// id); refused so a programming error cannot register a connection under the nil
		// UUID, which would then receive every message addressed to the zero value.
		return ErrUnavailable
	}

	// WHY 用写锁而不是读锁，并且在这里就 wg.Add：Shutdown 先置 closing 再 wg.Wait()。
	// 如果 Add 发生在 Wait 之后（旧写法：先读锁检查、升级完再 Add），就构成
	// WaitGroup 的经典误用——`go test -race` 会在"关服时正好有人握手的"场景里报竞争，
	// 而这类时序在真实部署（滚动重启）里并不罕见。把判定与 Add 放进同一把锁，
	// 使"进入注册"与"开始关闭"二者必有一个先发生。
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return ErrUnavailable
	}
	h.wg.Add(1)
	h.mu.Unlock()

	// Add 之后到"把连接的写泵交给 wg 管理"之间有若干早退路径（升级失败、构造失败…）。
	// 每一条都必须把计数还回去，否则 Shutdown 的 wg.Wait() 会一直等到超时——
	// 一次失败的握手就足以让优雅关闭多花几秒并打出一条误导性的超时告警。
	handedOff := false
	defer func() {
		if !handedOff {
			h.wg.Done()
		}
	}()

	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written the HTTP error (a 400 for a malformed handshake, a
		// 403 for a rejected origin).
		h.logger.Info("websocket upgrade rejected",
			"role", string(role),
			"user_id", userID.String(),
			"path", r.URL.Path,
			"origin", r.Header.Get("Origin"),
			"error", err,
		)
		return err
	}

	c := &conn{
		hub:    h,
		id:     uuid.New(),
		role:   role,
		userID: userID,
		ws:     ws,
		send:   make(chan Message, h.cfg.SendBuffer),
		done:   make(chan struct{}),
		logger: h.logger.With("role", string(role), "user_id", userID.String(), "connection_id", uuid.NewString()),
	}
	h.register(c)
	c.logger.Info("websocket connected", "action", "ws_connected", "path", r.URL.Path)

	writeDone := make(chan struct{})
	handedOff = true
	go func() {
		defer h.wg.Done()
		defer close(writeDone)
		c.writePump()
	}()

	// The read loop runs on the handler's goroutine: when it returns, the request is
	// over and the connection is torn down. Whatever ended it (client close, protocol
	// error, read deadline, a message flood) is logged once, here.
	reason := c.readPump()
	c.close(reason.code, reason.text)
	// Wait for the write pump so the connection is unregistered before this handler
	// returns. Without the wait, a client that reconnects immediately could race its own
	// stale registration, and a test that closes a socket could not know when the hub
	// stopped counting it.
	select {
	case <-writeDone:
	case <-time.After(h.cfg.WriteWait):
		// The write pump is stuck on a dead socket. Abandon it: the socket is closed
		// below either way and the process must not accumulate goroutines for clients
		// that never read.
		c.logger.Warn("websocket write pump did not stop in time", "action", "ws_write_pump_stuck")
	}
	c.logger.Info("websocket disconnected",
		"action", "ws_disconnected",
		"code", reason.code,
		"reason", reason.text,
	)
	return nil
}

// Refuse answers a handshake this endpoint must not serve.
//
// It accepts the upgrade and immediately closes with the given code, because that is the
// only channel a browser can read (see CloseSessionInvalid). The connection is NEVER
// registered with the hub: an unauthenticated client gets a socket that exists for one
// frame and receives nothing — it cannot observe any classroom, and it cannot use the hub
// as a resource (no reader goroutine, no write queue, no registration).
//
// If the request is not a WebSocket handshake at all (curl, a health check, an ops probe),
// the upgrade fails and the HTTP error written by the upgrader is what the caller sees;
// internal/httpapi answers those with the standard error envelope instead of calling this
// method, so the REST-shaped surface is unchanged.
func (h *Hub) Refuse(w http.ResponseWriter, r *http.Request, code int, reason string) error {
	if h == nil {
		return ErrUnavailable
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// The upgrader has already written the HTTP error (403 for a rejected Origin, 400
		// for a malformed handshake).
		h.logger.Info("websocket handshake refused before the upgrade",
			"path", r.URL.Path,
			"origin", r.Header.Get("Origin"),
			"error", err,
		)
		return err
	}
	defer func() { _ = ws.Close() }()

	_ = ws.SetWriteDeadline(time.Now().Add(h.cfg.WriteWait))
	message := websocket.FormatCloseMessage(code, reason)
	if err := ws.WriteMessage(websocket.CloseMessage, message); err != nil {
		return err
	}
	h.logger.Info("websocket handshake refused with a close code",
		"action", "ws_refused",
		"path", r.URL.Path,
		"close_code", code,
		"reason", reason,
	)
	return nil
}

// readPump reads client messages until the connection ends, and reports why.
//
// The contract is one message: {"type":"PING"} → {"type":"PONG"} (§47). Everything else
// is IGNORED and logged at Debug — deliberately not "rejected with an error frame",
// because an unknown message is more likely a newer frontend than an attack, and
// because parsing business commands here would put a second authorization path into the
// server (the REST API is the only place a state change may start).
func (c *conn) readPump() closeReason {
	c.ws.SetReadLimit(c.hub.cfg.ReadLimit)
	_ = c.ws.SetReadDeadline(time.Now().Add(c.hub.cfg.PongWait))
	c.ws.SetPongHandler(func(string) error {
		// A pong is liveness, nothing else: it carries no data and cannot change state.
		return c.ws.SetReadDeadline(time.Now().Add(c.hub.cfg.PongWait))
	})

	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return closeReason{code: websocket.CloseNormalClosure, text: "client closed the connection"}
			}
			return closeReason{code: websocket.CloseAbnormalClosure, text: err.Error()}
		}
		// Any traffic proves the client is alive, not only a pong: an application-level
		// PING is also a heartbeat, and disconnecting a chatty client would be absurd.
		_ = c.ws.SetReadDeadline(time.Now().Add(c.hub.cfg.PongWait))

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			c.hub.metrics.IncWSMessage(metrics.DirectionIn, inboundMessageLabel(""))
			c.logger.Debug("websocket message ignored",
				"action", "ws_message_ignored", "reason", "not a json object")
			continue
		}
		c.hub.metrics.IncWSMessage(metrics.DirectionIn, inboundMessageLabel(envelope.Type))
		switch MessageType(envelope.Type) {
		case TypePing:
			c.enqueue(newMessage(TypePong, nil))
		default:
			// Logged WITHOUT the body: this is unvalidated client input, and a body echo
			// is how a log becomes a place where credentials and personal data end up
			// (§59).
			c.logger.Debug("websocket message ignored",
				"action", "ws_message_ignored",
				"message_type", envelope.Type,
				"note", "only PING is accepted; client messages are never commands",
			)
		}
	}
}

// writePump owns the socket's write side.
//
// One goroutine per connection owns every write, which is what makes a broadcast safe:
// gorilla forbids concurrent writes to one connection, and the alternative (a mutex
// around the socket) would let one slow client block the goroutine that is broadcasting
// to everybody else.
func (c *conn) writePump() {
	ticker := time.NewTicker(c.hub.cfg.PingInterval)
	defer func() {
		ticker.Stop()
		c.hub.unregister(c)
		_ = c.ws.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(c.hub.cfg.WriteWait))
			if !ok {
				_ = c.ws.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.ws.WriteJSON(msg); err != nil {
				return
			}
			// Counted after the write succeeded: a message that never left the
			// process is not traffic, and counting it would hide a broken socket
			// behind a healthy-looking rate.
			c.hub.metrics.IncWSMessage(metrics.DirectionOut, outboundMessageLabel(msg.Type))

		case <-ticker.C:
			// A control PING is the liveness probe: the browser answers it without any
			// application code, so a client that stopped responding (a frozen tab, a
			// dead NAT entry) is detected even if its JavaScript never runs again.
			_ = c.ws.SetWriteDeadline(time.Now().Add(c.hub.cfg.WriteWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.done:
			// Graceful close (server shutdown, or this connection dropped as too slow).
			// The client gets a reason so it can tell "the server is going away" from
			// "my network died" and choose between a backoff and an immediate retry.
			_ = c.ws.SetWriteDeadline(time.Now().Add(c.hub.cfg.WriteWait))
			_ = c.ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(c.closeCode(), c.closeText()))
			return
		}
	}
}

// enqueue hands one message to the connection's write queue.
//
// A FULL QUEUE DROPS THE MESSAGE AND THE CONNECTION, and that is the load-bearing
// decision of this function (§47). The alternatives are worse:
//
//   - Blocking until there is room would make one client that stopped reading (a
//     suspended tab, a machine that lost its network) stall the broadcast for every
//     other client in the process, and — because the webhook handler broadcasts
//     synchronously — would eventually stall webhook processing itself.
//   - Dropping silently would leave that client with a stale picture of the lesson
//     while believing it is up to date, which is exactly the "the teacher's wall
//     disagrees with reality" failure this phase exists to prevent.
//
// So the connection is closed, the client reconnects (the frontend's reconnect loop is
// part of the contract), and the first thing it does is fetch the pull snapshot
// (§51) — which is the authoritative state, not the messages it missed.
func (c *conn) enqueue(msg Message) {
	select {
	case c.send <- msg:
	default:
		c.hub.metrics.IncWSSlowConsumerDisconnect()
		c.hub.logger.Warn("websocket client too slow; dropping the connection",
			"action", "ws_slow_consumer",
			"role", string(c.role),
			"user_id", c.userID.String(),
			"connection_id", c.id.String(),
			"message_type", string(msg.Type),
			"queue_size", c.hub.cfg.SendBuffer,
			"consequence", "the client reconnects and re-reads the authoritative snapshot",
		)
		c.close(websocket.ClosePolicyViolation, "client too slow to receive events")
	}
}

// close tears the connection down exactly once.
func (c *conn) close(code int, text string) {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.code = code
		c.text = text
		c.closeMu.Unlock()
		close(c.done)
	})
}

func (c *conn) closeCode() int {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.code == 0 {
		return websocket.CloseNormalClosure
	}
	return c.code
}

func (c *conn) closeText() string {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.text
}

// register adds a connection to the registry.
//
// The live-connection gauge is incremented here and decremented in unregister, which
// are exactly paired: a connection is registered once (right after a successful
// upgrade) and unregistered once (by the write pump's deferred cleanup, whatever
// ended it). Recording it anywhere else would drift on the paths that end a socket
// without a message — a client close, a read deadline, a policy violation.
func (h *Hub) register(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics.AddWSConnection(metricsRole(c.role), 1)
	index := h.students
	if c.role == RoleTeacher {
		index = h.teachers
	}
	if index[c.userID] == nil {
		index[c.userID] = make(map[*conn]struct{})
	}
	index[c.userID][c] = struct{}{}
}

// unregister removes a connection from the registry, whatever ends it.
func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics.AddWSConnection(metricsRole(c.role), -1)
	index := h.students
	if c.role == RoleTeacher {
		index = h.teachers
	}
	conns := index[c.userID]
	delete(conns, c)
	if len(conns) == 0 {
		// Delete the empty bucket: a server that has been up for a term must not keep a
		// map entry per student who ever connected.
		delete(index, c.userID)
	}
}

// ToStudents delivers one message to every live connection of these students (§47).
//
// The list is user ids resolved from the classroom roster by Service, so a message can
// only reach a student who is authorized for the classroom the message is about (§26).
// Students with no connection are simply skipped: a broadcast is not a delivery
// guarantee, and the pull endpoints exist for whoever missed it.
func (h *Hub) ToStudents(students []uuid.UUID, msg Message) {
	if h == nil || len(students) == 0 {
		return
	}
	targets := h.collect(h.students, students)
	h.deliver(targets, msg)
}

// ToTeacher delivers one message to every live connection of one teacher.
func (h *Hub) ToTeacher(teacherID uuid.UUID, msg Message) {
	if h == nil || teacherID == uuid.Nil {
		return
	}
	targets := h.collect(h.teachers, []uuid.UUID{teacherID})
	h.deliver(targets, msg)
}

// collect snapshots the connections of the given users.
//
// The snapshot is taken under the read lock and delivered outside it: enqueue may close
// a connection (a full queue), and closing takes the write lock. Delivering while
// holding the read lock would deadlock the hub on its own slow consumer.
func (h *Hub) collect(index map[uuid.UUID]map[*conn]struct{}, users []uuid.UUID) []*conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	targets := make([]*conn, 0, len(users))
	for _, userID := range users {
		for c := range index[userID] {
			targets = append(targets, c)
		}
	}
	return targets
}

func (h *Hub) deliver(targets []*conn, msg Message) {
	for _, c := range targets {
		c.enqueue(msg)
	}
}

// Connections reports how many live connections a user has on one entry point.
//
// It exists for the tests (waiting for a handshake to be registered instead of sleeping)
// and for the shutdown log line. It is not an authorization input.
func (h *Hub) Connections(role Role, userID uuid.UUID) int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	index := h.students
	if role == RoleTeacher {
		index = h.teachers
	}
	return len(index[userID])
}

// Shutdown closes every live connection and waits for the close frames to leave.
//
// WHY this is separate from closing the HTTP server: WebSocket connections are
// hijacked, so http.Server.Shutdown does not wait for them (and would leave the
// clients to discover the dead socket on their next PING). Telling them "going away"
// first is what lets the frontends reconnect deliberately — with a backoff — instead of
// treating a deploy as a network failure.
func (h *Hub) Shutdown(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closing = true
	all := make([]*conn, 0, 16)
	for _, index := range []map[uuid.UUID]map[*conn]struct{}{h.students, h.teachers} {
		for _, conns := range index {
			for c := range conns {
				all = append(all, c)
			}
		}
	}
	h.mu.Unlock()

	h.logger.Info("closing websocket connections", "action", "ws_shutdown", "connections", len(all))
	for _, c := range all {
		c.close(websocket.CloseGoingAway, "server is shutting down")
	}

	stopped := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		// The deadline is the contract: the process must exit, and the sockets are
		// closed by the write pumps' deferred Close (or by the OS).
		h.logger.Warn("websocket shutdown timed out", "action", "ws_shutdown_timeout")
	}
}

// metricsRole maps a connection's entry point onto the metric label.
//
// The mapping is explicit rather than a string conversion so that adding a role to
// this package cannot silently create a new time series: the compiler forces a
// decision here, and the label set stays closed (see docs/architecture/observability.md).
func metricsRole(role Role) string {
	if role == RoleTeacher {
		return metrics.WSRoleTeacher
	}
	return metrics.WSRoleStudent
}

// inboundMessageLabel bounds the `type` label of a CLIENT message.
//
// The type comes from the client, so it is unbounded: a script that sends
// {"type":"<30 random characters>"} in a loop would otherwise create one time
// series per message, and a scraper would fall over long before the hub did. Only
// the single message type this protocol accepts keeps its name; everything else
// collapses into a fixed label.
func inboundMessageLabel(messageType string) string {
	switch MessageType(messageType) {
	case TypePing:
		return messageType
	case "":
		return "unparseable"
	default:
		return "other"
	}
}

// outboundMessageLabel maps a server message onto its label. Server types are a
// closed set generated by this codebase, so the value passes through; the fallback
// exists so a zero-value message cannot produce an empty label.
func outboundMessageLabel(messageType MessageType) string {
	if messageType == "" {
		return "unknown"
	}
	return string(messageType)
}

// closeReason is why a connection ended, in the vocabulary of a WebSocket close code.
type closeReason struct {
	code int
	text string
}

// Compile-time assertion: the hub is the Broadcaster the services are written against.
var _ Broadcaster = (*Hub)(nil)
