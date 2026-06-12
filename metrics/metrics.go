// Package metrics is the public observability facade for GoGraph.
//
// It re-exports the [Backend] interface and the [SetBackend] function
// from the internal metrics subsystem, and provides a ready-to-use
// Prometheus text-exposition-format registry through
// [NewPrometheusRegistry]. External consumers should import this
// package instead of the internal sub-packages.
//
// # Wire-up
//
// Install the Prometheus backend early in main, before any blocking
// APIs are called:
//
//	import (
//	    "net/http"
//	    "github.com/FlavioCFOliveira/GoGraph/metrics"
//	)
//
//	reg := metrics.NewPrometheusRegistry()
//	metrics.SetBackend(reg)
//
//	// Expose /metrics endpoint:
//	http.Handle("/metrics", reg.Handler())
//	http.ListenAndServe(":9090", nil)
//
// To integrate with prometheus/client_golang or OpenTelemetry instead,
// implement [Backend] directly and install it via [SetBackend]:
//
//	type myBackend struct{}
//
//	func (b *myBackend) IncCounter(name string, delta uint64)         { /* ... */ }
//	func (b *myBackend) ObserveLatency(name string, d time.Duration)  { /* ... */ }
//
//	metrics.SetBackend(&myBackend{})
//
// [SetBackend](nil) restores the no-op default. Backend swaps are
// lock-free (atomic.Pointer), so a single global swap is safe even
// under concurrent load.
//
// # Metric names
//
// Every metric name follows the schema
//
//	<package-path>.<ExportedSymbol>[.errors]
//
// The full inventory is documented in docs/metrics.md.
package metrics

import (
	internalmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	internalprom "github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// Backend is the interface every metrics sink must implement.
// It is identical to internal/metrics.Backend; the type alias ensures
// that values of either type are interchangeable without conversion.
type Backend = internalmetrics.Backend

// SetBackend swaps the global metrics sink used by all GoGraph
// blocking APIs. Pass nil to restore the no-op default.
//
// The function is safe to call from any goroutine at any time;
// in-flight events on the previous backend complete against the
// previous pointer.
func SetBackend(b Backend) {
	internalmetrics.SetBackend(b)
}

// Registry is the Prometheus text-exposition-format backend.
// It is identical to internal/metrics/prometheus.Registry; the type
// alias lets callers call Handler() and WriteText() directly without
// importing the internal sub-package.
type Registry = internalprom.Registry

// NewPrometheusRegistry creates a new [Registry] ready to use as a
// [Backend]. The registry collects all counter increments and latency
// observations emitted by GoGraph's public APIs and serves them in
// Prometheus text format (version 0.0.4) via [Registry.WriteText] or
// [Registry.Handler].
//
// No external dependencies are required: the registry serialises the
// native Prometheus text format itself.
func NewPrometheusRegistry() *Registry {
	return internalprom.New()
}
