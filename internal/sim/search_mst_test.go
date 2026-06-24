package sim

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestMSTChecks_CleanOnFixtures asserts the MST battery (Kruskal vs an
// independent reference, and Prim per source) agrees on several shapes.
func TestMSTChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*nameGraph{
		"triangle": weightedTriangle(),
		"dag":      dagFixture(),
		"cyclic":   cyclicFixture(),
	} {
		if v := mstViolations(1, g); len(v) != 0 {
			t.Fatalf("MST battery on %s fixture: %v", name, v)
		}
	}
}

// TestNaiveKruskal_PicksTwoSmallestOnTriangle verifies the reference MST total on
// a triangle equals the sum of the two cheapest edges, using the actual
// synthetic symmetric weights.
func TestNaiveKruskal_PicksTwoSmallestOnTriangle(t *testing.T) {
	t.Parallel()
	g := oracleNameGraph(buildSearchOracle(
		[]string{"A", "B", "C"},
		[][2]string{{"A", "B"}, {"B", "C"}, {"A", "C"}},
	))
	edges := undirectedEdges(g)
	if len(edges) != 3 {
		t.Fatalf("expected 3 undirected edges, got %d", len(edges))
	}
	ws := []float64{symWeight("A", "B"), symWeight("B", "C"), symWeight("A", "C")}
	sort.Float64s(ws)
	want := ws[0] + ws[1]
	if got := naiveKruskalTotal(3, edges); got != want {
		t.Fatalf("triangle MST total got %v want %v (two smallest of %v)", got, want, ws)
	}
}

// TestMST_DisconnectedForestValidity asserts the spanning forest of a
// two-component graph has exactly one edge per component.
func TestMST_DisconnectedForestValidity(t *testing.T) {
	t.Parallel()
	// {A,B} and {C,D} are separate components.
	g := oracleNameGraph(buildSearchOracle(
		[]string{"A", "B", "C", "D"},
		[][2]string{{"A", "B"}, {"C", "D"}},
	))
	if v := mstViolations(1, g); len(v) != 0 {
		t.Fatalf("MST forest battery: %v", v)
	}
	kEdges, _, err := search.KruskalMST(undirectedWeightedCSR(g, undirectedEdges(g)))
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	if len(kEdges) != 2 {
		t.Fatalf("spanning forest of 2 components: got %d edges, want 2", len(kEdges))
	}
}
