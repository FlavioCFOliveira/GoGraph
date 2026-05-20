package centrality

import (
	"errors"
	"math"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
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
	cb, err := WeightedBetweenness(c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
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
	cb, err := WeightedBetweenness(c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	id1, _ := a.Mapper().Lookup(1)
	if cb[id1] <= 0 {
		t.Fatalf("centre vertex betweenness = %f, want > 0 (heavy detour bypassed via 0-1-2)", cb[id1])
	}
}

// TestWeightedBetweenness_NaN asserts that a NaN edge weight surfaces
// ErrInvalidInput rather than silently corrupting sigma/dist.
func TestWeightedBetweenness_NaN(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	a.AddEdge(0, 1, 1.0)
	a.AddEdge(1, 2, math.NaN())
	c := csr.BuildFromAdjList(a)
	got, err := WeightedBetweenness(c)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v, want ErrInvalidInput", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil on invalid input", got)
	}
}

// TestWeightedBetweenness_Inf asserts that +/-Inf edge weights also
// surface ErrInvalidInput.
func TestWeightedBetweenness_Inf(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	a.AddEdge(0, 1, 1.0)
	a.AddEdge(1, 2, math.Inf(1))
	c := csr.BuildFromAdjList(a)
	if _, err := WeightedBetweenness(c); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("+Inf err=%v, want ErrInvalidInput", err)
	}
}

// TestWeightedBetweenness_Negative asserts that a negative edge
// weight surfaces search.ErrNegativeWeight; weighted Brandes uses
// Dijkstra internally, which is undefined on negative arcs.
func TestWeightedBetweenness_Negative(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	a.AddEdge(0, 1, 1.0)
	a.AddEdge(1, 2, -2.0)
	c := csr.BuildFromAdjList(a)
	_, err := WeightedBetweenness(c)
	if !errors.Is(err, search.ErrNegativeWeight) {
		t.Fatalf("err=%v, want search.ErrNegativeWeight", err)
	}
}
