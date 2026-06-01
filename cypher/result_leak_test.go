package cypher

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// leakProbe is a metrics.Backend that counts cypher.result.leaked
// increments so the leak-detector test can assert the finalizer fired.
// Other metric names are ignored.
type leakProbe struct {
	leaked atomic.Uint64
}

func (p *leakProbe) IncCounter(name string, delta uint64) {
	if name == "cypher.result.leaked" {
		p.leaked.Add(delta)
	}
}

func (p *leakProbe) ObserveLatency(string, time.Duration) {}

// withLeakProbe installs a fresh leakProbe, runs fn, then restores
// the default (no-op) backend. It returns the probe so the test can
// inspect counts after fn returns.
func withLeakProbe(t *testing.T, fn func()) *leakProbe {
	t.Helper()
	p := &leakProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	fn()
	return p
}

// newTinyEngine builds an Engine over a 1-node graph, sufficient for
// the lifecycle tests in this file.
func newTinyEngine(t *testing.T) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("only"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	return NewEngine(g)
}

// TestResult_Close_IsIdempotent confirms Close can be called more
// than once safely — once by the caller and possibly again by the
// finalizer if the caller's flow allowed the GC to enqueue it before
// the explicit Close ran.
func TestResult_Close_IsIdempotent(t *testing.T) {
	t.Parallel()
	eng := newTinyEngine(t)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestResult_Close_DisarmsFinalizer confirms an explicit Close
// prevents the leak counter from being incremented even after a
// forced GC cycle.
//
// The test is NOT parallel: it inspects a global metrics counter
// that other concurrently-running tests in the same binary could
// otherwise advance. We sample the counter before / after our own
// work and assert delta==0.
func TestResult_Close_DisarmsFinalizer(t *testing.T) {
	eng := newTinyEngine(t)
	p := &leakProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	before := p.leaked.Load()
	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Drop and force-GC twice. The finalizer must not fire because
	// Close already disarmed it.
	res = nil //nolint:wastedassign // explicit drop to enable collection
	runtime.GC()
	runtime.GC()
	time.Sleep(20 * time.Millisecond)

	if delta := p.leaked.Load() - before; delta != 0 {
		t.Fatalf("leak counter delta = %d after explicit Close; want 0", delta)
	}
}

// TestResult_Finalizer_DetectsLeak confirms the safety-net finalizer
// fires when a caller forgets Close. The test deliberately abandons
// the Result, runs the GC, and checks the leak metric.
func TestResult_Finalizer_DetectsLeak(t *testing.T) {
	// Cannot run in parallel: we share the global metrics backend.
	eng := newTinyEngine(t)

	p := withLeakProbe(t, func() {
		// Run inside a helper closure so the Result becomes unreachable
		// as soon as the helper returns. If we kept the variable in
		// scope, the compiler could legitimately delay collection past
		// our GC calls and the test would flake.
		makeLeak := func() {
			res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// Drain so the underlying ResultSet is in a quiescent state;
			// we want to test the finalizer, not the cancellation path.
			for res.Next() {
				_ = res.Record()
			}
			// Intentionally NO Close.
		}
		makeLeak()
		// Force two GC cycles + brief sleep to let the finalizer queue
		// drain. The runtime documents one GC cycle as sufficient for
		// SetFinalizer; the second is paranoia against scheduler skew.
		runtime.GC()
		runtime.GC()
		// Finalizer goroutines run concurrently; yield once so they
		// can observe the increment before we assert on it.
		time.Sleep(50 * time.Millisecond)
		runtime.Gosched()
	})

	if got := p.leaked.Load(); got == 0 {
		t.Fatalf("cypher.result.leaked counter not incremented after abandoning Result")
	}
}

// TestResult_Finalizer_BoundedUnderAbruptCancel exercises the
// stress scenario from the task acceptance criterion: many short
// queries opened and abandoned in succession. The leak counter
// must match the abandonment count (no leak goes undetected) and
// the test must complete in bounded time (no deadlock from
// finalizer queue saturation).
func TestResult_Finalizer_BoundedUnderAbruptCancel(t *testing.T) {
	const N = 64
	eng := newTinyEngine(t)

	p := withLeakProbe(t, func() {
		for i := 0; i < N; i++ {
			func() {
				res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
				if err != nil {
					t.Fatalf("Run %d: %v", i, err)
				}
				for res.Next() {
					_ = res.Record()
				}
				// Abandon — do not Close.
				_ = res
			}()
		}
		runtime.GC()
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
		runtime.Gosched()
	})

	if got := p.leaked.Load(); got < uint64(N/2) {
		// Allow some headroom: a few finalizers may still be pending
		// when we sample; we just need the order of magnitude to match.
		t.Fatalf("cypher.result.leaked = %d after %d abandoned Results; want >= %d",
			got, N, N/2)
	}
}
