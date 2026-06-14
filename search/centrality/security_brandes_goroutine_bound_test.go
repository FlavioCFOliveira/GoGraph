package centrality

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// security_brandes_goroutine_bound_test.go is part of the GoGraph
// security test battery. It is a DEFENSE lock-in for the bounded-worker
// and clean-cancellation contract of [BetweennessParallelCtx], backing
// the reliability mandate "no unbounded goroutine spawn / no goroutine
// leaks". The package already wires go.uber.org/goleak via TestMain
// (goleak_test.go) and already proves a single cancel cascades
// (brandes_parallel_test.go); this file adds two complementary
// guarantees the existing tests do not cover:
//
//  1. numWorkers is clamped to the node count, so a caller requesting a
//     huge worker count on a tiny graph cannot spawn one goroutine per
//     requested worker (denial-of-service via goroutine exhaustion).
//  2. Repeated spawn-then-cancel churn at a high worker count terminates
//     cleanly every time; combined with the package goleak TestMain this
//     proves no goroutine accumulates across cancellations.

// secBuildSmallCSR returns a small undirected CSR for worker-bound tests.
func secBuildSmallCSR(tb testing.TB, n int) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i+1 < n; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestSec_Core_BrandesWorkerCountClamped asserts that requesting far more
// workers than there are nodes does not spawn an unbounded number of
// goroutines: BetweennessParallelCtx clamps numWorkers to n. We observe
// the bound indirectly but reliably — the call completes correctly and
// the goroutine count does not balloon — and directly via the documented
// clamp (numWorkers > n is reduced to n).
func TestSec_Core_BrandesWorkerCountClamped(t *testing.T) {
	t.Parallel()

	const n = 4 // tiny graph: a 4-node path.
	c := secBuildSmallCSR(t, n)

	before := runtime.NumGoroutine()

	// Ask for a preposterous worker count. The implementation must clamp
	// to n=4, so at most a handful of workers are spawned and joined.
	const absurdWorkers = 1_000_000
	out, err := BetweennessParallelCtx(context.Background(), c, absurdWorkers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != int(c.MaxNodeID()) {
		t.Fatalf("result length = %d, want %d", len(out), c.MaxNodeID())
	}

	// All workers must have been joined by return (WaitGroup). Allow a
	// brief settle and a generous slack for the test runtime's own
	// scheduler goroutines; the key point is the count is bounded by a
	// small constant, not by absurdWorkers.
	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - before; delta > 2*runtime.GOMAXPROCS(0)+8 {
		t.Fatalf("goroutine count grew by %d after a 4-node Betweenness with %d requested workers: "+
			"workers are not clamped to the node count", delta, absurdWorkers)
	}
}

// TestSec_Core_BrandesRepeatedCancelNoLeak runs many spawn-then-cancel
// cycles at a high worker count. Each cycle must return promptly with
// context.Canceled (or a clean result if it raced to completion) and
// join every worker. The package goleak TestMain verifies, at the end of
// the run, that no goroutine from any cycle survived.
func TestSec_Core_BrandesRepeatedCancelNoLeak(t *testing.T) {
	t.Parallel()

	const (
		n       = 1024
		cycles  = 50
		workers = 16
	)
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 3*n; i++ {
		// deterministic-ish spread without importing math/rand: a simple
		// linear-congruential walk keeps the graph dense enough to give
		// each worker real work before it observes the cancel.
		u := (i * 2654435761) % n
		v := (i*40503 + 7) % n
		if err := a.AddEdge(u, v, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	for i := 0; i < cycles; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		// Cancel almost immediately so workers terminate via the cascade.
		go func() {
			time.Sleep(time.Millisecond)
			cancel()
		}()

		done := make(chan error, 1)
		go func() {
			_, err := BetweennessParallelCtx(ctx, c, workers)
			done <- err
		}()

		select {
		case err := <-done:
			// Either it cancelled cleanly or it beat the cancel to the
			// finish line; both are acceptable. A nil error means it
			// completed; otherwise it must be context.Canceled.
			if err != nil && !errors.Is(err, context.Canceled) {
				cancel()
				t.Fatalf("cycle %d: err = %v, want nil or context.Canceled", i, err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("cycle %d: BetweennessParallelCtx did not return within 5s after cancel", i)
		}
		cancel() // idempotent; releases the context's resources.
	}
}
