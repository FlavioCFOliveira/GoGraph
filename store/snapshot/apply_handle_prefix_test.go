package snapshot

// apply_handle_prefix_test.go — recovery-path regression for the fast-path
// handle-column growth bug in graph/adjlist.upsertEdgeLocked.
//
// This is the path that surfaced the bug in practice: ApplyCSRToGraph replays a
// node's edges in slot order, calling AddEdge for handle-less slots and
// AddEdgeHIfAbsent for handle-bearing ones. A node carrying a handle-less
// prefix followed by a handle-bearing parallel edge therefore re-creates, on
// the fresh graph, the exact insertion sequence that drove the fast path into a
// short handle column. Before the fix this panicked during recovery — making
// any such snapshot (the typical state of a pre-regression store) impossible to
// open. The test reconstructs that snapshot via CSR and asserts it reopens.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestApplyCSRToGraph_HandlelessPrefixThenHandle reconstructs a hub node whose
// edges are a handle-less prefix followed by a handle-bearing parallel edge,
// then applies the CSR to a fresh graph. Recovery must not panic and the edges
// must be present.
func TestApplyCSRToGraph_HandlelessPrefixThenHandle(t *testing.T) {
	t.Parallel()

	// Build the source graph: five handle-less edges (so the hub's neighbour
	// array grows with spare capacity and the handle column stays nil) and then
	// one handle-bearing edge on the fast path. The CSR built from this graph
	// carries a handle column whose leading slots are 0 and last slot is the
	// stable handle — exactly the readback ApplyCSRToGraph dispatches on.
	orig := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	const handleless = 5
	for i := 0; i < handleless; i++ {
		dst := string(rune('a' + i))
		if err := orig.AddEdge("hub", dst, 1); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}
	if _, err := orig.AddEdgeH("hub", "z", 1); err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	cs := csr.BuildFromAdjList(orig.AdjList())
	if cs.HandlesSlice() == nil {
		t.Fatal("source CSR has no handle column; test cannot exercise the mixed path")
	}

	// Restore the mapper, then apply the CSR to a fresh graph — the recovery
	// replay that panicked before the fix.
	fresh := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	var pairs []MapperPair
	orig.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		pairs = append(pairs, MapperPair{ID: id, Key: k})
		return true
	})
	if err := ApplyMapperToGraph(fresh, MapperReadback{Pairs: pairs}); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}
	rb := &CSRReadback{
		Vertices: cs.VerticesSlice(),
		Edges:    cs.EdgesSlice(),
		Handles:  cs.HandlesSlice(),
	}
	if err := ApplyCSRToGraph(fresh, rb); err != nil {
		t.Fatalf("ApplyCSRToGraph (handle-less prefix then handle): %v", err)
	}

	for i := 0; i < handleless; i++ {
		dst := string(rune('a' + i))
		if !fresh.AdjList().HasEdge("hub", dst) {
			t.Errorf("hub→%s missing after recovery", dst)
		}
	}
	if !fresh.AdjList().HasEdge("hub", "z") {
		t.Error("hub→z (handle-bearing) missing after recovery")
	}
}
