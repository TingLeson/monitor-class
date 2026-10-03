package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// Metric names. Prefixing every family with `classwatch_` is what keeps this
// service's series from colliding with a sidecar exporter (node_exporter,
// postgres_exporter, caddy) scraped into the same Prometheus, and it makes
// `classwatch_*` a complete, greppable inventory of what this process reports.
const (
	NameHTTPRequestsTotal              = "classwatch_http_requests_total"
	NameHTTPRequestDurationSeconds     = "classwatch_http_request_duration_seconds"
	NameHTTPRequestsInFlight           = "classwatch_http_requests_in_flight"
	NameRateLimitRejectedTotal         = "classwatch_ratelimit_rejected_total"
	NameWSConnections                  = "classwatch_ws_connections"
	NameWSMessagesTotal                = "classwatch_ws_messages_total"
	NameWSSlowConsumerDisconnectsTotal = "classwatch_ws_slow_consumer_disconnects_total"
	NameSessionStatus                  = "classwatch_session_status"
	NameWebhookEventsTotal             = "classwatch_webhook_events_total"
	NameMediaCallsTotal                = "classwatch_media_calls_total"
	NameMediaCallDurationSeconds       = "classwatch_media_call_duration_seconds"
	NamePrivateTalkActive              = "classwatch_private_talk_active"
	NameDBPoolConnections              = "classwatch_db_pool_connections"
	NameBuildInfo                      = "classwatch_build_info"
)

// Label values whose cardinality is bounded by construction. They are constants
// rather than string literals at call sites so a typo cannot mint a second series
// (a `ip`/`account` mix-up would silently split a metric in two).
const (
	// Rate-limit scopes: one per policy, not one per route. A route that gains a
	// second limiter adds a scope here, which is a deliberate act.
	RateLimitScopeLogin        = "login"
	RateLimitScopeLoginAccount = "login_account"
	RateLimitScopeAPI          = "api"
	RateLimitScopePrivateTalk  = "private_talk"
	RateLimitScopeMediaToken   = "media_token"
	RateLimitScopeJoin         = "join"
	RateLimitScopeWSHandshake  = "ws_handshake"

	// Rate-limit dimensions: what the key was made of. "ip" is the client address
	// under the trusted-proxy policy; "account" is the login account name, which
	// is NOT put in a label (it is unbounded and identifies a person) — the
	// dimension only says that such a key exists.
	DimensionIP      = "ip"
	DimensionAccount = "account"

	// Webhook results.
	//
	// The four values are exactly the four outcomes the endpoint can produce, and
	// they are worth separating: `invalid_signature` means the sender is not
	// LiveKit (or the secret is wrong), while `rejected` means LiveKit called us
	// correctly and we failed to record it. One counter for both would hide which
	// half of the webhook path is broken.
	WebhookResultApplied          = "applied"
	WebhookResultIgnored          = "ignored"
	WebhookResultRejected         = "rejected"
	WebhookResultInvalidSignature = "invalid_signature"
	// WebhookEventUnverified is the `event` label used when the body was not
	// verified. The event name is attacker-controlled in that case, so using it
	// would let a scanner with one request per event name grow this metric
	// without bound — the exact cardinality bomb the label rules forbid.
	WebhookEventUnverified = "unknown"

	// WebSocket message directions, from the server's point of view.
	DirectionIn  = "in"
	DirectionOut = "out"

	// WSRole* mirror the two entry points that have a socket (§5). The role is the
	// connection's identity, not the user's: an admin socket does not exist.
	WSRoleStudent = "student"
	WSRoleTeacher = "teacher"

	// DBPoolState* are the three pgxpool.Stat() gauges worth watching. A pool that
	// is permanently at `total` with a queue is the shape of "the database is the
	// bottleneck"; `acquired` alone cannot distinguish that from a healthy burst.
	DBPoolStateAcquired = "acquired"
	DBPoolStateIdle     = "idle"
	DBPoolStateTotal    = "total"
)

