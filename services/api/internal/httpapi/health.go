package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Probe timeouts. Each dependency gets probeTimeout, and the whole response is
// bounded by readinessBodyLimit: three checks run concurrently, so a hung
// dependency cannot make the probe itself hang and cascade into an orchestrator
// killing an otherwise healthy process.
const (
	probeTimeout       = 2 * time.Second
	readinessBodyLimit = 4 * time.Second
)

// Probe status strings. They are part of the contract orchestrators and humans
// read, so they stay stable and short.
const (
	statusOK       = "ok"
	statusDegraded = "degraded"
	statusReady    = "ready"
)

// Pinger is the minimum a dependency must satisfy to be probed: PostgreSQL and
// Redis both implement it, and a test double needs three lines.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RoomLister is the media-plane probe contract. *media.Client implements it; a
// fake implements it without a LiveKit server.
type RoomLister interface {
	HealthCheck(ctx context.Context) error
}

// ReadinessDeps is the set of dependencies /readyz reports on.
//
// Dependencies are injected rather than reached for globally so the probe is
// unit-testable without PostgreSQL, Redis or LiveKit, and so a nil dependency has
// exactly one meaning: "failed to connect at boot". That is reported as degraded
// rather than crashing a process which, in relaxed mode, is deliberately running
// anyway.
type ReadinessDeps struct {
	Postgres Pinger
	Redis    Pinger
	LiveKit  RoomLister
}

// readyzResponse is the documented /readyz body: one entry per dependency, so an
// operator sees which one is failing without reading logs.
type readyzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// healthzHandler answers liveness: "is this process running and able to serve?".
//
// It deliberately checks NOTHING. A liveness probe that failed when the database
// blipped would make the orchestrator kill and restart a perfectly healthy
// process, turning a dependency hiccup into an outage. Dependency health belongs
// to /readyz, where the only consequence is "stop routing traffic here".
func healthzHandler(c *gin.Context) {
	RespondJSON(c, http.StatusOK, gin.H{"status": statusOK})
}

// readyzHandler answers readiness: "should traffic be routed here right now?".
//
// SECURITY: this endpoint is unauthenticated and its body is stored by monitoring
// systems. It must never contain a connection string, a password, an API secret
// or a raw driver error — only a short classification. Full errors go to the log,
// correlated by the same request id the response reports.
func readyzHandler(deps ReadinessDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), readinessBodyLimit)
		defer cancel()

		probes := map[string]func(context.Context) string{
			"postgres": pingProbe(deps.Postgres),
			"redis":    pingProbe(deps.Redis),
			"livekit":  roomProbe(deps.LiveKit),
		}

		results := make(map[string]string, len(probes))
		var mu sync.Mutex
		var wg sync.WaitGroup

		for name, probe := range probes {
			wg.Add(1)
			go func(name string, probe func(context.Context) string) {
				defer wg.Done()
				outcome := probe(ctx)
				mu.Lock()
				results[name] = outcome
				mu.Unlock()
			}(name, probe)
		}
		wg.Wait()

		ready := true
		for _, value := range results {
			if value != statusOK {
				ready = false
				break
			}
		}

		body := readyzResponse{Status: statusReady, Checks: results}
		code := http.StatusOK
		if !ready {
			body.Status = statusDegraded
			// 503 is the signal that makes a load balancer stop routing here.
			code = http.StatusServiceUnavailable
			LoggerFrom(c).Warn("readiness degraded", "checks", results)
		}
		RespondJSON(c, code, body)
	}
}

// pingProbe adapts a Pinger into a probe returning a safe classification.
func pingProbe(p Pinger) func(context.Context) string {
	return func(ctx context.Context) string {
		if p == nil {
			// Reporting "not configured" rather than "unreachable" is the more
			// actionable truth: nothing was ever contacted.
			return "error: not configured"
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			// The cause is logged here, where it stays server-side; the response
			// only carries the category.
			return "error: " + classify(err)
		}
		return statusOK
	}
}

// roomProbe adapts the media client into a probe. Listing rooms is the cheapest
// call that proves the LiveKit server is reachable AND that our credentials are
// accepted — a plain TCP dial would prove neither.
func roomProbe(r RoomLister) func(context.Context) string {
	return func(ctx context.Context) string {
		if r == nil {
			return "error: not configured"
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		if err := r.HealthCheck(ctx); err != nil {
			return "error: " + classify(err)
		}
		return statusOK
	}
}

// classify reduces any dependency error to a short, non-sensitive category.
//
// WHY a fixed vocabulary instead of err.Error(): driver and gRPC errors quote host
// names, DSNs, room names and sometimes credentials. These categories are the only
// thing a probe response may ever say, and they are enough to route the incident:
// "timeout" → network or capacity, "unauthenticated" → wrong key,
// "unreachable" → is the container even up?
func classify(err error) string {
	if err == nil {
		return statusOK
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	// LiveKit speaks gRPC/twirp. status.FromError unwraps, so the classification
	// survives the fmt.Errorf("%w") wrappers used by the media and infra packages.
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.OK, codes.Unknown:
			// Not a gRPC status: keep going with the network/text checks below.
		case codes.DeadlineExceeded, codes.Canceled:
			return "timeout"
		case codes.Unavailable:
			return "unreachable"
		case codes.Unauthenticated, codes.PermissionDenied:
			return "unauthenticated"
		case codes.NotFound, codes.Unimplemented:
			return "incompatible"
		default:
			return "error"
		}
	}

	if errors.As(err, &netErr) {
		return "unreachable"
	}

	// pgx and go-redis also surface plain dial errors. Matching text is a last
	// resort, and it stays safe because only the category is ever returned.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "connection reset"):
		return "unreachable"
	case strings.Contains(msg, "password authentication failed"),
		strings.Contains(msg, "WRONGPASS"):
		return "unauthenticated"
	default:
		return "error"
	}
}
