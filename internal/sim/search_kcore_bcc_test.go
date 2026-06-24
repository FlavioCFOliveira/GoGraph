package sim

import (
	"slices"
	"testing"
)

// ngFromEdges builds a name-keyed graph from directed edges (the undirected
// checks symmetrise it).
func ngFromEdges(names []string, edges [][2]string) *nameGraph {
	return oracleNameGraph(buildSearchOracle(names, edges))
}

// TestKCore_AgreesAndReferenceCorrect checks KCore against the definition-based
// reference on shapes with known coreness, and validates the reference itself.
func TestKCore_AgreesAndReferenceCorrect(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		g        *nameGraph
		wantCore int // expected coreness shared by all nodes of these regular shapes
	}{
		"path":     {ngFromEdges([]string{"A", "B", "C", "D"}, [][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}}), 1},
		"star":     {ngFromEdges([]string{"S", "A", "B", "C"}, [][2]string{{"S", "A"}, {"S", "B"}, {"S", "C"}}), 1},
		"triangle": {ngFromEdges([]string{"A", "B", "C"}, [][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}}), 2},
		"k4": {ngFromEdges([]string{"A", "B", "C", "D"}, [][2]string{
			{"A", "B"}, {"A", "C"}, {"A", "D"}, {"B", "C"}, {"B", "D"}, {"C", "D"},
		}), 3},
	}
	for name, tc := range cases {
		// The library must agree with the reference.
		if v := kcoreViolations(1, tc.g); len(v) != 0 {
			t.Fatalf("%s: KCore disagrees with reference: %v", name, v)
		}
		// The reference must report the known coreness for every node.
		core := naiveCoreness(len(tc.g.names), undirectedAdj(len(tc.g.names), undirectedEdges(tc.g)))
		for i, c := range core {
			if c != tc.wantCore {
				t.Fatalf("%s: coreness[%s]=%d, want %d", name, tc.g.names[i], c, tc.wantCore)
			}
		}
	}
}

// TestBCC_AgreesAndReferenceCorrect checks articulation points and bridges on
// two triangles joined by a single bridge edge, and validates the references.
func TestBCC_AgreesAndReferenceCorrect(t *testing.T) {
	t.Parallel()
	// Triangles {A,B,C} and {D,E,F}; bridge C-D.
	g := ngFromEdges(
		[]string{"A", "B", "C", "D", "E", "F"},
		[][2]string{
			{"A", "B"}, {"B", "C"}, {"C", "A"},
			{"D", "E"}, {"E", "F"}, {"F", "D"},
			{"C", "D"},
		},
	)
	if v := bccViolations(1, g); len(v) != 0 {
		t.Fatalf("BCC disagrees with reference: %v", v)
	}
	adj := undirectedAdj(len(g.names), undirectedEdges(g))
	// C and D are the articulation points; C-D is the only bridge.
	wantArts := []int{g.idx["C"], g.idx["D"]}
	slices.Sort(wantArts)
	if got := naiveArticulation(len(g.names), adj, g.incidentMask()); !slices.Equal(got, wantArts) {
		t.Fatalf("articulation points got %v want %v", got, wantArts)
	}
	bridges := naiveBridges(len(g.names), adj)
	if len(bridges) != 1 {
		t.Fatalf("expected exactly one bridge (C-D), got %v", bridges)
	}
}

// TestBCC_NoArticulationInCycle confirms a single cycle has no articulation
// points and no bridges (every edge is on a cycle).
func TestBCC_NoArticulationInCycle(t *testing.T) {
	t.Parallel()
	g := ngFromEdges([]string{"A", "B", "C", "D"}, [][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}, {"D", "A"}})
	if v := bccViolations(1, g); len(v) != 0 {
		t.Fatalf("BCC on a cycle: %v", v)
	}
	adj := undirectedAdj(len(g.names), undirectedEdges(g))
	if got := naiveArticulation(len(g.names), adj, g.incidentMask()); len(got) != 0 {
		t.Fatalf("a cycle has no articulation points, got %v", got)
	}
	if got := naiveBridges(len(g.names), adj); len(got) != 0 {
		t.Fatalf("a cycle has no bridges, got %v", got)
	}
}