// Media operations, one per method of the media plane the control plane calls.
// They are constants so the label set stays a closed list (see docs/architecture/observability.md).
const (
	MediaOperationCreateRoom          = "create_room"
	MediaOperationEnsureRoom          = "ensure_room"
	MediaOperationTerminateRoom       = "terminate_room"
	MediaOperationObserveRoom         = "observe_room"
	MediaOperationRemoveParticipant   = "remove_participant"
	MediaOperationUpdateSubscriptions = "update_subscriptions"
	MediaOperationUpdatePrivateTalk   = "update_private_talk"
	MediaOperationSignToken           = "sign_token"
	MediaOperationHealthCheck         = "health_check"
)

// Media results. `ok` includes every tolerated outcome (removing a participant
// who already left): the metric answers "did the control plane have to give up?",
// not "did the RPC do any work?".
const (
	MediaResultOK    = "ok"
	MediaResultError = "error"
)

// httpDurationBuckets are the HTTP latency buckets of §77: 5ms to 10s.
//
// WHY these bounds: the interesting questions for this API are "is a normal
// control-plane call still single-digit milliseconds?" (5ms/10ms/25ms), "is a
// monitoring poll slow enough to make the teacher's wall feel stale?"
// (100ms-500ms) and "is anything hitting a multi-second timeout?" (1s-10s). The
// +Inf bucket is implicit and catches everything slower than 10s, which is where
// a /readyz probe timeout or a database stall lands.
var httpDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// mediaDurationBuckets are the media-plane RPC buckets.
//
// They are coarser and reach further than the HTTP ones on purpose: a LiveKit
// RoomService call is a network round trip to a different system, so a p95 under
// 100ms is healthy and a call in the seconds range is what a teacher experiences
// as "加入课堂 is stuck".
var mediaDurationBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// SessionStatuses is the closed set of §12's six session states. The gauge is
// initialised with all six at zero so a status that never occurs is visibly zero
// rather than absent, which is the difference between "no student is
// SCREEN_LOST" and "the scraper forgot this series".
var SessionStatuses = []string{"CONNECTING", "ONLINE", "SCREEN_LOST", "DISCONNECTED", "LEFT", "ROOM_CLOSED"}

// Metrics is the whole ClassWatch metric set bound to one registry.
//
// Every method is safe to call on a nil *Metrics. That is what lets the services
// take metrics as an optional collaborator without a nil check at every call
// site, and it keeps every Phase 1-10 test working unchanged (they construct a
// service with no metrics at all).
type Metrics struct {
	registry *Registry

	// HTTPRequestDuration is labelled by method and route TEMPLATE (see HTTPRoute).
	HTTPRequestDuration *HistogramVec
	// HTTPRequestsInFlight is the concurrency of the process: it is what turns
	// "everything got slow" into "we were serving N requests at once".
	HTTPRequestsInFlight           *Gauge
	RateLimitRejectedTotal         *CounterVec
	WSConnections                  *GaugeVec
	WSMessagesTotal                *CounterVec
	WSSlowConsumerDisconnectsTotal *Counter
	SessionStatus                  *GaugeVec
	WebhookEventsTotal             *CounterVec
	MediaCallsTotal                *CounterVec
	MediaCallDuration              *HistogramVec
	PrivateTalkActive              *Gauge
	DBPoolConnections              *GaugeVec

	// routes caches one instrument bundle per (method, route template). The HTTP
	// middleware holds its own copy of this cache so that a request never takes
	// the lock below (see HTTPRoute).
	mu     sync.Mutex
	routes map[httpRouteKey]*RouteInstruments

	buildInfo *GaugeVec
}

// httpRouteKey identifies one Gin route template.
//
// WHY the METHOD is part of the key and the real path is not: the label must be
// the template (`/api/v1/classrooms/:id`) because the real path
// (`/api/v1/classrooms/8f14e45f-.../students`) has one value per classroom and
// would create unboundedly many series — a "cardinality bomb" that takes down the
// scraping Prometheus, not the API. Gin exposes the template via c.FullPath()
// after routing, which is exactly why this middleware runs at the router level.
type httpRouteKey struct {
	method string
	route  string
}

