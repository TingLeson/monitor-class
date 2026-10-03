package httpapi

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/classwatch/classwatch/services/api/internal/metrics"
)

// This file is the HTTP half of the metrics instrumentation of §59/§77.
//
// Every other metric in the set is recorded where the event happens (the hub for
// sockets, the media client for LiveKit calls, the scheduler for the session
// census). HTTP is different: the event is "one request finished", which only the
// middleware chain can see, and it must be recorded for EVERY response — including
// the ones with no handler (404/405), the ones a middleware produced (413, 429,
// 503) and the ones that came from a panic.

// routeUnmatched is the `route` label for a request that matched no route
// template.
//
// WHY a constant instead of the real path: a 404 for a scanned URL
// (/wp-login.php, /admin.php, ...) is attacker-chosen, so using the path as a
// label would create one time series per probe and take down the scraping
// Prometheus — the cardinality bomb. The label answers "how much of our traffic is
// going nowhere?", and the access log (which keeps the real path) answers "what did
// they probe for?".
const routeUnmatched = "unmatched"

// routeCache caches one instrument bundle per (method, route template).
//
// It is the reason the metrics middleware does not allocate or lock on the hot
// path: a Gin route template is a fixed string per route, the lookup key is a
// comparable struct (no boxing, no concatenation) and the value is a pointer the
// request path only reads.
type routeCache struct {
	mu    sync.RWMutex
	meter map[routeKey]*metrics.RouteInstruments
}

type routeKey struct {
	method string
	route  string
}

func newRouteCache() *routeCache {
	return &routeCache{meter: make(map[routeKey]*metrics.RouteInstruments)}
}

// get returns the instrument bundle for one route, resolving it at most once.
//
// A read lock is taken on the fast path and released immediately: after the first
// request of a route, this never writes. Contention is therefore one shared
// RWMutex read (an atomic add) per request — the same cost as the request id
// lookup next to it, and far below the cost of the JSON encoder that will run
// afterwards.
func (c *routeCache) get(m *metrics.Metrics, method, route string) *metrics.RouteInstruments {
	key := routeKey{method: method, route: route}
	c.mu.RLock()
	meter, ok := c.meter[key]
	c.mu.RUnlock()
	if ok {
		return meter
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if meter, ok := c.meter[key]; ok {
		return meter
	}
	meter = m.HTTPRoute(method, route)
	c.meter[key] = meter
	return meter
}

// MetricsMiddleware records request count, latency and in-flight concurrency.
//
// # Why it sits between the access log and the recovery middleware
//
// The order of the chain is what makes the numbers mean what they say:
//
//	RequestID -> AccessLog -> Metrics -> Recovery -> ...
//
// Recovery must be INSIDE metrics, so a panic is recovered, answered as a 500 and
// then observed as a 500. If metrics were inside recovery, the deferred recording
// would run while the panic is still unwinding, when the writer still claims 200 —
// and a crashing endpoint would look healthy on the dashboard.
//
// # Why /metrics is excluded
//
// A scrape is a request like any other, and counting it would make the counter
// grow at exactly the rate it is scraped: a self-referential series that is
// useless for capacity and actively misleading for "requests per second"
// dashboards. The in-flight gauge is excluded with it, so a slow scrape cannot
// look like application load.
func MetricsMiddleware(m *metrics.Metrics) gin.HandlerFunc {
	if m == nil {
		// No metrics configured (tests, a probe-only deployment): install nothing
		// rather than a middleware that does work and discards it.
		return func(c *gin.Context) { c.Next() }
	}
	cache := newRouteCache()

	return func(c *gin.Context) {
		if c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}

		// The gauge is released with a defer, not at the end of the function: the
		// recovery middleware re-panics on http.ErrAbortHandler (a client that went
		// away mid-response), and an unreleased in-flight slot would make the process
		// look permanently busy after one client hang-up.
		m.HTTPRequestsInFlight.Inc()
		defer m.HTTPRequestsInFlight.Dec()

		start := time.Now()
		c.Next()

		// Recorded after c.Next() returns — i.e. once the status is final — and
		// deliberately NOT in a defer: a panic that reaches net/http (the abort case
		// above) produced no response to classify, and counting it as the 200 the
		// writer still claims would understate exactly the errors this metric exists
		// to show. Recovered panics DO land here, because Recovery is inside this
		// middleware.
		//
		// c.FullPath() is the route TEMPLATE Gin matched ("" when nothing matched) —
		// see routeUnmatched.
		route := c.FullPath()
		if route == "" {
			route = routeUnmatched
		}
		cache.get(m, normalizeMethod(c.Request.Method), route).
			Observe(c.Writer.Status(), time.Since(start).Seconds())
	}
}

// normalizeMethod bounds the `method` label.
//
// The route is a Gin template and therefore bounded, but a request to an UNMATCHED
// path still carries a client-chosen method token: `curl -X $(random 30 chars)`
// against 30 random paths would otherwise create 30 fresh time series, and a
// scanner with a loop creates them without bound. Known HTTP methods keep their
// name (they are what makes a dashboard readable); anything else collapses into
// `other`. This is the same anti-cardinality rule as routeUnmatched, applied to
// the other half of the label set.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace, http.MethodConnect:
		return method
	default:
		return "other"
	}
}
