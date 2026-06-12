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
// observations are lock-free once the named series has been created; the
// write-lock is held only during the first creation of a new name.
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
	// counterMu guards counterMap; held only during first-creation.
	counterMu  sync.RWMutex
	counterMap map[string]*counter

	// histMu guards histMap; held only during first-creation.
	histMu  sync.RWMutex
	histMap map[string]*histogram
}

// New creates a new Registry ready for use as a metrics.Backend.
func New() *Registry {
	return &Registry{
		counterMap: make(map[string]*counter),
		histMap:    make(map[string]*histogram),
	}
}

// sanitize converts Prometheus-incompatible characters in metric names to
// underscores. The set {'.', '-', '/'} is replaced; all other characters
// are passed through unchanged.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '-', '/':
			return '_'
		}
		return r
	}, name)
}

// getOrCreateCounter returns the existing counter for name, or creates one.
// The fast path (counter already exists) takes only an RLock.
func (r *Registry) getOrCreateCounter(name string) *counter {
	r.counterMu.RLock()
	c, ok := r.counterMap[name]
	r.counterMu.RUnlock()
	if ok {
		return c
	}

	r.counterMu.Lock()
	// Re-check under write-lock to guard against a concurrent creator.
	if c, ok = r.counterMap[name]; ok {
		r.counterMu.Unlock()
		return c
	}
	c = &counter{}
	r.counterMap[name] = c
	r.counterMu.Unlock()
	return c
}

// getOrCreateHistogram returns the existing histogram for name, or creates one.
func (r *Registry) getOrCreateHistogram(name string) *histogram {
	r.histMu.RLock()
	h, ok := r.histMap[name]
	r.histMu.RUnlock()
	if ok {
		return h
	}

	r.histMu.Lock()
	if h, ok = r.histMap[name]; ok {
		r.histMu.Unlock()
		return h
	}
	h = &histogram{}
	r.histMap[name] = h
	r.histMu.Unlock()
	return h
}

// IncCounter implements metrics.Backend. It increments the named counter by
// delta. The name is sanitized before storage.
func (r *Registry) IncCounter(name string, delta uint64) {
	r.getOrCreateCounter(sanitize(name)).value.Add(delta)
}

// ObserveLatency implements metrics.Backend. It records d in the latency
// histogram named name. The name is sanitized before storage.
func (r *Registry) ObserveLatency(name string, d time.Duration) {
	r.getOrCreateHistogram(sanitize(name)).observe(d)
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

	// Snapshot counter names under read-lock.
	r.counterMu.RLock()
	cNames := make([]string, 0, len(r.counterMap))
	cSnap := make(map[string]uint64, len(r.counterMap))
	for name, c := range r.counterMap {
		cNames = append(cNames, name)
		cSnap[name] = c.value.Load()
	}
	r.counterMu.RUnlock()
	sort.Strings(cNames)

	for _, name := range cNames {
		ew.printf("# TYPE %s counter\n", name)
		ew.printf("%s %d\n", name, cSnap[name])
	}

	// Snapshot histogram names under read-lock.
	r.histMu.RLock()
	hNames := make([]string, 0, len(r.histMap))
	type histSnap struct {
		buckets [len(latencyBuckets)]uint64
		inf     uint64
		sumNs   int64
	}
	hSnap := make(map[string]histSnap, len(r.histMap))
	for name, h := range r.histMap {
		hNames = append(hNames, name)
		var snap histSnap
		for i := range latencyBuckets {
			snap.buckets[i] = h.buckets[i].Load()
		}
		snap.inf = h.inf.Load()
		snap.sumNs = h.sumNs.Load()
		hSnap[name] = snap
	}
	r.histMu.RUnlock()
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
