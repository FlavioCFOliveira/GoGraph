package search

// dijkstra_ctx_cancel_test.go — Task 738, Sprint 62.
//
// Tests that DijkstraCtx honours context cancellation:
//   - pre-cancelled context: returns context.Canceled immediately.
//   - goroutine-triggered cancellation: returns context.Canceled.
//
// A soak-layer variant builds a 1 M-node graph and verifies that
// Dijkstra shuts down within 50 ms of cancellation.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// cancelAfterFirstCheck is a context whose Err returns nil on its first call
// and context.Canceled on every call thereafter. DijkstraCtx polls ctx.Err()
// every 4096 heap pops, so the first poll lets the traversal begin and a later
// poll observes the cancellation — making "cancelled mid-traversal" fully
// deterministic, with no dependence on goroutine scheduling or traversal speed
// (the source of the historical flake under load and -coverpkg instrumentation).
type cancelAfterFirstCheck struct {
	context.Context
	calls atomic.Int64
}

func (c *cancelAfterFirstCheck) Err() error {
	if c.calls.Add(1) <= 1 {
		return nil
	}
	return context.Canceled
}

// buildDirectedPath constructs a directed CSR path
// 0 → 1 → 2 → … → (n-1) with edge weight 1.0.
// The path ensures that Dijkstra has meaningful work to do before
// cancellation; its linear topology keeps construction O(n).
func buildDirectedPath(n int) *csr.CSR[float64] {
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		_ = a.AddNode(i) // AddNode is total; error is always nil
	}
	for i := 0; i < n-1; i++ {
		_ = a.AddEdge(i, i+1, 1.0) // linear path; no error possible
	}
	return csr.BuildFromAdjList(a)
}

// TestDijkstraCtx_Cancel_PreCancelled verifies that a pre-cancelled
// context causes DijkstraCtx to return context.Canceled immediately
// without performing any traversal.
func TestDijkstraCtx_Cancel_PreCancelled(t *testing.T) {
	t.Parallel()

	// Medium graph: 100 k nodes, path topology.
	const n = 100_000
	c := buildDirectedPath(n)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before calling Dijkstra

	_, err := DijkstraCtx(ctx, c, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestDijkstraCtx_Cancel_DuringTraversal verifies that a context which becomes
// cancelled after the traversal has started causes DijkstraCtx to observe the
// cancellation and return context.Canceled.
//
// This replaces an earlier goroutine+sleep variant that was inherently racy:
// it relied on a 1 ms cancel timer beating a large traversal, which broke on
// fast cores (traversal finishes first → spurious nil, task #925) and again
// under -coverpkg instrumentation on slow CI runners (cancel goroutine starved
// → traversal finishes first). cancelAfterFirstCheck removes the race entirely:
// the traversal must run past at least one ctx.Err() poll before the next poll
// reports cancellation. The graph only needs to exceed the 4096-pop poll
// interval, so 20 k nodes suffices and keeps the test fast.
func TestDijkstraCtx_Cancel_DuringTraversal(t *testing.T) {
	t.Parallel()

	const n = 20_000
	c := buildDirectedPath(n)

	ctx := &cancelAfterFirstCheck{Context: context.Background()}

	_, err := DijkstraCtx(ctx, c, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestDijkstraCtx_Cancel_LargeGraph is the soak-layer variant. It builds a
// 1 M-node directed path (larger than any CPU L3 cache) and verifies that
// cancellation is honoured at scale: the traversal aborts at the
// deterministic ctx.Err() poll boundary rather than scanning all 1 M nodes.
//
// It reuses the cancelAfterFirstCheck mechanism of the mid-traversal variant
// — deterministic, with no dependence on wall-clock timing or goroutine
// scheduling. The earlier version asserted shutdown within an absolute 50 ms
// window; that wall-clock bound flaked under -race on loaded CI runners
// (observed: 386 ms > 50 ms), failing the Nightly job for a scheduling
// artefact rather than a real regression. Promptness is now guaranteed
// structurally: cancelAfterFirstCheck forces a return at the second poll
// (~8192 pops), independent of graph size, so a regression that ignored
// cancellation would scan all 1 M nodes and return a nil error — caught by
// the errors.Is check. The generous wall-clock ceiling is only a
// belt-and-braces guard against such a runaway, set far above any plausible
// scheduling jitter so it cannot itself flake.
func TestDijkstraCtx_Cancel_LargeGraph(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const n = 1_000_000
	c := buildDirectedPath(n)

	ctx := &cancelAfterFirstCheck{Context: context.Background()}

	start := time.Now()
	_, err := DijkstraCtx(ctx, c, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("cancellation ignored: 1 M-node traversal not aborted (%v)", elapsed)
	}
}
