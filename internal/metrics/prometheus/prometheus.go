// Package prometheus provides a [metrics.Backend] implementation that
// produces Prometheus-compatible text exposition output — with no
// dependency on github.com/prometheus/client_golang. The native
// Prometheus text format (version 0.0.4) is serialised directly,
// keeping the entire metrics sub-system import-graph clean.
//
// # Wire-up
//
// Install the backend early in main before any blocking APIs are called:
//
//	import (
//	    "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
//	    "github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
//	)
//
//	reg := prometheus.New()
//	metrics.SetBackend(reg)
//
//	// Expose over HTTP:
//	http.Handle("/metrics", reg.Handler())
//
// # Concurrency
//
// [Registry] is safe for concurrent use. Counter increments and histogram
// observations are lock-free once the named series has been created: the hot
// path is a single sync.Map load keyed by the raw name. Only the first call for
// a given name sanitizes it and inserts into the canonical map (a sync.Map
// LoadOrStore); no mutex is held on any path.
//
// Lock-free is not the same as contention-free. A counter or histogram that
// several goroutines emit to concurrently would serialise them all on ONE cache
// line, which is a scaling defect rather than a correctness one; a series grows
// per-core accumulators the first time that contention is observed. See
// striped.go. Nothing about the exposed metric changes: the shards are summed
// on read, so names, labels, types and values are exactly what they were.
package prometheus

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// latencyBuckets are the upper-bound thresholds (in nanoseconds) for the
// standard latency histogram. The buckets cover the range from 100 µs to 5 s.
var latencyBuckets = [10]time.Duration{
	100 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	5 * time.Second,
}

// counter is a named monotonic counter.
//
// It starts as a single atomic and grows per-core shards the first time two
// goroutines are observed incrementing it concurrently. See striped.go for why
// the shards exist, how a shard is chosen, and why they are not allocated up
// front. The total is value plus every shard, so an increment is never lost
// whichever side of the promotion it lands on.
type counter struct {
	// value carries every increment applied before this counter was promoted,
	// plus any that raced the promotion. After promotion it stops growing but
	// remains part of the total.
	value atomic.Uint64
	// shards is nil until contention is observed; installed at most once.
	shards atomic.Pointer[counterShards]
}

// probe tests whether another goroutine is writing this counter concurrently
// and promotes it if so. It is the COLD half of the increment path: the hot
// half is written out in [Registry.IncCounter], because a method call there
// would not be inlined and would cost a real call on every increment. That is
// not a guess — it was measured. See the note on [counter].
//
// The swap is deliberately a NO-OP one, w for w: it changes nothing and cannot
// corrupt the count. Its only purpose is to FAIL, because CompareAndSwap fails
// exactly when another goroutine wrote between the load and the swap — which is
// the contention itself, observed rather than predicted. The technique is Doug
// Lea's, from java.util.concurrent.atomic.Striped64 (JSR-166), which promotes
// its per-thread cells on the same signal.
func (c *counter) probe() {
	w := c.value.Load()
	if !c.value.CompareAndSwap(w, w) {
		c.promote()
	}
}

// promote installs the shard array. It is idempotent and safe to call from any
// goroutine: the loser of the race drops its allocation.
func (c *counter) promote() {
	if c.shards.Load() != nil {
		return
	}
	c.shards.CompareAndSwap(nil, &counterShards{})
}

// load returns the counter's total.
func (c *counter) load() uint64 {
	total := c.value.Load()
	if s := c.shards.Load(); s != nil {
		for i := range s.slots {
			total += s.slots[i].n.Load()
		}
	}
	return total
}

// gauge holds the CURRENT value of a quantity that can fall as well as rise.
//
// The bits are stored in an atomic.Uint64 via math.Float64bits rather than
// behind a mutex: a gauge is written on the reclamation path and read by a
// scrape, and neither should wait on the other.
type gauge struct {
	value atomic.Uint64
}

// load returns the gauge's current value.
func (g *gauge) load() float64 { return math.Float64frombits(g.value.Load()) }

// histogram holds per-bucket counts plus a running sum (in nanoseconds)
// for Prometheus _sum exposition.
//
// An observation is THREE read-modify-writes — the sum, the total count and one
// bucket — and they sit in the same object, so a shared histogram costs three
// times a counter's cache-line traffic per observation. It is promoted to
// per-core shards on the same signal and by the same rule; see striped.go.
type histogram struct {
	// buckets[i] counts observations <= latencyBuckets[i].
	// Each is independent; the Prometheus _bucket{le=x} value is
	// computed as a cumulative sum at serialisation time.
	buckets [len(latencyBuckets)]atomic.Uint64
	// inf counts all observations regardless of magnitude.
	inf atomic.Uint64
	// sumNs accumulates raw nanosecond values; converted to seconds on
	// WriteText. Using int64 via atomic is safe because nanoseconds for
	// durations up to 5 s × 2^63 operations will not overflow in
	// practice, and time.Duration is int64-backed.
	sumNs atomic.Int64
	// shards is nil until contention is observed; installed at most once.
	shards atomic.Pointer[histShards]
}

