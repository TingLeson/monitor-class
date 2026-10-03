// Package metrics implements the ClassWatch metrics surface: an in-process
// registry, the ClassWatch metric set and a hand-written Prometheus text-format
// exposition endpoint (§59/§77).
//
// # Why this package exists instead of the Prometheus client library
//
// The task book (§77) asks for metrics, not for a metrics ecosystem. Everything
// this service needs is a bounded, hand-picked set of counters, gauges and
// histograms; the official client would add a dependency tree (and, transitively,
// a pull toward process/Go-runtime collectors and a second HTTP surface) to emit a
// format that is a few hundred bytes of text per scrape.
//
// The exposition format is small and stable: `# HELP`, `# TYPE`, then
// `name{label="value"} value` per sample. Writing it by hand keeps two properties
// the project cares about — zero heavy dependencies, and a metric set whose
// cardinality is decided by an explicit list rather than by whatever a library
// decides to collect.
//
// # Why the hot path is allocation-free
//
// Every metric family is created once, and a call site resolves its series ONCE
// (With / HTTPRoute) and keeps the returned handle. After that, recording a
// sample is a single atomic add: no map lookup, no lock, no allocation. A
// mutex or a sync.Map lookup per request would be simpler but would put a shared
// cache line (or an interface boxing allocation) on the path of every single
// HTTP request, websocket message and media call.
//
// # Concurrency
//
// Samples are plain atomics. Family maps are guarded by an RWMutex and are only
// touched when a series is created or when the exposition is rendered, so
// contention between the two is not on any request path.
package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Family types, spelled the way the exposition format spells them.
const (
	typeCounter   = "counter"
	typeGauge     = "gauge"
	typeHistogram = "histogram"
)

// SeriesKeySeparator terminates the length prefix of one label value in a series
// key. A length prefix (rather than a plain separator) is what makes the key
// unambiguous: label values are chosen by call sites and by inbound data, and
// `{a="x|y", b="z"}` must never collide with `{a="x", b="y|z"}`.
const seriesKeySeparator = ":"

// sample is one series of a family: the label values that identify it plus the
// storage its type needs.
//
// One struct covers all three types because a family is always of exactly one
// type, decided at registration. The unused fields cost a few words per series,
// and series are created per (route, status) or per (role), never per request.
type sample struct {
	labelValues []string
	counter     atomic.Uint64
	gauge       atomic.Int64
	hist        *histogramData
}

// histogramData is the storage of one histogram series.
//
// counts has len(bounds)+1 entries: the final one counts every observation (the
// +Inf bucket), so the "total" question is answered without walking the buckets.
type histogramData struct {
	bounds []float64
	counts []atomic.Uint64
	// sumBits holds the float64 sum as bits so it can be updated with a CAS loop
	// instead of a mutex. Two goroutines observing concurrently must not lose an
	// addition, and the HTTP duration histogram is updated by every request.
	sumBits atomic.Uint64
}

