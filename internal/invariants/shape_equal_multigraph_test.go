package invariants_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/invariants"
)

// multigraphLPG builds a directed multigraph *lpg.Graph[int, struct{}]
// from an edge list where each entry [u, v] represents one directed
// edge (parallel entries produce parallel edges).
func multigraphLPG(nodes []int, edges [][2]int) *lpg.Graph[int, struct{}] {
	g := lpg.New[int, struct{}](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range nodes {
		_ = g.AddNode(n)
	}
	for _, e := range edges {
		_ = g.AddEdge(e[0], e[1], struct{}{})
	}
	return g
}

// TestAssertShapeEqual_Multigraph_SameMultiplicity verifies that two
// multigraphs with identical per-(u,v) edge counts are considered equal.
func TestAssertShapeEqual_Multigraph_SameMultiplicity(t *testing.T) {
	// a: u→v×2, u→w×1  — same in b
	nodes := []int{1, 2, 3}
	edges := [][2]int{{1, 2}, {1, 2}, {1, 3}}
	a := multigraphLPG(nodes, edges)
	b := multigraphLPG(nodes, edges)

	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if failed(tb) {
		t.Errorf("identical multigraphs: unexpected failure: %v", tb.failures)
	}
}

// TestAssertShapeEqual_Multigraph_DifferentMultiplicity is the gate test
// for #1393. Before the fix, AssertShapeEqual passed vacuously for
// multigraphs with same Order+Size but different per-(u,v) multiplicities.
// After the fix it must report an error.
//
// Graph a: 1→2 ×2, 1→3 ×1  (Order=3, Size=3)
// Graph b: 1→2 ×1, 1→3 ×2  (Order=3, Size=3)
func TestAssertShapeEqual_Multigraph_DifferentMultiplicity(t *testing.T) {
	nodes := []int{1, 2, 3}
	edgesA := [][2]int{{1, 2}, {1, 2}, {1, 3}} // 1→2×2, 1→3×1
	edgesB := [][2]int{{1, 2}, {1, 3}, {1, 3}} // 1→2×1, 1→3×2

	a := multigraphLPG(nodes, edgesA)
	b := multigraphLPG(nodes, edgesB)

	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if passed(tb) {
		t.Error("multigraphs with different multiplicities: expected failure, got none")
	}
}

// TestAssertShapeEqual_Multigraph_MissingEdgePair verifies that a pair
// where b has an extra edge pair (different topology, same size via
// a compensating miss) is also detected.
func TestAssertShapeEqual_Multigraph_MissingEdgePair(t *testing.T) {
	nodes := []int{1, 2, 3}
	edgesA := [][2]int{{1, 2}, {1, 3}} // 1→2×1, 1→3×1 (Size=2)
	edgesB := [][2]int{{1, 2}, {1, 2}} // 1→2×2 (Size=2, but 1→3 absent)

	a := multigraphLPG(nodes, edgesA)
	b := multigraphLPG(nodes, edgesB)

	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if passed(tb) {
		t.Error("multigraphs with different edge pairs: expected failure, got none")
	}
}
