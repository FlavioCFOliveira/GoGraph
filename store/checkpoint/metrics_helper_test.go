package checkpoint

// metrics_helper_test.go — Local test-only helpers for capturing
// metric emissions during checkpoint tests.
//
// Mirrors the pattern in internal/metrics/wireup_test.go but kept
// package-private here to avoid an import-cycle and an unrelated
// dependency on internal/metrics test code.

import (
	"sync"
	"time"

	"gograph/internal/metrics"
)

// countingBackend records every IncCounter/ObserveLatency call so
// tests can assert specific metric names were emitted.
type countingBackend struct {
	mu      sync.Mutex
	counter map[string]uint64
	latency map[string][]time.Duration
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		counter: map[string]uint64{},
		latency: map[string][]time.Duration{},
	}
}

func (c *countingBackend) IncCounter(name string, delta uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counter[name] += delta
}

func (c *countingBackend) ObserveLatency(name string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latency[name] = append(c.latency[name], d)
}

func (c *countingBackend) count(name string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counter[name]
}

func (c *countingBackend) snapshot() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.counter))
	for k, v := range c.counter {
		out[k] = v
	}
	return out
}

// setMetricsBackend installs b as the current metrics backend and
// returns the previous one. The previous backend may be nil if the
// no-op default was active, in which case setMetricsBackend(nil)
// restores the no-op.
func setMetricsBackend(b metrics.Backend) metrics.Backend {
	// metrics.SetBackend(nil) restores the no-op default; we mirror
	// that contract by returning nil to indicate "default" so callers
	// can pass the returned value back symmetrically.
	metrics.SetBackend(b)
	return nil // caller restores by passing nil
}
