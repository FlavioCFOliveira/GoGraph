package centrality

import (
	"math"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestWeightedBetweenness_PathUnitWeights asserts the weighted
// variant produces the same centrality as the unweighted Brandes on
// a graph with unit weights — the betweenness equality is the
// definition of "consistent with unweighted" used in the literature.
func TestWeightedBetweenness_PathUnitWeights(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		a.AddEdge(i, i+1, 1.0)
	}
	c := csr.BuildFromAdjList(a)
	cb := WeightedBetweenness(c)
	// Unweighted equivalent.
	au := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		au.AddEdge(i, i+1, struct{}{})
	}
	cu := csr.BuildFromAdjList(au)
	cbu := Betweenness(cu)
	for i := 0; i < 5; i++ {
		id, _ := a.Mapper().Lookup(i)
		idu, _ := au.Mapper().Lookup(i)
		if math.Abs(cb[id]-cbu[idu]) > 1e-9 {
			t.Fatalf("Weighted vs Unweighted disagreement at %d: %f vs %f", i, cb[id], cbu[idu])
		}
	}
}

// TestWeightedBetweenness_WeightSensitive verifies the weights
// actually influence the result: the same topology with a heavy
// detour weight changes which paths are shortest.
func TestWeightedBetweenness_WeightSensitive(t *testing.T) {
	t.Parallel()
	// Triangle 0-1-2: edges (0,1)=1, (1,2)=1, (0,2)=10. The (0,2)
	// shortest path now goes 0->1->2 (cost 2 < 10), so vertex 1
	// has nonzero betweenness; without the heavy detour every pair
	// is directly connected so the centre vertex would have zero.
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	a.AddEdge(0, 1, 1.0)
	a.AddEdge(1, 2, 1.0)
	a.AddEdge(0, 2, 10.0)
	c := csr.BuildFromAdjList(a)
	cb := WeightedBetweenness(c)
	id1, _ := a.Mapper().Lookup(1)
	if cb[id1] <= 0 {
		t.Fatalf("centre vertex betweenness = %f, want > 0 (heavy detour bypassed via 0-1-2)", cb[id1])
	}
}
