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

	// Settle the finalizers owed by Results that EARLIER tests in this binary
	// abandoned, BEFORE the probe is installed, so their increments land on the
	// outgoing backend instead of this test's counter.
	//
	// Not defensive padding — a measured defect. Those Results are already
	// unreachable but their finalizers have not necessarily run, so the two
	// forced GCs further down would collect them INSIDE the measurement window
	// and charge their increments to this test. Observed on a full-suite `make
	// smoke` run as "leak counter delta = 13 after explicit Close" while the
	// test passed in isolation and on three consecutive runs of this package
	// alone — a latent flake, not a regression in Close.
	//
	// The test's own note below is right that a *parallel* sibling cannot
	// interfere (Go defers parallel tests until the sequential ones finish), but
	// that was never the contamination source; completed earlier tests were.
	runtime.GC()
	runtime.GC()
	time.Sleep(20 * time.Millisecond)

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

// TestDDLSequences_DoNotLeakAnInternalResult is the regression gate for a defect
// found while fixing rmp #2229: four intra-sequence callers of the DDL operator
// runner discarded the *Result it returned with `_`, so the finalizer armed on
// that Result fired on the next GC and counted a leak against the library's own
// `cypher.result.leaked` metric.
//
// It was invisible in normal use — the statement still succeeded and the caller
// still got exactly one Result to close — and invisible in the test suite except
// as a mysterious non-zero delta in TestResult_Close_DisarmsFinalizer, which
// forces a GC and therefore collects whatever any earlier test left pending.
// That is the failure mode this test replaces with a direct assertion.
//
// CREATE CONSTRAINT was the original reproduction (one leak per statement);
// DROP CONSTRAINT went through the same runner. CREATE INDEX did not, and is
// kept here as the control that already passed.
func TestDDLSequences_DoNotLeakAnInternalResult(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"CREATE INDEX (control: never leaked)", []string{
			`CREATE INDEX t_p FOR (n:T) ON (n.p)`,
		}},
		{"CREATE CONSTRAINT UNIQUE", []string{
			`CREATE CONSTRAINT t_p_uniq FOR (n:T) REQUIRE n.p IS UNIQUE`,
		}},
		{"CREATE CONSTRAINT NOT NULL", []string{
			`CREATE CONSTRAINT t_p_nn FOR (n:T) REQUIRE n.p IS NOT NULL`,
		}},
		{"CREATE then DROP CONSTRAINT", []string{
			`CREATE CONSTRAINT t_p_uniq FOR (n:T) REQUIRE n.p IS UNIQUE`,
			`DROP CONSTRAINT t_p_uniq`,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: the leak counter is a process-global metric backend.
			eng := newTinyEngine(t)
			p := withLeakProbe(t, func() {})

			before := p.leaked.Load()
			for _, stmt := range tc.stmts {
				res, err := eng.RunAny(context.Background(), stmt, nil)
				if err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
				if err := res.Close(); err != nil {
					t.Fatalf("%s: close: %v", stmt, err)
				}
			}
			// Force the finalizer of anything the statement abandoned.
			runtime.GC()
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			runtime.GC()
			time.Sleep(20 * time.Millisecond)

			if delta := p.leaked.Load() - before; delta != 0 {
				t.Fatalf("leak counter delta = %d after %d correctly-closed statement(s); want 0. "+
					"A DDL sequence is building an internal Result it never closes — use applyDDLOp, "+
					"which runs the operator for its effect without constructing one", delta, len(tc.stmts))
			}
		})
	}
}
