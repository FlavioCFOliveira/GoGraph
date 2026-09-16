package search

import (
	"context"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// denseChainCSR builds a weighted CSR over a dense NodeID space of
// spanNodes slots in which only the first chainNodes slots carry edges: a
// bidirectional chain 0-1-…-(chainNodes-1), every remaining slot isolated.
//
// It uses csr.FromArrays rather than the adjacency path because the
// project's Mapper scatters keys across 256 shards, which puts a reached
// node in almost every region of the id space. A dense id space is what a
// bulk loader (store/bulk) produces, and it is the only shape in which
// "the span the query reached" is a meaningful prefix of MaxNodeID.
func denseChainCSR(t testing.TB, chainNodes, spanNodes int) *csr.CSR[int64] {
	t.Helper()
	if chainNodes < 2 || chainNodes > spanNodes {
		t.Fatalf("denseChainCSR(%d, %d): need 2 <= chain <= span", chainNodes, spanNodes)
	}
	vertices := make([]uint64, spanNodes+1)
	var edges []graph.NodeID
	var weights []int64
	for v := 0; v < spanNodes; v++ {
		vertices[v] = uint64(len(edges))
		if v < chainNodes {
			if v > 0 {
				edges = append(edges, graph.NodeID(v-1))
				weights = append(weights, 1)
			}
			if v+1 < chainNodes {
				edges = append(edges, graph.NodeID(v+1))
				weights = append(weights, 1)
			}
		}
	}
	vertices[spanNodes] = uint64(len(edges))
	return csr.FromArrays(vertices, edges, weights, uint64(chainNodes), uint64(len(edges)))
}

// TestNewDistancesCopy_SizedToReachedSpan pins the working-set change of
// rmp #2862: the returned [Distances] carries only the span the traversal
// reached, not the whole MaxNodeID span. It fails on the pre-fix
// behaviour, which copied three arrays of length MaxNodeID regardless of
// reach.
//
// The observable surface is asserted separately, node by node, against
// the full id space — the truncation must be invisible through every
// accessor.
func TestNewDistancesCopy_SizedToReachedSpan(t *testing.T) {
	t.Parallel()
	const (
		chain = 8
		span  = 4096
	)
	c := denseChainCSR(t, chain, span)
	if got := uint64(c.MaxNodeID()); got != span {
		t.Fatalf("MaxNodeID() = %d, want %d", got, span)
	}

	d, err := Dijkstra(c, 0)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}

	// White-box: the arrays are sized to the reached span. This is the
	// evidence that the new path actually ran — a result-identical change
	// is invisible to the black-box assertions below.
	if got := len(d.found); got != chain {
		t.Fatalf("len(found) = %d, want %d (the result is still sized to MaxNodeID = %d)", got, chain, span)
	}
	if got := len(d.dist); got != chain {
		t.Fatalf("len(dist) = %d, want %d", got, chain)
	}
	if got := len(d.parent); got != chain {
		t.Fatalf("len(parent) = %d, want %d", got, chain)
	}

	// Black-box: every node of the FULL id space answers exactly as it
	// would have with full-length arrays.
	for v := 0; v < span; v++ {
		gotDist, gotOK := d.Distance(graph.NodeID(v))
		wantOK := v < chain
		var wantDist int64
		if wantOK {
			wantDist = int64(v)
		}
		if gotOK != wantOK || gotDist != wantDist {
			t.Fatalf("Distance(%d) = (%d, %t), want (%d, %t)", v, gotDist, gotOK, wantDist, wantOK)
		}
		path := d.Path(graph.NodeID(v))
		if !wantOK {
			if path != nil {
				t.Fatalf("Path(%d) = %v, want nil", v, path)
			}
			continue
		}
		if len(path) != v+1 {
			t.Fatalf("Path(%d) has %d nodes, want %d", v, len(path), v+1)
		}
		for i, node := range path {
			if node != graph.NodeID(i) {
				t.Fatalf("Path(%d)[%d] = %d, want %d", v, i, node, i)
			}
		}
	}
	if d.Source() != 0 {
		t.Fatalf("Source() = %d, want 0", d.Source())
	}
}

// TestNewDistancesCopy_FullSpanWhenFullyReached is the complement: when
// the traversal reaches the highest id, nothing is truncated. It guards
// against a truncation that clipped a reachable node.
func TestNewDistancesCopy_FullSpanWhenFullyReached(t *testing.T) {
	t.Parallel()
	const span = 64
	c := denseChainCSR(t, span, span)

	d, err := Dijkstra(c, 0)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}
	if got := len(d.found); got != span {
		t.Fatalf("len(found) = %d, want %d", got, span)
	}
	for v := 0; v < span; v++ {
		got, ok := d.Distance(graph.NodeID(v))
		if !ok || got != int64(v) {
			t.Fatalf("Distance(%d) = (%d, %t), want (%d, true)", v, got, ok, v)
		}
	}
}

// TestNewDistancesCopy_UnreachedSourceYieldsEmptySpan covers the
// degenerate reach: a source outside the vertex array reaches nothing, so
// the span is zero and every lookup still answers "not found".
func TestNewDistancesCopy_UnreachedSourceYieldsEmptySpan(t *testing.T) {
	t.Parallel()
	const span = 32
	c := denseChainCSR(t, 4, span)

	d, err := Dijkstra(c, graph.NodeID(span))
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}
	if got := len(d.found); got != 0 {
		t.Fatalf("len(found) = %d, want 0", got)
	}
	for v := 0; v <= span; v++ {
		if _, ok := d.Distance(graph.NodeID(v)); ok {
			t.Fatalf("Distance(%d) reported found on a traversal that never ran", v)
		}
		if p := d.Path(graph.NodeID(v)); p != nil {
			t.Fatalf("Path(%d) = %v, want nil", v, p)
		}
	}
}

