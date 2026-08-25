package sim

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

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

// TestBiDijkstraOnParity_DetectsDivergence proves the parity oracle added for
// [search.BidirectionalDijkstraOn] can actually fail, in each of the three
// directions it claims to cover, and that it stays silent when the two entry
// points agree. Without this the parity assertion could be a check that never
// speaks — the failure mode a green suite cannot distinguish from a correct
// engine.
func TestBiDijkstraOnParity_DetectsDivergence(t *testing.T) {
	t.Parallel()
	g := weightedTriangle()
	a, c := g.idx["A"], g.idx["C"]

	// Agreement: identical success and identical cost must flag nothing.
	if vs := bidijkstraOnParity(0, g, a, c, 7, nil, 7, nil); len(vs) != 0 {
		t.Fatalf("agreeing outcomes must flag nothing, got %v", vs)
	}
	// Agreement: both failing with ErrNoPath must flag nothing.
	if vs := bidijkstraOnParity(0, g, a, c, 0, search.ErrNoPath, 0, search.ErrNoPath); len(vs) != 0 {
		t.Fatalf("both-ErrNoPath must flag nothing, got %v", vs)
	}

	// Direction 1: one succeeded, the other did not.
	assertOneBiDijkstraOnParityViolation(t, "success/failure disagreement",
		bidijkstraOnParity(0, g, a, c, 7, nil, 0, search.ErrNoPath))
	assertOneBiDijkstraOnParityViolation(t, "failure/success disagreement",
		bidijkstraOnParity(0, g, a, c, 0, search.ErrNoPath, 7, nil))

	// Direction 2: both failed, but only one of them with ErrNoPath.
	assertOneBiDijkstraOnParityViolation(t, "ErrNoPath classification disagreement",
		bidijkstraOnParity(0, g, a, c, 0, search.ErrNoPath, 0, search.ErrInvalidInput))

	// Direction 3: both succeeded with different costs. One ULP is enough,
	// because the predicate is exact equality and the true costs are integers.
	assertOneBiDijkstraOnParityViolation(t, "cost disagreement (1 unit)",
		bidijkstraOnParity(0, g, a, c, 7, nil, 8, nil))
	assertOneBiDijkstraOnParityViolation(t, "cost disagreement (1 ULP)",
		bidijkstraOnParity(0, g, a, c, 7, nil, math.Nextafter(7, 8), nil))
}

// assertOneBiDijkstraOnParityViolation asserts exactly one SEARCH_DIVERGENCE
// tagged for the hoisted-reverse variant was produced.
func assertOneBiDijkstraOnParityViolation(t *testing.T, what string, vs []Violation) {
	t.Helper()
	if len(vs) != 1 {
		t.Fatalf("%s must flag exactly one divergence, got %d (%v)", what, len(vs), vs)
	}
	if vs[0].Kind != ViolationSearchDivergence {
		t.Fatalf("%s: divergence kind = %q, want %q", what, vs[0].Kind, ViolationSearchDivergence)
	}
	if vs[0].Op != "search:BiDijkstraOn" {
		t.Fatalf("%s: divergence Op = %q, want %q", what, vs[0].Op, "search:BiDijkstraOn")
	}
}

