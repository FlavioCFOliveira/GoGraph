package search

import (
	"context"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// diameterCycle builds an undirected cycle over n keys. A cycle is the
// shape that drives [DiameterCtx] through the most sweeps: every vertex has
// the same eccentricity, so no bound removes a vertex before it has been a
// sweep source, and the run makes one bfsFarthest sweep per vertex.
func diameterCycle(t testing.TB, n int) *csr.CSR[struct{}] {
	t.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{})
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, struct{}{}); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%n, err)
		}
	}
	return csr.BuildFromAdjList(a).BuildSymmetric()
}

// diameterQueueMallocCeiling is the ceiling on the number of heap
// objects one DiameterCtx call over the fixed diameterQueueCycleOrder
// cycle may allocate.
//
// The workload is fixed, so the ceiling is structural rather than
// proportional. Before rmp #2861 the frontier queue was built inside
// bfsFarthest at capacity 1 and regrown to the reachable-set size once
// per BFS source (9394 objects on the iFUB walk of the time). Since the
// BoundingDiameters rewrite (rmp #3085) the run makes one sweep per vertex
// (1500 here) and allocates per call, never per sweep: per-worker scratch,
// the bound arrays and the worker pool. A per-source queue would add at
// least 1500 objects and break the ceiling.
const (
	diameterQueueCycleOrder    = 1500
	diameterQueueMallocCeiling = 1200
)

// TestDiameterCtx_QueueIsCallerOwnedScratch pins the allocation profile
// of the iFUB refinement walk (rmp #2861). It fails on the pre-fix
// behaviour, where every eccentricity BFS allocated and regrew its own
// frontier queue.
//
// The assertion is on runtime.MemStats.Mallocs rather than on bytes: the
// object count is what scales with the number of BFS sources, and it is
// counted exactly rather than sampled.
func TestDiameterCtx_QueueIsCallerOwnedScratch(t *testing.T) {
	c := diameterCycle(t, diameterQueueCycleOrder)

	// Warm every lazily-initialised singleton the call path touches (the
	// metrics backend pointer, the runtime's size classes for these
	// shapes) so the measured window holds only the algorithm's own work.
	if _, _, _, err := DiameterCtx(context.Background(), c); err != nil {
		t.Fatalf("warm-up DiameterCtx: %v", err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	lo, hi, exact, err := DiameterCtx(context.Background(), c)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if err != nil {
		t.Fatalf("DiameterCtx: %v", err)
	}
	// The cycle's diameter is floor(n/2), and the run is exact. Asserting the result here keeps the
	// allocation assertion honest: a fix that allocated less by doing less
	// work would fail this line first.
	wantDiameter := diameterQueueCycleOrder / 2
	if lo != wantDiameter || hi != wantDiameter || !exact {
		t.Fatalf("DiameterCtx = (%d, %d, %t), want (%d, %d, true)",
			lo, hi, exact, wantDiameter, wantDiameter)
	}

	mallocs := after.Mallocs - before.Mallocs
	if mallocs > diameterQueueMallocCeiling {
		t.Fatalf("DiameterCtx allocated %d heap objects, ceiling %d: the BFS frontier queue is being reallocated per source (rmp #2861)",
			mallocs, diameterQueueMallocCeiling)
	}
	t.Logf("DiameterCtx over a %d-vertex cycle: %d heap objects (ceiling %d)",
		diameterQueueCycleOrder, mallocs, diameterQueueMallocCeiling)
}

// TestBfsFarthest_ReusesCallerQueue is the white-box counterpart: it
// proves the reused-queue path is the one that runs, by showing a second
// sweep over an already-grown caller queue allocates nothing at all. A
// per-call queue could not reach zero, whatever the caller does.
func TestBfsFarthest_ReusesCallerQueue(t *testing.T) {
	c := diameterCycle(t, 512)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := int(c.MaxNodeID())

	dist := newBFSDist(n)
	queue := make([]graph.NodeID, 0, n)

	// First sweep: grows the queue to the reachable-set size.
	var farthest graph.NodeID
	farthest, _, _, queue = bfsFarthest(verts, edges, 0, dist, queue)
	if len(queue) == 0 {
		t.Fatal("bfsFarthest returned an empty frontier: the sweep never ran")
	}
	grown := len(queue)

	allocs := testing.AllocsPerRun(5, func() {
		_, _, _, queue = bfsFarthest(verts, edges, 0, dist, queue)
	})
	if allocs != 0 {
		t.Fatalf("bfsFarthest allocated %.0f objects per sweep over a caller-owned queue, want 0", allocs)
	}

	// Traversal order is unchanged, so the repeated sweeps agree with the
	// first on both outputs.
	again, _, distAgain, _ := bfsFarthest(verts, edges, 0, dist, queue)
	if again != farthest {
		t.Fatalf("farthest vertex changed across sweeps: %d then %d", farthest, again)
	}
	if len(distAgain) != n {
		t.Fatalf("dist length = %d, want %d", len(distAgain), n)
	}
	if got := len(queue); got != grown {
		t.Fatalf("frontier length = %d, want %d (the sweep must enqueue the same set)", got, grown)
	}
}
