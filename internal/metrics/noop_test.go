package metrics

import (
	"testing"
	"time"
)

// TestNoopBackend_DirectInvocation pins the no-op backend's contract:
// every method must be a no-cost no-op that accepts any payload and
// returns nothing observable. Calling them directly (instead of via
// the package-level dispatchers) guarantees coverage of the no-op
// path independently of any concurrent test that may have swapped
// the global backend.
func TestNoopBackend_DirectInvocation(t *testing.T) {
	t.Parallel()
	var b Backend = noopBackend{}
	// These calls have no return value and must not panic.
	b.IncCounter("never.observed", 1<<32)
	b.IncCounter("", 0)
	b.ObserveLatency("never.observed", time.Hour)
	b.ObserveLatency("", 0)
}