// RouteInstruments is the per-route instrument bundle returned by HTTPRoute.
//
// One instance is created per (method, route template) and reused by every
// request of that route: recording a sample is therefore two atomic adds and no
// map lookup, which is what keeps the metrics middleware off the allocation path.
type RouteInstruments struct {
	method string
	route  string
	// duration is resolved once, here, so no request re-resolves a labelled series.
	duration *Histogram
	// statuses is indexed by HTTP status code.
	//
	// WHY an array instead of a labelled counter per code: the status is only known
	// AFTER the handler runs, so a per-code handle cannot be cached per route. A
	// fixed-size array indexed by the code keeps the recording path to one atomic
	// add with no allocation, and the codes this API can produce are a closed,
	// bounded set (3 digits, RFC 9110).
	statuses [600]atomic.Uint64
}

// Observe records one finished request: its status and its duration.
func (m *RouteInstruments) Observe(status int, seconds float64) {
	if m == nil {
		return
	}
	if status >= 100 && status < len(m.statuses) {
		m.statuses[status].Add(1)
	}
	m.duration.Observe(seconds)
}

// HTTPRoute returns the instrument bundle of one (method, route template) pair,
// creating it on first use.
//
// The caller is expected to cache the result (the middleware does): calling this
// per request would take the registry lock on the hot path.
func (m *Metrics) HTTPRoute(method, route string) *RouteInstruments {
	if m == nil {
		return nil
	}
	key := httpRouteKey{method: method, route: route}
	m.mu.Lock()
	defer m.mu.Unlock()
	if meter, ok := m.routes[key]; ok {
		return meter
	}
	meter := &RouteInstruments{method: method, route: route, duration: m.HTTPRequestDuration.With(method, route)}
	m.routes[key] = meter
	return meter
}

// New builds the metric set on a fresh registry.
//
// The gauges whose label sets are closed are pre-created at zero: a gauge that
// appears only after its first event is indistinguishable from a broken exporter,
// and "no websocket is connected" is a fact this service should be able to state.
func New() *Metrics {
	registry := NewRegistry()
	m := &Metrics{
		registry: registry,
		HTTPRequestDuration: registry.NewHistogramVec(
			NameHTTPRequestDurationSeconds,
			"HTTP request latency in seconds, measured after the handler and before the response is flushed.",
			httpDurationBuckets, "method", "route"),
		HTTPRequestsInFlight: registry.NewGauge(
			NameHTTPRequestsInFlight,
			"HTTP requests currently being served by this process (excludes /metrics itself)."),
		RateLimitRejectedTotal: registry.NewCounterVec(
			NameRateLimitRejectedTotal,
			"Requests rejected by a rate limit, by policy scope and by what the key was made of.",
			"scope", "dimension"),
		WSConnections: registry.NewGaugeVec(
			NameWSConnections,
			"Live business WebSocket connections registered in this process, by entry-point role.",
			"role"),
		WSMessagesTotal: registry.NewCounterVec(
			NameWSMessagesTotal,
			"Business WebSocket messages, by direction from the server and by message type.",
			"direction", "type"),
		WSSlowConsumerDisconnectsTotal: registry.NewCounter(
			NameWSSlowConsumerDisconnectsTotal,
			"WebSocket connections dropped because their send queue was full (the client stopped reading)."),
		SessionStatus: registry.NewGaugeVec(
			NameSessionStatus,
			"Student sessions by status (six states of the session state machine), sampled periodically from PostgreSQL.",
			"status"),
		WebhookEventsTotal: registry.NewCounterVec(
			NameWebhookEventsTotal,
			"LiveKit webhook deliveries by event type and outcome. Deliveries whose signature did not verify are counted under event=\"unknown\" because their body is untrusted.",
			"event", "result"),
		MediaCallsTotal: registry.NewCounterVec(
			NameMediaCallsTotal,
			"Media-plane (LiveKit RoomService) calls by operation and outcome.",
			"operation", "result"),
		MediaCallDuration: registry.NewHistogramVec(
			NameMediaCallDurationSeconds,
			"Media-plane (LiveKit RoomService) call latency in seconds, by operation.",
			mediaDurationBuckets, "operation"),
		PrivateTalkActive: registry.NewGauge(
			NamePrivateTalkActive,
			"Private talks currently active in this process (one teacher microphone to one student)."),
		DBPoolConnections: registry.NewGaugeVec(
			NameDBPoolConnections,
			"PostgreSQL pool connections from pgxpool.Stat(), by state.",
			"state"),
		buildInfo: registry.NewGaugeVec(
			NameBuildInfo,
			"Build identity as a constant 1 per version/commit, for correlating a behaviour change with a deploy.",
			"version", "commit"),
		routes: make(map[httpRouteKey]*RouteInstruments),
	}

	// Pre-create the closed label sets so the metrics exist (at zero) before the
	// first event. See the doc comment above.
	for _, role := range []string{WSRoleStudent, WSRoleTeacher} {
		m.WSConnections.With(role).Set(0)
	}
	for _, status := range SessionStatuses {
		m.SessionStatus.With(status).Set(0)
	}
	for _, state := range []string{DBPoolStateAcquired, DBPoolStateIdle, DBPoolStateTotal} {
		m.DBPoolConnections.With(state).Set(0)
	}
	return m
}