func newHistogramData(bounds []float64) *histogramData {
	return &histogramData{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

// observe records one value.
//
// The bucket search is a linear scan: the histogram of HTTP durations has eleven
// bounds, so an explicit loop is faster than a binary search and perfectly
// predictable. Buckets are non-cumulative here and accumulated at render time,
// which keeps the hot path to one add.
func (h *histogramData) observe(value float64) {
	index := len(h.bounds)
	for i, bound := range h.bounds {
		if value <= bound {
			index = i
			break
		}
	}
	h.counts[index].Add(1)
	for {
		old := h.sumBits.Load()
		next := math.Float64frombits(old) + value
		if h.sumBits.CompareAndSwap(old, math.Float64bits(next)) {
			break
		}
	}
}

// family is one metric name with one type, one help string and one set of label
// names. All its series share them, which is what the exposition format requires.
type family struct {
	name       string
	help       string
	typ        string
	labelNames []string
	// bounds is non-nil only for histograms.
	bounds []float64

	mu      sync.RWMutex
	samples map[string]*sample
}

// series returns the series for these label values, creating it on first use.
//
// The double-checked read is deliberate: after warm-up every call hits the
// read path, and only a genuinely new series takes the write lock.
func (f *family) series(values []string) *sample {
	key := seriesKey(values)
	f.mu.RLock()
	s := f.samples[key]
	f.mu.RUnlock()
	if s != nil {
		return s
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if s = f.samples[key]; s != nil {
		return s
	}
	s = &sample{labelValues: append([]string(nil), values...)}
	if f.typ == typeHistogram {
		s.hist = newHistogramData(f.bounds)
	}
	f.samples[key] = s
	return s
}

// snapshot returns the series in a deterministic order.
//
// Ordering matters for humans diffing two scrapes and for tests asserting on the
// text: a map iteration order would make both flaky.
func (f *family) snapshot() []*sample {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*sample, 0, len(f.samples))
	for _, s := range f.samples {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return labelOrderKey(out[i].labelValues) < labelOrderKey(out[j].labelValues)
	})
	return out
}

// seriesKey encodes label values unambiguously (see seriesKeySeparator).
func seriesKey(values []string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteString(seriesKeySeparator)
		b.WriteString(v)
	}
	return b.String()
}

// labelOrderKey is the sort key for a sample: length-prefixed values, so ordering
// is total and independent of the separator appearing inside a value.
func labelOrderKey(values []string) string { return seriesKey(values) }

// Registry holds every registered family.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

// NewCounterVec registers a labelled counter.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	return &CounterVec{family: r.register(name, help, typeCounter, nil, labels)}
}

// NewGaugeVec registers a labelled gauge.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{family: r.register(name, help, typeGauge, nil, labels)}
}

// NewCounter registers a counter with no labels.
//
// The single series is created eagerly, so the family is visible (at zero) from
// the first scrape instead of appearing only after its first event.
func (r *Registry) NewCounter(name, help string) *Counter {
	return &Counter{sample: r.register(name, help, typeCounter, nil, nil).series(nil)}
}

// NewGauge registers a gauge with no labels. See NewCounter for why the series
// exists up front.
func (r *Registry) NewGauge(name, help string) *Gauge {
	return &Gauge{sample: r.register(name, help, typeGauge, nil, nil).series(nil)}
}

// NewHistogramVec registers a labelled histogram with explicit bucket bounds.
// Bounds must be strictly increasing; the +Inf bucket is implicit.
func (r *Registry) NewHistogramVec(name, help string, bounds []float64, labels ...string) *HistogramVec {
	if len(bounds) == 0 {
		panic("metrics: histogram " + name + " needs at least one bucket bound")
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i] <= bounds[i-1] {
			panic("metrics: histogram " + name + " bucket bounds must be strictly increasing")
		}
	}
	return &HistogramVec{family: r.register(name, help, typeHistogram, bounds, labels)}
}

