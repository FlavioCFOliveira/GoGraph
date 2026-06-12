package centrality

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestWeightedBetweenness_PathUnitWeights asserts the weighted
// variant produces the same centrality as the unweighted Brandes on
// a graph with unit weights — the betweenness equality is the
// definition of "consistent with unweighted" used in the literature.
func TestWeightedBetweenness_PathUnitWeights(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	cb, err := WeightedBetweenness(c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Unweighted equivalent.
	au := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := au.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
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
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 2, 10.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
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
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, math.NaN()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
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
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, math.Inf(1)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if _, err := WeightedBetweenness(c); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("+Inf err=%v, want ErrInvalidInput", err)
	}
}

// TestWeightedBetweenness_Negative asserts that a negative edge
// weight surfaces ErrNonPositiveWeight; weighted Brandes uses
// Dijkstra internally, which is undefined on non-positive arcs.
func TestWeightedBetweenness_Negative(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, -2.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	_, err := WeightedBetweenness(c)
	if !errors.Is(err, ErrNonPositiveWeight) {
		t.Fatalf("err=%v, want ErrNonPositiveWeight", err)
	}
}

// TestWeightedBetweenness_ZeroWeight is the gate test for the zero-weight
// fix. Before the fix, WeightedBetweenness accepted zero-weight edges and
// returned silently wrong centrality values (σ inconsistency); after the
// fix it must return ErrNonPositiveWeight.
//
// The graph is a symmetric diamond:
//
//	0 --0.0-- 1
//	0 --1.0-- 2
//	1 --1.0-- 3
//	2 --1.0-- 3
//
// Nodes 0 and 1 are connected by a zero-weight edge; via Dijkstra both
// are settled at the same distance from any source, so node 1's σ can
// be consumed before its predecessor 0 has contributed — corrupting
// σ silently.
func TestWeightedBetweenness_ZeroWeight(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	edges := [][3]float64{
		{0, 1, 0.0}, // zero-weight edge — the trigger
		{0, 2, 1.0},
		{1, 3, 1.0},
		{2, 3, 1.0},
	}
	for _, e := range edges {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge(%v->%v, %v): %v", e[0], e[1], e[2], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	got, err := WeightedBetweenness(c)
	if !errors.Is(err, ErrNonPositiveWeight) {
		t.Fatalf("zero-weight edge: err=%v, want ErrNonPositiveWeight; got centrality=%v", err, got)
	}
	if got != nil {
		t.Fatalf("zero-weight edge: got non-nil result %v on error path", got)
	}
}

// TestWeightedBetweenness_StrictlyPositive verifies that a graph with all
// strictly positive weights returns correct centrality values and no error.
// The linear path 0-1-2-3-4 with unit weights has an analytically known
// betweenness: interior nodes have higher centrality than endpoints.
func TestWeightedBetweenness_StrictlyPositive(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, 0.5); err != nil { // positive, non-unit
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	cb, err := WeightedBetweenness(c)
	if err != nil {
		t.Fatalf("unexpected error on valid graph: %v", err)
	}
	// On a path graph n=5, interior node 2 (the centre) has the highest
	// betweenness; endpoints 0 and 4 have zero betweenness.
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	id4, _ := a.Mapper().Lookup(4)
	if cb[id0] != 0 {
		t.Errorf("endpoint 0 betweenness = %f, want 0", cb[id0])
	}
	if cb[id4] != 0 {
		t.Errorf("endpoint 4 betweenness = %f, want 0", cb[id4])
	}
	if cb[id2] <= 0 {
		t.Errorf("centre 2 betweenness = %f, want > 0", cb[id2])
	}
}