// bucketOf returns the index of the bucket d falls in, or -1 when d exceeds
// every upper bound and belongs only to +Inf.
func bucketOf(d time.Duration) int {
	for i, upper := range latencyBuckets {
		if d <= upper {
			return i
		}
	}
	return -1
}

// observe records one latency sample.
func (h *histogram) observe(d time.Duration) {
	idx := bucketOf(d)
	if s := h.shards.Load(); s != nil {
		sh := &s.slots[shardIndex()]
		sh.sumNs.Add(int64(d))
		sh.inf.Add(1)
		if idx >= 0 {
			sh.buckets[idx].Add(1)
		}
		return
	}
	// Unpromoted: the three plain adds, plus the same sampled contention probe
	// the counter uses. inf is the field every observation touches, so it is the
	// one that carries the probe.
	h.sumNs.Add(int64(d))
	v := h.inf.Add(1)
	if idx >= 0 {
		h.buckets[idx].Add(1)
	}
	if v&probeMask == 0 {
		w := h.inf.Load()
		if !h.inf.CompareAndSwap(w, w) {
			h.promote()
		}
	}
}

// promote installs the shard array. Idempotent; the loser of a race drops its
// allocation.
func (h *histogram) promote() {
	if h.shards.Load() != nil {
		return
	}
	h.shards.CompareAndSwap(nil, &histShards{})
}

// snapshot returns the histogram's totals, summing the unpromoted state and
// every shard.
func (h *histogram) snapshot() (buckets [len(latencyBuckets)]uint64, inf uint64, sumNs int64) {
	for i := range latencyBuckets {
		buckets[i] = h.buckets[i].Load()
	}
	inf = h.inf.Load()
	sumNs = h.sumNs.Load()
	s := h.shards.Load()
	if s == nil {
		return buckets, inf, sumNs
	}
	for j := range s.slots {
		sh := &s.slots[j]
		for i := range latencyBuckets {
			buckets[i] += sh.buckets[i].Load()
		}
		inf += sh.inf.Load()
		sumNs += sh.sumNs.Load()
	}
	return buckets, inf, sumNs
}

// Registry is a metrics.Backend that formats observations as Prometheus
// text exposition (version 0.0.4). It requires no external dependencies.
//
// All methods are safe for concurrent use. Counter and histogram lookups
// are lock-free after the first observation for a given name.
type Registry struct {
	// counters is the canonical sanitized-name -> *counter map, ranged by
	// WriteText. counterByRaw caches raw-name -> *counter so the hot IncCounter
	// path, once a counter is established, is a single lock-free sync.Map load
	// with no per-call sanitize allocation or mutex (#1519). Metric names are a
	// bounded set of code constants, so counterByRaw cannot grow without bound.
	counters     sync.Map // string (sanitized) -> *counter
	counterByRaw sync.Map // string (raw)        -> *counter

	hists     sync.Map // string (sanitized) -> *histogram
	histByRaw sync.Map // string (raw)        -> *histogram

	gauges     sync.Map // string (sanitized) -> *gauge
	gaugeByRaw sync.Map // string (raw)        -> *gauge
}

// New creates a new Registry ready for use as a metrics.Backend.
func New() *Registry {
	// sync.Map zero values are ready for use; no field initialisation needed.
	return &Registry{}
}