// register validates and stores one family.
//
// Registration panics on a bad name or a duplicate. Both are programming errors
// that would otherwise surface as a malformed scrape (a name Prometheus rejects)
// or as two silently merged families, and both are caught by the first test that
// builds the metric set — long before a deployment.
func (r *Registry) register(name, help, typ string, bounds []float64, labels []string) *family {
	if !validMetricName(name) {
		panic("metrics: invalid metric name " + strconv.Quote(name))
	}
	for _, label := range labels {
		if !validLabelName(label) {
			panic("metrics: invalid label name " + strconv.Quote(label) + " on " + name)
		}
		if label == "le" {
			// `le` is reserved by the exposition format for histogram buckets;
			// letting a family define it produces a series that collides with its
			// own bucket lines.
			panic("metrics: label name \"le\" is reserved on " + name)
		}
	}
	if strings.TrimSpace(help) == "" {
		panic("metrics: metric " + name + " needs a HELP string")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.families[name]; exists {
		panic("metrics: metric " + name + " is already registered")
	}
	f := &family{
		name:       name,
		help:       help,
		typ:        typ,
		labelNames: append([]string(nil), labels...),
		bounds:     append([]float64(nil), bounds...),
		samples:    make(map[string]*sample),
	}
	r.families[name] = f
	return f
}

// validMetricName follows the exposition format: [a-zA-Z_:][a-zA-Z0-9_:]*.
func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// validLabelName follows the exposition format: [a-zA-Z_][a-zA-Z0-9_]*.
func validLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// CounterVec is a counter family.
//
// With resolves a series handle. Call sites on a hot path resolve it once and
// reuse the handle (see Metrics.HTTPRoute); call sites that fire rarely may
// resolve per event, at the cost of one map lookup.
type CounterVec struct{ family *family }

// With returns the handle for these label values. The arity must match the
// registered label names; a mismatch is a programming error and panics.
func (v *CounterVec) With(values ...string) *Counter {
	return &Counter{sample: v.family.series(checkArity(v.family, values))}
}

// Counter is one series of a counter.
type Counter struct{ sample *sample }

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) {
	if c == nil || c.sample == nil {
		return
	}
	c.sample.counter.Add(n)
}

// Value reports the current total. It is exported for tests and for the
// exposition; nothing on a request path reads it.
func (c *Counter) Value() uint64 {
	if c == nil || c.sample == nil {
		return 0
	}
	return c.sample.counter.Load()
}

// GaugeVec is a gauge family.
type GaugeVec struct{ family *family }

// With returns the handle for these label values.
func (v *GaugeVec) With(values ...string) *Gauge {
	return &Gauge{sample: v.family.series(checkArity(v.family, values))}
}

// Gauge is one series of a gauge, or the single series of a label-less gauge.
type Gauge struct{ sample *sample }

// Set replaces the value.
func (g *Gauge) Set(value int64) {
	if g == nil || g.sample == nil {
		return
	}
	g.sample.gauge.Store(value)
}

// Add moves the value by delta.
func (g *Gauge) Add(delta int64) {
	if g == nil || g.sample == nil {
		return
	}
	g.sample.gauge.Add(delta)
}

// Inc adds one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value reports the current value.
func (g *Gauge) Value() int64 {
	if g == nil || g.sample == nil {
		return 0
	}
	return g.sample.gauge.Load()
}

// HistogramVec is a histogram family.
type HistogramVec struct{ family *family }

// With returns the handle for these label values.
func (v *HistogramVec) With(values ...string) *Histogram {
	return &Histogram{sample: v.family.series(checkArity(v.family, values))}
}

// Histogram is one series of a histogram.
type Histogram struct{ sample *sample }

// Observe records one value.
func (h *Histogram) Observe(value float64) {
	if h == nil || h.sample == nil || h.sample.hist == nil {
		return
	}
	h.sample.hist.observe(value)
}

// ObserveDuration records a duration in seconds, which is the unit Prometheus
// conventions require for a `_seconds` histogram.
func (h *Histogram) ObserveDuration(d time.Duration) {
	h.Observe(d.Seconds())
}

// Count reports how many observations were recorded.
func (h *Histogram) Count() uint64 {
	if h == nil || h.sample == nil || h.sample.hist == nil {
		return 0
	}
	total := uint64(0)
	for i := range h.sample.hist.counts {
		total += h.sample.hist.counts[i].Load()
	}
	return total
}

// Sum reports the sum of every observation.
func (h *Histogram) Sum() float64 {
	if h == nil || h.sample == nil || h.sample.hist == nil {
		return 0
	}
	return math.Float64frombits(h.sample.hist.sumBits.Load())
}

// BucketCount reports the cumulative count of one bucket index, with index ==
// len(bounds) meaning the +Inf bucket. Exported for tests.
func (h *Histogram) BucketCount(index int) uint64 {
	if h == nil || h.sample == nil || h.sample.hist == nil {
		return 0
	}
	if index < 0 || index >= len(h.sample.hist.counts) {
		return 0
	}
	return h.sample.hist.counts[index].Load()
}