// SetBuildInfo records the version and commit this process is running.
func (m *Metrics) SetBuildInfo(version, commit string) {
	if m == nil {
		return
	}
	m.buildInfo.With(sanitizeLabelValue(version), sanitizeLabelValue(commit)).Set(1)
}

// sanitizeLabelValue keeps a label value to a bounded, low-cardinality alphabet.
//
// version/commit come from build flags and are effectively constant per binary,
// but they are still external text; bounding them keeps a build that was stamped
// with something exotic from producing an unparseable scrape.
func sanitizeLabelValue(value string) string {
	const maxLen = 64
	if value == "" {
		return "unknown"
	}
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '.' || r == '-' || r == '_' || r == '+':
			out = append(out, r)
		default:
			// Dropped: a value that needs escaping is not something a build
			// identity should contain.
		}
		if len(out) >= maxLen {
			break
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}

// Handler returns the /metrics endpoint.
//
// # Why this endpoint has no session authentication
//
// It exposes aggregate counters only: no user id, no session id, no classroom
// name, no account, no token, no request body. There is nothing here that a
// student's or teacher's session would protect, and requiring one would mean the
// scraper holds a human's credential — a worse trade than the exposure.
//
// It is still NOT meant to be on the public internet: the numbers reveal load,
// version and error rates. A production deployment puts it on the internal
// network, on a separate port, or behind a Bearer token / IP ACL at the reverse
// proxy (see docs/architecture/observability.md, and the Caddy section owned by
// the deploy configuration).
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// A scrape is cheap, but a cache in front of it would serve a stale body
		// without varying on anything meaningful.
		w.Header().Set("Cache-Control", "no-store")
		if err := m.WriteExposition(w); err != nil {
			// The status line is already written at this point; the only honest
			// thing left is to make the failure visible to the scraper.
			_, _ = w.Write([]byte("# exposition failed: " + escapeHelp(err.Error()) + "\n"))
		}
	})
}

// WriteExposition renders every family in the Prometheus text format (version
// 0.0.4).
//
// Families are emitted in name order with all of their samples contiguous, which
// is what the format requires and what makes two scrapes diffable.
func (m *Metrics) WriteExposition(w http.ResponseWriter) error {
	blocks := m.registry.renderBlocks()
	blocks = append(blocks, block{name: NameHTTPRequestsTotal, text: m.renderHTTPRequests()})
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].name < blocks[j].name })
	for _, b := range blocks {
		if _, err := w.Write([]byte(b.text)); err != nil {
			return err
		}
	}
	return nil
}

