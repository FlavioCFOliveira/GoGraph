// Package metrics is GoGraph's optional observability surface.
//
// The package exposes lightweight counters and latency observers
// that public blocking APIs can populate. Two backends are wired up:
//
//   - the default no-op backend: zero overhead, used when no
//     external metrics system is configured.
//   - a Prometheus-compatible backend (opt-in via [SetBackend]):
//     once installed, latency histograms and counters are exported
//     through the standard prometheus.Registry mechanism.
//
// The Prometheus client_golang import is intentionally NOT a
// dependency of this package: keeping it out of go.mod means
// every consumer that doesn't want Prometheus pays no module-graph
// cost. Callers that want the Prometheus backend implement the
// [Backend] interface in their own code; the [Prometheus] helper
// lives in a separate internal/metrics/prometheus subpackage when
// added later.
//
// Wire-up. Every public blocking API in search/, search/centrality/,
// search/community/, search/flow/, search/extern/,
// graph/io/{csv,graphml,dot,jsonl}, and
// store/{wal,snapshot,txn,checkpoint,recovery,bulk} emits a
// latency observation under the name "<package-path>.<ExportedSymbol>"
// and increments a paired "<package-path>.<ExportedSymbol>.errors"
// counter on the error path. The authoritative inventory of every
// wired metric is in docs/metrics.md; the wireup smoke test in
// internal/metrics/wireup_test.go fails loudly if a wired symbol
// stops emitting its expected name.
package metrics

import (
	"sync/atomic"
	"time"
)

// Backend is the interface every metrics sink implements. It is
// intentionally tiny so the no-op default is zero overhead.
type Backend interface {
	// IncCounter increments the named counter by delta.
	IncCounter(name string, delta uint64)
	// ObserveLatency records a single latency sample under name.
	ObserveLatency(name string, d time.Duration)
}

// noopBackend ignores every event. It is the default until
// [SetBackend] swaps it out.
type noopBackend struct{}

func (noopBackend) IncCounter(string, uint64)            {}
func (noopBackend) ObserveLatency(string, time.Duration) {}

// backendPtr is an atomic.Pointer wrapping a [Backend]; readers
// snap the current pointer on every event so backend swaps are
// lock-free and visible to all goroutines.
var backendPtr atomic.Pointer[Backend]

// current returns the active backend, lazily installing the no-op
// default on first access. It is nil-safe under concurrency: a
// SetBackend(nil) racing between the CompareAndSwap and the reload can
// leave backendPtr nil again, so the final value is dereferenced only
// when non-nil and otherwise falls back to a fresh no-op backend rather
// than dereferencing a nil pointer.
func current() Backend {
	if p := backendPtr.Load(); p != nil {
		return *p
	}
	def := Backend(noopBackend{})
	backendPtr.CompareAndSwap(nil, &def)
	if p := backendPtr.Load(); p != nil {
		return *p
	}
	// A concurrent SetBackend(nil) reset the pointer between the CAS and
	// this reload. Fall back to a no-op backend; never dereference nil.
	return noopBackend{}
}

// SetBackend swaps the global metrics sink. Pass nil to restore the
// no-op default. The function is safe to call from any goroutine at
// any time; in-flight events on the previous backend complete
// against the previous pointer.
func SetBackend(b Backend) {
	if b == nil {
		def := Backend(noopBackend{})
		backendPtr.Store(&def)
		return
	}
	backendPtr.Store(&b)
}

// IncCounter increments the named counter by delta on the current
// backend.
func IncCounter(name string, delta uint64) {
	current().IncCounter(name, delta)
}

// ObserveLatency records a latency sample on the current backend.
func ObserveLatency(name string, d time.Duration) {
	current().ObserveLatency(name, d)
}

// Time is a convenience helper that observes the elapsed time from
// invocation until the returned function is called. Usage:
//
//	defer metrics.Time("search.dijkstra")()
//
// On the no-op backend the overhead is two atomic loads + a
// time.Now pair (~50 ns), payable once per call site.
func Time(name string) func() {
	start := time.Now()
	return func() {
		current().ObserveLatency(name, time.Since(start))
	}
}
