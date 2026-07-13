package sim

import "testing"

// TestSearchAlgorithm_ShapedGraphClean drives searchAlgorithmViolations on a
// graph with deliberate structure — a directed triangle (a genuine triangle in
// the undirected view) plus a separate weakly-connected component — so the
// triangle, WCCParallel, BiBFS, and buffer-reuse SSSP checks all run on
// non-trivial input and must agree with their references (0 violations).
func TestSearchAlgorithm_ShapedGraphClean(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan", "Grace", "Edsger", "Donald", "Barbara"}
	edges := [][2]string{
		// Triangle {Ada, Alan, Grace}: undirected view has all three sides.
		{"Ada", "Alan"}, {"Alan", "Grace"}, {"Grace", "Ada"},
		// Second component: a chain Edsger -> Donald -> Barbara.
		{"Edsger", "Donald"}, {"Donald", "Barbara"},
	}
	g := oracleNameGraph(buildSearchOracle(names, edges))
	if vs := searchAlgorithmViolations(1, g); len(vs) != 0 {
		t.Errorf("searchAlgorithmViolations on shaped graph = %d violation(s), want 0:", len(vs))
		for _, v := range vs {
			t.Errorf("  %s", v)
		}
	}
}

// TestNaiveTriangleCounts_Known anchors the triangle reference on hand-checked
// undirected shapes: a single triangle {0,1,2} has one triangle, and K4 has four
// (each vertex participates in three).
func TestNaiveTriangleCounts_Known(t *testing.T) {
	t.Parallel()
	tri := []mstEdge{{u: 0, v: 1}, {u: 1, v: 2}, {u: 0, v: 2}}
	total, per := naiveTriangleCounts(3, tri)
	if total != 1 {
		t.Errorf("triangle total = %d, want 1", total)
	}
	for i, c := range per {
		if c != 1 {
			t.Errorf("triangle per-node[%d] = %d, want 1", i, c)
		}
	}

	k4 := []mstEdge{{u: 0, v: 1}, {u: 0, v: 2}, {u: 0, v: 3}, {u: 1, v: 2}, {u: 1, v: 3}, {u: 2, v: 3}}
	total, per = naiveTriangleCounts(4, k4)
	if total != 4 {
		t.Errorf("K4 triangle total = %d, want 4", total)
	}
	for i, c := range per {
		if c != 3 {
			t.Errorf("K4 per-node[%d] = %d, want 3", i, c)
		}
	}
}

// TestNaiveBFSDistance_Known anchors the hop-distance reference on a directed
// chain: from the first node, distances increase by one per hop, and a node in a
// disjoint tail is unreachable (-1).
func TestNaiveBFSDistance_Known(t *testing.T) {
	t.Parallel()
	// Names sort to A,B,C,Z -> dense ids 0,1,2,3. Edges A->B->C; Z isolated tail.
	names := []string{"A", "B", "C", "Z"}
	edges := [][2]string{{"A", "B"}, {"B", "C"}}
	g := oracleNameGraph(buildSearchOracle(names, edges))
	dist := g.naiveBFSDistance(0) // from A
	want := []int{0, 1, 2, -1}
	for i := range want {
		if dist[i] != want[i] {
			t.Errorf("naiveBFSDistance(A)[%d] = %d, want %d (dist=%v)", i, dist[i], want[i], dist)
		}
	}
}