// denseStarCSR builds a weighted, directed star over a dense NodeID
// space: node 0 has an out-edge to every other slot, and no slot has any
// other edge.
//
// The shape matters. A chain keeps the priority queue two items deep
// however long it is, so its backing never grows and the pre-sizing is
// unobservable; a star pushes every other node before the first pop, so
// the queue's high-water mark is the node count and an un-hinted backing
// has to climb the whole doubling chain to reach it.
func denseStarCSR(t testing.TB, spanNodes int) *csr.CSR[int64] {
	t.Helper()
	if spanNodes < 2 {
		t.Fatalf("denseStarCSR(%d): need at least 2 slots", spanNodes)
	}
	vertices := make([]uint64, spanNodes+1)
	edges := make([]graph.NodeID, 0, spanNodes-1)
	weights := make([]int64, 0, spanNodes-1)
	for v := 1; v < spanNodes; v++ {
		edges = append(edges, graph.NodeID(v))
		weights = append(weights, int64(spanNodes-v)) // descending, so each push improves
	}
	vertices[0] = 0
	for v := 1; v <= spanNodes; v++ {
		vertices[v] = uint64(len(edges))
	}
	return csr.FromArrays(vertices, edges, weights, uint64(spanNodes), uint64(len(edges)))
}

// dijkstraIntoColdPoolRounds and dijkstraIntoMallocCeiling bound the
// heap-backing allocation of the *Into entrypoints across repeated
// cold-pool acquisitions.
//
// The ceiling is structural over a FIXED workload: with the backing
// pre-sized from the caller's node count, one cold acquisition allocates
// a bounded set of objects (the pooled struct plus one right-sized
// backing), so the count is O(rounds). Before rmp #2862 acquireDijkHeap
// returned a zero-capacity heap and the traversal climbed to the queue's
// high-water mark through a doubling chain, making it
// O(rounds * log2(maxID)). Measured on this shape, go1.27.1 darwin/arm64
// without -race: 965 objects at 8affe124, 214 after the fix. 350 sits
// between them — 1.6x slack over the measured value, 2.8x below the
// pre-fix count.
const (
	dijkstraIntoColdPoolRounds = 50
	dijkstraIntoMallocCeiling  = 350
	dijkstraIntoStarSpan       = 4096
)

// TestDijkstraInto_HeapBackingPreSized proves the *Into heap comes back
// from the pool already sized for the graph, by forcing a cold pool
// before every call. Two runtime.GC() calls are what it takes: sync.Pool
// moves its primary cache to the victim cache on the first and drops the
// victim on the second, so the next acquisition is a genuine miss —
// exactly the state a high allocation rate keeps the pool in, and the
// state under which the doubling chain was 7.88% of a profile's total
// allocation.
func TestDijkstraInto_HeapBackingPreSized(t *testing.T) {
	c := denseStarCSR(t, dijkstraIntoStarSpan)
	maxID := uint64(c.MaxNodeID())
	dist := make([]int64, maxID)
	parent := make([]graph.NodeID, maxID)
	found := make([]bool, maxID)
	ctx := context.Background()

	// Warm-up outside the measured window: the first call of the process
	// installs the per-W pools and the metrics backend.
	if err := DijkstraInto(ctx, c, 0, dist, parent, found); err != nil {
		t.Fatalf("warm-up DijkstraInto: %v", err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < dijkstraIntoColdPoolRounds; i++ {
		runtime.GC()
		runtime.GC()
		if err := DijkstraInto(ctx, c, 0, dist, parent, found); err != nil {
			t.Fatalf("DijkstraInto round %d: %v", i, err)
		}
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// The traversal must still be correct, and the queue must really have
	// reached its high-water mark — a heap that never grew because nothing
	// was pushed would pass the ceiling for the wrong reason.
	for v := 1; v < dijkstraIntoStarSpan; v++ {
		if !found[v] || dist[v] != int64(dijkstraIntoStarSpan-v) {
			t.Fatalf("star spoke %d: found=%t dist=%d, want true and %d",
				v, found[v], dist[v], dijkstraIntoStarSpan-v)
		}
	}

	mallocs := after.Mallocs - before.Mallocs
	if mallocs > dijkstraIntoMallocCeiling {
		t.Fatalf("%d cold-pool DijkstraInto calls allocated %d heap objects, ceiling %d: the heap backing is being grown by append (rmp #2862)",
			dijkstraIntoColdPoolRounds, mallocs, dijkstraIntoMallocCeiling)
	}
	t.Logf("%d cold-pool DijkstraInto calls over a %d-spoke star: %d heap objects (ceiling %d)",
		dijkstraIntoColdPoolRounds, dijkstraIntoStarSpan-1, mallocs, dijkstraIntoMallocCeiling)
}

// TestReachedSpan covers the helper directly, including the two ends of
// the scan the callers cannot reach through a traversal.
func TestReachedSpan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		found []bool
		want  uint64
	}{
		{name: "empty", found: nil, want: 0},
		{name: "none-reached", found: []bool{false, false, false}, want: 0},
		{name: "first-only", found: []bool{true, false, false}, want: 1},
		{name: "last-only", found: []bool{false, false, true}, want: 3},
		{name: "all", found: []bool{true, true, true}, want: 3},
		{name: "gap-inside", found: []bool{true, false, true, false}, want: 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reachedSpan(tc.found); got != tc.want {
				t.Fatalf("reachedSpan(%v) = %d, want %d", tc.found, got, tc.want)
			}
		})
	}
}