// renderHTTPRequests renders the one family that is not a plain labelled counter.
//
// It is bespoke because the `status` label is only known after the handler ran, so
// the per-route handles cannot include it (see routeMeter.statuses). Rendering
// walks the cached routes and emits the statuses that actually occurred.
func (m *Metrics) renderHTTPRequests() string {
	m.mu.Lock()
	meters := make([]*RouteInstruments, 0, len(m.routes))
	for _, meter := range m.routes {
		meters = append(meters, meter)
	}
	m.mu.Unlock()

	sort.Slice(meters, func(i, j int) bool {
		if meters[i].route != meters[j].route {
			return meters[i].route < meters[j].route
		}
		return meters[i].method < meters[j].method
	})

	var b []byte
	b = append(b, "# HELP "+NameHTTPRequestsTotal+" HTTP requests served, by method, route template and status code. The route label is the Gin template, never the real path, to keep the series count bounded.\n"...)
	b = append(b, "# TYPE "+NameHTTPRequestsTotal+" counter\n"...)
	for _, meter := range meters {
		for status := 100; status < len(meter.statuses); status++ {
			total := meter.statuses[status].Load()
			if total == 0 {
				continue
			}
			b = append(b, NameHTTPRequestsTotal...)
			b = append(b, `{method="`...)
			b = append(b, escapeLabelValue(meter.method)...)
			b = append(b, `",route="`...)
			b = append(b, escapeLabelValue(meter.route)...)
			b = append(b, `",status="`...)
			b = append(b, strconv.Itoa(status)...)
			b = append(b, `"} `...)
			b = append(b, strconv.FormatUint(total, 10)...)
			b = append(b, '\n')
		}
	}
	return string(b)
}

// AddWSConnection moves the live-connection gauge of one role by delta.
func (m *Metrics) AddWSConnection(role string, delta int64) {
	if m == nil {
		return
	}
	m.WSConnections.With(role).Add(delta)
}

// IncWSMessage counts one business message in one direction.
func (m *Metrics) IncWSMessage(direction, messageType string) {
	if m == nil {
		return
	}
	if messageType == "" {
		messageType = "unknown"
	}
	m.WSMessagesTotal.With(direction, messageType).Inc()
}

// IncWSSlowConsumerDisconnect counts one connection dropped for not reading.
func (m *Metrics) IncWSSlowConsumerDisconnect() {
	if m == nil {
		return
	}
	m.WSSlowConsumerDisconnectsTotal.Inc()
}

// IncRateLimitRejected counts one refused request.
//
// The account name is deliberately NOT a label: it is unbounded and it identifies
// a person. `dimension` only says which kind of key the limit used.
func (m *Metrics) IncRateLimitRejected(scope, dimension string) {
	if m == nil {
		return
	}
	m.RateLimitRejectedTotal.With(scope, dimension).Inc()
}

// IncWebhookEvent counts one webhook delivery. event must be the verified event
// name, or WebhookEventUnverified when the signature did not check out.
func (m *Metrics) IncWebhookEvent(event, result string) {
	if m == nil {
		return
	}
	if event == "" {
		event = WebhookEventUnverified
	}
	m.WebhookEventsTotal.With(event, result).Inc()
}

// ObserveMediaCall counts one media-plane call and its latency.
func (m *Metrics) ObserveMediaCall(operation, result string, seconds float64) {
	if m == nil {
		return
	}
	m.MediaCallsTotal.With(operation, result).Inc()
	m.MediaCallDuration.With(operation).Observe(seconds)
}

// SetSessionStatus replaces the sampled session census.
//
// The caller must set ALL six statuses on every refresh: setting only the
// non-zero ones would leave a status stuck at its last non-zero value forever
// (the classic "gauge that only goes up" bug).
func (m *Metrics) SetSessionStatus(counts map[string]int64) {
	if m == nil {
		return
	}
	for _, status := range SessionStatuses {
		m.SessionStatus.With(status).Set(counts[status])
	}
}

// SetPrivateTalkActive replaces the active private-talk count of this process.
func (m *Metrics) SetPrivateTalkActive(count int64) {
	if m == nil {
		return
	}
	m.PrivateTalkActive.Set(count)
}

// SetDBPoolConnections replaces the pool census from pgxpool.Stat().
func (m *Metrics) SetDBPoolConnections(acquired, idle, total int32) {
	if m == nil {
		return
	}
	m.DBPoolConnections.With(DBPoolStateAcquired).Set(int64(acquired))
	m.DBPoolConnections.With(DBPoolStateIdle).Set(int64(idle))
	m.DBPoolConnections.With(DBPoolStateTotal).Set(int64(total))
}