// sanitize converts an arbitrary string into a valid Prometheus metric
// name. A valid name matches [a-zA-Z_:][a-zA-Z0-9_:]*: every character
// outside [a-zA-Z0-9_:] is replaced with '_', a leading digit is prefixed
// with '_', and an empty result becomes "_".
//
// Applying this at the IncCounter / ObserveLatency boundary means a name
// can never carry a newline, brace, quote, or space into the exposition
// output, so a hostile or buggy caller cannot inject forged series or
// break a scrape — even though Registry is now a public type alias whose
// methods accept caller-supplied names.
//
// This is stricter than the previous mapping (which replaced only
// {'.','-','/'}): any other out-of-charset byte — a leading digit, a
// non-ASCII rune — now also maps to '_'. The in-tree metric names are
// all ASCII dotted identifiers, so the rendered output is unchanged for
// them; callers must not rely on the old verbatim passthrough of other
// characters.
func sanitize(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 1)
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_') // a name may not start with a digit
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// getOrCreateCounter returns the counter for the raw (un-sanitized) name,
// creating it on first sight. The fast path — the counter already exists — is a
// single lock-free sync.Map load keyed by the raw name, with no per-call
// sanitize allocation. Only the first call for a given name sanitizes and
// consults the canonical sanitized-keyed map; two raw names that sanitize to
// the same metric name share one counter (LoadOrStore on the canonical key), so
// WriteText still emits one line per metric.
func (r *Registry) getOrCreateCounter(rawName string) *counter {
	if v, ok := r.counterByRaw.Load(rawName); ok {
		//nolint:forcetypeassert // counterByRaw is unexported and written only at prometheus.go:307, which stores the *counter obtained just above; counters, gauges and histograms occupy three separate map pairs (prometheus.go:237-244), so a name cannot collide across kinds
		return v.(*counter)
	}
	actual, _ := r.counters.LoadOrStore(sanitize(rawName), &counter{})
	//nolint:forcetypeassert // r.counters is written only by the LoadOrStore at prometheus.go:304, whose zero value is a &counter{}, so the map holds nothing but *counter
	c := actual.(*counter)
	r.counterByRaw.Store(rawName, c)
	return c
}

// getOrCreateGauge returns the gauge for the raw (un-sanitized) name, creating
// it on first sight; mirrors getOrCreateCounter.
func (r *Registry) getOrCreateGauge(rawName string) *gauge {
	if v, ok := r.gaugeByRaw.Load(rawName); ok {
		//nolint:forcetypeassert // gaugeByRaw is unexported and written only at prometheus.go:321, which stores the *gauge obtained just above; the three metric kinds occupy separate map pairs (prometheus.go:237-244)
		return v.(*gauge)
	}
	actual, _ := r.gauges.LoadOrStore(sanitize(rawName), &gauge{})
	//nolint:forcetypeassert // r.gauges is written only by the LoadOrStore at prometheus.go:318, whose zero value is a &gauge{}, so the map holds nothing but *gauge
	g := actual.(*gauge)
	r.gaugeByRaw.Store(rawName, g)
	return g
}

// getOrCreateHistogram returns the histogram for the raw (un-sanitized) name,
// creating it on first sight; mirrors getOrCreateCounter.
func (r *Registry) getOrCreateHistogram(rawName string) *histogram {
	if v, ok := r.histByRaw.Load(rawName); ok {
		//nolint:forcetypeassert // histByRaw is unexported and written only at prometheus.go:335, which stores the *histogram obtained just above; the three metric kinds occupy separate map pairs (prometheus.go:237-244)
		return v.(*histogram)
	}
	actual, _ := r.hists.LoadOrStore(sanitize(rawName), &histogram{})
	//nolint:forcetypeassert // r.hists is written only by the LoadOrStore at prometheus.go:332, whose zero value is a &histogram{}, so the map holds nothing but *histogram
	h := actual.(*histogram)
	r.histByRaw.Store(rawName, h)
	return h
}

// IncCounter implements metrics.Backend. It increments the named counter by
// delta. The name is sanitized before storage.
//
// The increment is written out here rather than delegated to a method on
// [counter] because the Go inliner will not inline a function this shape — it
// costs 144 against a budget of 80 — so a method would add a real call to every
// increment. It did: an earlier revision that delegated measured +21.2% on
// BenchmarkIncCounter (8.257ns -> 10.003ns, p=0.000, n=8) with no other change.
// IncCounter is itself never inlined either way, so putting the work here
// leaves the call count exactly what it was before the shards existed.
func (r *Registry) IncCounter(name string, delta uint64) {
	c := r.getOrCreateCounter(name)
	if s := c.shards.Load(); s != nil {
		s.slots[shardIndex()].n.Add(delta)
		return
	}
	// Unpromoted: the plain atomic add this counter has always done, plus a
	// contention probe on one increment in probeMask+1. See [counter.probe].
	if v := c.value.Add(delta); v&probeMask == 0 {
		c.probe()
	}
}

// SetGauge implements metrics.Backend. It records the current value of the
// named gauge. The name is sanitized before storage.
func (r *Registry) SetGauge(name string, v float64) {
	r.getOrCreateGauge(name).value.Store(math.Float64bits(v))
}

