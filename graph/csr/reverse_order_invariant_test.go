package csr

// reverse_order_invariant_test.go — the load-bearing ordering invariant of
// [CSR.BuildReverse] (rmp #2151, for #2149).
//
// Layer: short.
//
// WHY THIS TEST EXISTS. [BuildFromAdjList] and [BuildFromAdjListLive] order their
// runs by calling [OrderRuns] explicitly, so a reader can see the guarantee.
// BuildReverse does NOT call it: its runs come out ordered by (source, handle) only
// BY CONSTRUCTION, because pass 2 iterates the source u ascending over forward runs
// that are already (destination, handle)-ordered. Nothing in the code says so.
//
// That implicit property became load-bearing in #2149, when the query executor
// started BINARY-SEARCHING a reverse run to seek an already-bound destination
// (Expand.seekIntoRuns, via cypher/exec/csrprobe.go). A binary search over an
// unordered run does not return a wrong row slowly — it silently returns the WRONG
// ROWS, or none. So a future change to the scatter order in pass 2 must fail a test
// rather than corrupt a query, which is what this pins.
//
// The forward CSR is asserted too, so the test states the whole invariant the probes
// rely on rather than half of it.

import (
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// assertRunsOrdered requires every source's run in c to be non-decreasing in the
// total key (neighbour, handle) — the exact precondition lowerBoundDst, firstDstPos
// and dstRun are written against.
func assertRunsOrdered(t *testing.T, which string, c *CSR[float64], iter int) {
	t.Helper()
	verts, edges, handles := c.VerticesSlice(), c.EdgesSlice(), c.HandlesSlice()
	for u := 0; u+1 < len(verts); u++ {
		start, end := verts[u], verts[u+1]
		for p := start + 1; p < end; p++ {
			prevN, curN := uint64(edges[p-1]), uint64(edges[p])
			if curN < prevN {
				t.Fatalf("iter %d: %s run of node %d is NOT neighbour-ordered at pos %d: %d then %d",
					iter, which, u, p, prevN, curN)
			}
			if curN == prevN && handles != nil && handles[p] < handles[p-1] {
				t.Fatalf("iter %d: %s run of node %d is NOT handle-ordered within neighbour %d "+
					"at pos %d: handle %d then %d",
					iter, which, u, curN, p, handles[p-1], handles[p])
			}
		}
	}
}

// TestBuildReverse_RunsAreNeighbourAndHandleOrdered_2151 asserts the invariant over
// randomised multigraphs that are DENSE IN PARALLEL EDGES — the case that makes the
// handle component of the key observable at all — and that include a handle-0 slot,
// the "no handle" sentinel a run may legitimately carry.
func TestBuildReverse_RunsAreNeighbourAndHandleOrdered_2151(t *testing.T) {
	rng := rand.New(rand.NewSource(20482151))
	const (
		iterations = 200
		nodes      = 24
		arcs       = 160
	)
	for iter := 0; iter < iterations; iter++ {
		adj := adjlist.New[int, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < nodes; i++ {
			if err := adj.AddNode(i); err != nil {
				t.Fatalf("AddNode(%d): %v", i, err)
			}
		}
		// Far more arcs than node pairs, so the same (u,v) recurs and handles are
		// what distinguish the slots within a destination run.
		for k := 0; k < arcs; k++ {
			u, v := rng.Intn(nodes), rng.Intn(nodes)
			if err := adj.AddEdgeH(u, v, 1.0, uint64(k+1)); err != nil {
				t.Fatalf("AddEdgeH: %v", err)
			}
		}
		if err := adj.AddEdgeH(rng.Intn(nodes), rng.Intn(nodes), 1.0, 0); err != nil {
			t.Fatalf("AddEdgeH handle 0: %v", err)
		}

		fwd := BuildFromAdjList(adj)
		assertRunsOrdered(t, "forward", fwd, iter)
		assertRunsOrdered(t, "reverse", fwd.BuildReverse(), iter)
	}
}

// TestBuildReverse_SelfLoopsAndSingletonsStayOrdered_2151 covers the degenerate
// shapes the randomised loop is unlikely to produce in isolation: a node whose whole
// adjacency is self-loops (so its reverse run is its forward run), a node with no
// edges at all (an empty run, which every probe must treat as a miss rather than
// walking into the next node's slots), and a graph of one node.
func TestBuildReverse_SelfLoopsAndSingletonsStayOrdered_2151(t *testing.T) {
	adj := adjlist.New[int, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 4; i++ {
		if err := adj.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	// Node 0: three parallel self-loops, handles deliberately added out of order so
	// only a real ordering pass could produce an ordered run.
	for _, h := range []uint64{9, 3, 7} {
		if err := adj.AddEdgeH(0, 0, 1.0, h); err != nil {
			t.Fatalf("AddEdgeH self-loop: %v", err)
		}
	}
	// Node 1: one edge. Node 2: none at all. Node 3: parallel edges to one target,
	// handles descending on input.
	if err := adj.AddEdgeH(1, 3, 1.0, 5); err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	for _, h := range []uint64{8, 2} {
		if err := adj.AddEdgeH(3, 1, 1.0, h); err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
	}

	fwd := BuildFromAdjList(adj)
	rev := fwd.BuildReverse()
	assertRunsOrdered(t, "forward", fwd, 0)
	assertRunsOrdered(t, "reverse", rev, 0)

	// Node 2 has no incident edges, so its run must be EMPTY in both directions —
	// the case a probe must report as a miss.
	for _, tc := range []struct {
		name string
		c    *CSR[float64]
	}{{"forward", fwd}, {"reverse", rev}} {
		verts := tc.c.VerticesSlice()
		if len(verts) > 3 && verts[2] != verts[3] {
			t.Fatalf("%s run of node 2 is not empty: [%d,%d)", tc.name, verts[2], verts[3])
		}
	}
}
