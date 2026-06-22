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
package prometheus

import (
	"fmt"
	"io"
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

// counter is a named monotonic counter backed by a single atomic.
type counter struct {
	value atomic.Uint64
}

// histogram holds per-bucket counts plus a running sum (in nanoseconds)
// for Prometheus _sum exposition.
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
}

// observe records one latency sample.
func (h *histogram) observe(d time.Duration) {
	ns := int64(d)
	h.sumNs.Add(ns)
	h.inf.Add(1)
	for i, upper := range latencyBuckets {
		if d <= upper {
			h.buckets[i].Add(1)
			return
		}
	}
	// d > 5 s: only the +Inf bucket (already incremented above).
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
		return v.(*counter)
	}
	actual, _ := r.counters.LoadOrStore(sanitize(rawName), &counter{})
	c := actual.(*counter)
	r.counterByRaw.Store(rawName, c)
	return c
}

// getOrCreateHistogram returns the histogram for the raw (un-sanitized) name,
// creating it on first sight; mirrors getOrCreateCounter.
func (r *Registry) getOrCreateHistogram(rawName string) *histogram {
	if v, ok := r.histByRaw.Load(rawName); ok {
		return v.(*histogram)
	}
	actual, _ := r.hists.LoadOrStore(sanitize(rawName), &histogram{})
	h := actual.(*histogram)
	r.histByRaw.Store(rawName, h)
	return h
}

// IncCounter implements metrics.Backend. It increments the named counter by
// delta. The name is sanitized before storage.
func (r *Registry) IncCounter(name string, delta uint64) {
	r.getOrCreateCounter(name).value.Add(delta)
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
		name := k.(string)
		cNames = append(cNames, name)
		cSnap[name] = v.(*counter).value.Load()
		return true
	})
	sort.Strings(cNames)

	for _, name := range cNames {
		ew.printf("# TYPE %s counter\n", name)
		ew.printf("%s %d\n", name, cSnap[name])
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
		name := k.(string)
		h := v.(*histogram)
		hNames = append(hNames, name)
		var snap histSnap
		for i := range latencyBuckets {
			snap.buckets[i] = h.buckets[i].Load()
		}
		snap.inf = h.inf.Load()
		snap.sumNs = h.sumNs.Load()
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
