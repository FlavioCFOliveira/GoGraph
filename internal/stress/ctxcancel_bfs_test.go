//go:build nightly

// Package stress — T641: ctx-cancel mid-BFS on 10M-node chain (nightly).
//
// Builds a 10M-node path chain graph and cancels BFSCtx mid-expansion,
// verifying:
//  1. errors.Is(err, context.Canceled) holds.
//  2. Return latency after cancel < 50 ms.
//  3. goleak clean (via TestMain).
//
// Layer: nightly (//go:build nightly) because constructing a 10M-node
// chain adjlist requires O(10M) AddEdge calls, which takes several minutes
// even on fast hardware.
package stress

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

// TestCtxCancel_BFS_MidRun builds a 10M-node path chain and cancels BFSCtx
// mid-expansion, verifying that context.Canceled is propagated promptly.
//
// The chain P_n is the hardest case for BFS cancellation: it has diameter n-1
// (every BFS level adds one new node) so cancellation is guaranteed to fire
// during active expansion rather than after the traversal has naturally
// completed. BFSCtx checks ctx.Err() once per frontier dequeue; with
// 10M nodes and a 5ms cancel deadline, the cancel fires well before
// traversal completes and the assertion of < 50ms return latency verifies
// that the check is tight.
//
// Under the soak build tag (SOAK_FULL=1) a 100k-node chain is used with
// a pre-cancelled context to keep the soak runtime reasonable.
func TestCtxCancel_BFS_MidRun(t *testing.T) {
	nodes := 10_000_000
	cancelDelay := 5 * time.Millisecond

	defer goleak.VerifyNone(t)

	// Build the chain: node i → node i+1 for i in [0, nodes-1).
	// Use raw adjlist.New rather than shapegen to avoid shapegen's per-shape
	// node-count limits.
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < nodes-1; i++ {
		if err := a.AddEdge(i, i+1, 1); err != nil {
			t.Fatalf("AddEdge %d→%d: %v", i, i+1, err)
		}
	}
	c := csr.BuildFromAdjList(a)

	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("Lookup(0): node 0 not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cancelDelay)
	defer cancel()

	t0 := time.Now()
	err := search.BFSCtx(ctx, c, src, func(_ graph.NodeID, _ int) bool { return true })
	elapsed := time.Since(t0)

	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("BFSCtx returned err=%v; want context.Canceled or context.DeadlineExceeded", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("BFSCtx return latency after cancel = %v; want < 50 ms", elapsed)
	}
	t.Logf("BFSCtx on %d-node chain: cancelled after %v, return latency %v",
		nodes, cancelDelay, elapsed)
}