// ObserveLatency implements metrics.Backend. It records d in the latency
// histogram named name. The name is sanitized before storage.
func (r *Registry) ObserveLatency(name string, d time.Duration) {
	r.getOrCreateHistogram(name).observe(d)
}

// errWriter wraps an io.Writer and accumulates the first write error,
// short-circuiting all subsequent writes. This lets WriteText check
// errors once at the end rather than after every fmt.Fprintf call.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

// bucketLabel formats a duration upper-bound into the Prometheus le= label
// value using seconds as the canonical unit and %g as the shortest
// unambiguous decimal representation.
func bucketLabel(d time.Duration) string {
	return fmt.Sprintf("%g", d.Seconds())
}

// WriteText writes all collected metrics to w in Prometheus text exposition
// format (version 0.0.4).
//
// Metrics are emitted in two groups — counters first, then histograms —
// each sorted alphabetically by name so the output is deterministic.
// The first write error, if any, is returned; partial output may have been
// written before the error occurred.
func (r *Registry) WriteText(w io.Writer) error {
	ew := &errWriter{w: w}

	// Snapshot counters from the canonical sanitized-name map. sync.Map.Range
	// is safe for concurrent use; a counter created mid-range may or may not be
	// included, which is acceptable for a point-in-time metrics scrape.
	var cNames []string
	cSnap := make(map[string]uint64)
	r.counters.Range(func(k, v any) bool {
		//nolint:forcetypeassert // the only write to r.counters is the LoadOrStore at prometheus.go:304, whose key is the sanitize(rawName) string
		name := k.(string)
		cNames = append(cNames, name)
		//nolint:forcetypeassert // the only write to r.counters is the LoadOrStore at prometheus.go:304, whose value is a &counter{}
		cSnap[name] = v.(*counter).load()
		return true
	})
	sort.Strings(cNames)

	for _, name := range cNames {
		ew.printf("# TYPE %s counter\n", name)
		ew.printf("%s %d\n", name, cSnap[name])
	}

	// Gauges, between the counters and the histograms and sorted the same way,
	// so the exposition stays deterministic.
	var gNames []string
	gSnap := make(map[string]float64)
	r.gauges.Range(func(k, v any) bool {
		//nolint:forcetypeassert // the only write to r.gauges is the LoadOrStore at prometheus.go:318, whose key is the sanitize(rawName) string
		name := k.(string)
		gNames = append(gNames, name)
		//nolint:forcetypeassert // the only write to r.gauges is the LoadOrStore at prometheus.go:318, whose value is a &gauge{}
		gSnap[name] = v.(*gauge).load()
		return true
	})
	sort.Strings(gNames)
	for _, name := range gNames {
		ew.printf("# TYPE %s gauge\n", name)
		ew.printf("%s %g\n", name, gSnap[name])
	}

	// Snapshot histograms from the canonical sanitized-name map.
	var hNames []string
	type histSnap struct {
		buckets [len(latencyBuckets)]uint64
		inf     uint64
		sumNs   int64
	}
	hSnap := make(map[string]histSnap)
	r.hists.Range(func(k, v any) bool {
		//nolint:forcetypeassert // the only write to r.hists is the LoadOrStore at prometheus.go:332, whose key is the sanitize(rawName) string
		name := k.(string)
		//nolint:forcetypeassert // the only write to r.hists is the LoadOrStore at prometheus.go:332, whose value is a &histogram{}
		h := v.(*histogram)
		hNames = append(hNames, name)
		var snap histSnap
		snap.buckets, snap.inf, snap.sumNs = h.snapshot()
		hSnap[name] = snap
		return true
	})
	sort.Strings(hNames)

	for _, name := range hNames {
		snap := hSnap[name]
		ew.printf("# TYPE %s histogram\n", name)

		var cumulative uint64
		for i, upper := range latencyBuckets {
			cumulative += snap.buckets[i]
			ew.printf("%s_bucket{le=%q} %d\n", name, bucketLabel(upper), cumulative)
		}
		// +Inf bucket equals total observation count.
		ew.printf("%s_bucket{le=\"+Inf\"} %d\n", name, snap.inf)

		sumSec := float64(snap.sumNs) / float64(time.Second)
		ew.printf("%s_sum %g\n", name, sumSec)
		ew.printf("%s_count %d\n", name, snap.inf)
	}
	return ew.err
}

const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler returns an http.Handler that serves all collected metrics in
// Prometheus text exposition format on every GET request. The response
// carries Content-Type: text/plain; version=0.0.4; charset=utf-8.
//
// If writing the response body fails (e.g. a broken connection), the
// handler responds with HTTP 500 and the error message in the body.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if err := r.WriteText(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