// TestBiDijkstraOn_DrivenAndComparedToReference brackets the new arm end to end
// on a real fixture with a genuinely hoisted reverse CSR: the call must agree
// with the naive reference and with [search.BidirectionalDijkstra], and the
// reference comparison must flag a deliberately-wrong reference. The reachable
// assertion is the non-vacuity guard — without it the whole check could pass by
// only ever taking the unreachable branch.
func TestBiDijkstraOn_DrivenAndComparedToReference(t *testing.T) {
	t.Parallel()
	g := weightedTriangle()
	c := g.toWeightedCSR()
	rev := c.BuildReverse()
	src, dst := g.idx["A"], g.idx["C"]
	dist, reach := g.naiveSSSP(src)

	if !reach[dst] {
		t.Fatal("fixture must make C reachable from A, or the cost branch is never taken")
	}

	_, costOn, errOn := search.BidirectionalDijkstraOn(c, rev, graph.NodeID(src), graph.NodeID(dst))
	if errOn != nil {
		t.Fatalf("BidirectionalDijkstraOn on a reachable pair returned err=%v", errOn)
	}
	if vs := comparePointToPoint(0, g, "BiDijkstraOn", src, dst, costOn, errOn, dist, reach); len(vs) != 0 {
		t.Fatalf("BidirectionalDijkstraOn disagreed with the naive reference: %v", vs)
	}

	// The hoisted-reverse call must match the self-building one exactly.
	_, cost, err := search.BidirectionalDijkstra(c, graph.NodeID(src), graph.NodeID(dst))
	if vs := bidijkstraOnParity(0, g, src, dst, cost, err, costOn, errOn); len(vs) != 0 {
		t.Fatalf("hoisted-reverse and self-building variants disagreed: %v", vs)
	}

	// Injected divergence: a reference one unit off must be flagged, proving
	// the reference comparison for this label is not vacuous either.
	wrong := make([]float64, len(dist))
	copy(wrong, dist)
	wrong[dst]++
	if vs := comparePointToPoint(0, g, "BiDijkstraOn", src, dst, costOn, errOn, wrong, reach); len(vs) != 1 {
		t.Fatalf("a reference off by one must flag exactly one divergence, got %d (%v)", len(vs), vs)
	}

	// And an unreachable target must be classified as such by both variants.
	iso := oracleNameGraph(buildSearchOracle([]string{"A", "B", "C"}, [][2]string{{"A", "B"}}))
	ic := iso.toWeightedCSR()
	irev := ic.BuildReverse()
	_, _, isoErr := search.BidirectionalDijkstraOn(ic, irev, graph.NodeID(iso.idx["A"]), graph.NodeID(iso.idx["C"]))
	if !errors.Is(isoErr, search.ErrNoPath) {
		t.Fatalf("BidirectionalDijkstraOn to an unreachable target: err=%v, want ErrNoPath", isoErr)
	}
}

// TestPointToPointViolations_DrivesAllThreeVariants proves the hoisted-reverse
// variant is actually REACHED from [pointToPointViolations], not merely present
// in the file. A green battery cannot tell "BidirectionalDijkstraOn ran and
// agreed" from "the call was never added"; both are silence.
//
// So the reference is corrupted for one reachable target — every point-to-point
// entry point then has something to disagree with — and the set of Ops that come
// back must name all three. Remove the BidirectionalDijkstraOn call and the
// "search:BiDijkstraOn" label disappears, failing this test.
//
// The parity check must stay silent throughout: it compares the two engine
// results against each other, so corrupting the reference cannot move it.
func TestPointToPointViolations_DrivesAllThreeVariants(t *testing.T) {
	t.Parallel()
	g := weightedTriangle()
	c := g.toWeightedCSR()
	rev := c.BuildReverse()
	src, dst := g.idx["A"], g.idx["C"]
	dist, reach := g.naiveSSSP(src)
	if !reach[dst] {
		t.Fatal("C must be reachable from A for the cost branch to be exercised")
	}

	// Control: the true reference must produce nothing at all.
	if vs := pointToPointViolations(0, g, c, rev, src, dist, reach); len(vs) != 0 {
		t.Fatalf("point-to-point battery on a clean fixture flagged %d violation(s): %v", len(vs), vs)
	}

	wrong := make([]float64, len(dist))
	copy(wrong, dist)
	wrong[dst]++

	got := map[string]int{}
	for _, v := range pointToPointViolations(0, g, c, rev, src, wrong, reach) {
		if v.Kind != ViolationSearchDivergence {
			t.Errorf("violation kind = %q, want %q (%s)", v.Kind, ViolationSearchDivergence, v.Op)
		}
		got[v.Op]++
	}
	for _, op := range []string{"search:BiDijkstra", "search:BiDijkstraOn", "search:AStar"} {
		if got[op] != 1 {
			t.Errorf("Op %q fired %d time(s), want exactly 1 — it is not wired into pointToPointViolations (fired: %v)", op, got[op], got)
		}
	}
	if len(got) != 3 {
		t.Errorf("point-to-point battery produced Ops %v, want exactly the three point-to-point entry points "+
			"(a fourth means the parity check fired on a reference-only perturbation, which it must not)", got)
	}
}
