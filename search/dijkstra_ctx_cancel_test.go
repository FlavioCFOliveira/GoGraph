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
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/testlayers"
)

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

// TestDijkstraCtx_Cancel_ViaChan verifies that cancelling a context
// via a goroutine after a short delay causes DijkstraCtx to return
// context.Canceled. The graph is large enough that Dijkstra would
// not finish before the cancellation fires.
func TestDijkstraCtx_Cancel_ViaChan(t *testing.T) {
	t.Parallel()

	const n = 100_000
	c := buildDirectedPath(n)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := DijkstraCtx(ctx, c, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestDijkstraCtx_Cancel_LargeGraph is the soak-layer variant. It
// builds a 1 M-node directed path (larger than any CPU L3 cache) and
// verifies that cancellation is honoured within 50 ms.
func TestDijkstraCtx_Cancel_LargeGraph(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const n = 1_000_000
	c := buildDirectedPath(n)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	start := time.Now()
	_, err := DijkstraCtx(ctx, c, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	const maxShutdown = 50 * time.Millisecond
	if elapsed > maxShutdown {
		t.Fatalf("shutdown too slow: %v > %v", elapsed, maxShutdown)
	}
}
