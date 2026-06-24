package sim

import "testing"

// weightedTriangle is A->B, B->C, A->C: a direct edge and a two-hop path to C,
// so the shortest distance to C exercises path choice under weights.
func weightedTriangle() *nameGraph {
	return oracleNameGraph(buildSearchOracle(
		[]string{"A", "B", "C"},
		[][2]string{{"A", "B"}, {"B", "C"}, {"A", "C"}},
	))
}

// TestEdgeWeight_DeterministicAndBounded covers the synthetic weight function.
func TestEdgeWeight_DeterministicAndBounded(t *testing.T) {
	t.Parallel()
	if edgeWeight("Ada", "Alan") != edgeWeight("Ada", "Alan") {
		t.Fatal("edgeWeight is not deterministic")
	}
	for _, p := range [][2]string{{"A", "B"}, {"x", "y"}, {"Grace", "Ada"}} {
		if w := edgeWeight(p[0], p[1]); w < 1 || w > 16 {
			t.Fatalf("edgeWeight(%q,%q)=%v outside [1,16]", p[0], p[1], w)
		}
	}
}

// TestNaiveSSSP_PicksShortestPath verifies the reference chooses the cheaper of
// the direct and two-hop routes, computed from the actual synthetic weights.
func TestNaiveSSSP_PicksShortestPath(t *testing.T) {
	t.Parallel()
	g := weightedTriangle()
	dist, reach := g.naiveSSSP(g.idx["A"])
	if !reach[g.idx["C"]] {
		t.Fatal("C must be reachable from A")
	}
	if dist[g.idx["A"]] != 0 {
		t.Fatalf("dist(A,A)=%v, want 0", dist[g.idx["A"]])
	}
	direct := edgeWeight("A", "C")
	viaB := edgeWeight("A", "B") + edgeWeight("B", "C")
	want := direct
	if viaB < want {
		want = viaB
	}
	if dist[g.idx["C"]] != want {
		t.Fatalf("dist(A,C)=%v, want min(direct=%v, viaB=%v)=%v", dist[g.idx["C"]], direct, viaB, want)
	}
}

// TestSSSPChecks_CleanOnFixtures asserts the whole SSSP/APSP battery (search vs
// naive reference, and serial vs parallel APSP) agrees on several shapes.
func TestSSSPChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*nameGraph{
		"triangle": weightedTriangle(),
		"dag":      dagFixture(),
		"cyclic":   cyclicFixture(),
	} {
		if v := ssspViolations(1, g); len(v) != 0 {
			t.Fatalf("SSSP battery on %s fixture: %v", name, v)
		}
	}
}

// TestSSSP_UnreachableHandled asserts unreachable targets are handled: a sink and
// an isolated node yield matching unreachability across every algorithm.
func TestSSSP_UnreachableHandled(t *testing.T) {
	t.Parallel()
	// A->B only; C is isolated.
	g := oracleNameGraph(buildSearchOracle([]string{"A", "B", "C"}, [][2]string{{"A", "B"}}))
	if v := ssspViolations(1, g); len(v) != 0 {
		t.Fatalf("SSSP battery with unreachable nodes: %v", v)
	}
	_, reach := g.naiveSSSP(g.idx["A"])
	if reach[g.idx["C"]] {
		t.Fatal("C must be unreachable from A")
	}
}