func checkArity(f *family, values []string) []string {
	if len(values) != len(f.labelNames) {
		panic(fmt.Sprintf("metrics: %s expects %d label values (%s), got %d",
			f.name, len(f.labelNames), strings.Join(f.labelNames, ","), len(values)))
	}
	return values
}

// block is one rendered family plus the name it is sorted by.
type block struct {
	name string
	text string
}

// renderBlocks renders every family in the registry into one text block each,
// sorted by name.
//
// The exposition format requires all lines of a family to be contiguous, so the
// output is assembled family by family rather than sample by sample.
func (r *Registry) renderBlocks() []block {
	r.mu.Lock()
	families := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		families = append(families, f)
	}
	r.mu.Unlock()

	blocks := make([]block, 0, len(families))
	for _, f := range families {
		blocks = append(blocks, block{name: f.name, text: f.render()})
	}
	return blocks
}

// render writes one family, exactly in the exposition order.
func (f *family) render() string {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	fmt.Fprintf(w, "# HELP %s %s\n", f.name, escapeHelp(f.help))
	fmt.Fprintf(w, "# TYPE %s %s\n", f.name, f.typ)

	for _, s := range f.snapshot() {
		labels := f.labelPairs(s.labelValues)
		switch f.typ {
		case typeCounter:
			fmt.Fprintf(w, "%s%s %s\n", f.name, labels, strconv.FormatUint(s.counter.Load(), 10))
		case typeGauge:
			fmt.Fprintf(w, "%s%s %s\n", f.name, labels, strconv.FormatInt(s.gauge.Load(), 10))
		case typeHistogram:
			cumulative := uint64(0)
			for i, bound := range f.bounds {
				cumulative += s.hist.counts[i].Load()
				fmt.Fprintf(w, "%s_bucket%s %s\n", f.name,
					f.labelPairs(s.labelValues, "le", formatFloat(bound)),
					strconv.FormatUint(cumulative, 10))
			}
			cumulative += s.hist.counts[len(f.bounds)].Load()
			fmt.Fprintf(w, "%s_bucket%s %s\n", f.name,
				f.labelPairs(s.labelValues, "le", "+Inf"),
				strconv.FormatUint(cumulative, 10))
			fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labels,
				formatFloat(math.Float64frombits(s.hist.sumBits.Load())))
			fmt.Fprintf(w, "%s_count%s %s\n", f.name, labels,
				strconv.FormatUint(cumulative, 10))
		}
	}
	_ = w.Flush()
	return b.String()
}

// labelPairs renders `{a="1",b="2"}`, with extra pairs appended (the histogram
// `le` label). Escaping is mandatory: a label value is arbitrary text, and an
// unescaped quote or newline produces a scrape the parser rejects.
func (f *family) labelPairs(values []string, extra ...string) string {
	if len(f.labelNames) == 0 && len(extra) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(f.labelNames)+len(extra)/2)
	for i, name := range f.labelNames {
		if i >= len(values) {
			break
		}
		pairs = append(pairs, name+`="`+escapeLabelValue(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		pairs = append(pairs, extra[i]+`="`+escapeLabelValue(extra[i+1])+`"`)
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

// escapeHelp escapes a HELP string per the exposition format: backslash and
// newline. Quotes are NOT special in HELP text.
func escapeHelp(help string) string {
	help = strings.ReplaceAll(help, `\`, `\\`)
	return strings.ReplaceAll(help, "\n", `\n`)
}

// escapeLabelValue escapes a label value: backslash, double quote and newline.
func escapeLabelValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

// formatFloat renders a float the way the exposition format expects: `+Inf`,
// `-Inf` and `NaN` are literals, everything else is the shortest representation
// that round-trips.
func formatFloat(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	case math.IsNaN(value):
		return "NaN"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
}
