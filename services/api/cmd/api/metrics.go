package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/classwatch/classwatch/services/api/internal/config"
	"github.com/classwatch/classwatch/services/api/internal/metrics"
	"github.com/classwatch/classwatch/services/api/internal/session"
)

// metricsSampler refreshes the gauges that describe STATE rather than events.
//
// # Why these cannot be counters
//
// Everything else in the metric set is recorded where it happens (a request, a
// websocket message, a media call). These four describe a population that changes
// without a request behind it — a student's session moves state from a WEBHOOK, a
// pool connection is acquired by a background query — so there is no call site to
// attach them to. Counting them with increment/decrement pairs would eventually
// drift (one missed path is enough) and a gauge that disagrees with the state it
// names is worse than no gauge: it sends an operator to look for a classroom that
// does not exist.
//
// # Why the interval is configuration
//
// 15 seconds is the default: fine enough that a dashboard sees a lesson start, coarse
// enough that the query is negligible (it is three cheap statements against indexed
// columns at most). It must stay well above the scrape interval, or each scrape would
// see a different value of a slowly-moving population and the graph would look noisy.
type metricsSampler struct {
	logger   *slog.Logger
	metrics  *metrics.Metrics
	pool     *pgxpool.Pool
	sessions *session.Service
	// repo is a second thin wrapper over the same pool, used only for the census
	// query. It is a separate value rather than a method on the service because the
	// census is an operational read, not a session use case: nothing that serves a
	// request should grow a method only the metrics loop calls.
	repo     *session.Postgres
	interval time.Duration
}

// run samples until ctx is cancelled, then returns.
//
// The first sample happens immediately: a process that has been up for ten seconds
// should report its state, not have its first /metrics scrape show zeros.
func (s *metricsSampler) run(ctx context.Context) {
	if s == nil || s.metrics == nil {
		return
	}
	s.sample(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sample(ctx)
		}
	}
}

// sample refreshes every derived gauge once.
//
// A nil sampler or a nil registry is a no-op, so a caller never has to guard: the
// sampler is optional by construction (a deployment may disable metrics).
func (s *metricsSampler) sample(ctx context.Context) {
	if s == nil || s.metrics == nil {
		return
	}
	// A bound on the whole sample: a hung database must not leave the metrics loop
	// blocked forever (it would then be one more goroutine a shutdown has to wait
	// for, and it would stop reporting pool pressure exactly when pool pressure is
	// the problem).
	sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if s.sessions != nil {
		s.metrics.SetPrivateTalkActive(int64(s.sessions.ActivePrivateTalkCount()))
	}
	if s.pool != nil {
		stat := s.pool.Stat()
		s.metrics.SetDBPoolConnections(stat.AcquiredConns(), stat.IdleConns(), stat.TotalConns())
	}
	if s.repo == nil {
		return
	}

	counts, err := s.repo.CountByStatus(sampleCtx)
	if err != nil {
		// A failed sample leaves the previous values in place rather than zeroing
		// them: zeroing would look exactly like "every student left", which is the
		// single most alarming thing this gauge can say.
		s.logger.Warn("session census sample failed; the gauge keeps its previous value",
			"action", "metrics_sample_failed", "error", err)
		return
	}
	byStatus := make(map[string]int64, len(counts))
	for status, count := range counts {
		byStatus[string(status)] = count
	}
	s.metrics.SetSessionStatus(byStatus)
}

// newMetricsSampler wires the sampler. It returns nil when metrics are disabled,
// and the caller must tolerate that (run is nil-safe).
func newMetricsSampler(
	logger *slog.Logger,
	cfg *config.Config,
	m *metrics.Metrics,
	pool *pgxpool.Pool,
	sessions *session.Service,
) *metricsSampler {
	if m == nil {
		return nil
	}
	sampler := &metricsSampler{
		logger:   logger,
		metrics:  m,
		pool:     pool,
		sessions: sessions,
		interval: cfg.MetricsSessionRefreshInterval,
	}
	if pool != nil {
		sampler.repo = session.NewPostgres(pool)
	}
	return sampler
}
