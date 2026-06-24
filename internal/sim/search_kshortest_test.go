package sim

import (
	"context"
	"reflect"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// kshortestHandGraph is the small, hand-checked fixture used across the
// reference- and comparison-correctness tests. Nodes 0..3 (src=0, dst=3):
//
//	0 -1-> 1   0 -4-> 2   1 -1-> 2   1 -5-> 3   2 -1-> 3
//
// The three simple 0->3 paths and their costs, verifiable by inspection:
//
//	0->1->2->3 = 1+1+1 = 3
//	0->2->3    = 4+1   = 5
//	0->1->3    = 1+5   = 6
//
// so the ascending cost list is [3, 5, 6].
func kshortestHandGraph() *kshortestGraph {
	g := &kshortestGraph{n: 4, adj: make([][]kshortestEdge, 4)}
	g.adj[0] = []kshortestEdge{{to: 1, weight: 1}, {to: 2, weight: 4}}
	g.adj[1] = []kshortestEdge{{to: 2, weight: 1}, {to: 3, weight: 5}}
	g.adj[2] = []kshortestEdge{{to: 3, weight: 1}}
	return g
}

// kshortestHandCosts is the ascending cost list of every simple 0->3 path in
// kshortestHandGraph, established by inspection above.
var kshortestHandCosts = []int{3, 5, 6}

// --- Clean-on-fixtures: the whole battery is silent on every seed-derived
// instance across a range of ticks (and reproducible, since each fixture is a
// pure function of the tick). ---

// TestKShortestChecks_CleanOnFixtures asserts the K-shortest battery (Yen,
// bounded loopless, and the Eppstein alias, each vs the brute-force reference +
// per-path validity) flags nothing on every generated fixture across many ticks.
func TestKShortestChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for tick := int64(0); tick < 300; tick++ {
		if v := kshortestViolations(tick); len(v) != 0 {
			t.Fatalf("k-shortest battery flagged a clean instance at tick=%d:\n%v", tick, v)
		}
	}
}

// TestKShortestViolations_Deterministic asserts the same tick yields the same
// (empty) result on repeat, i.e. the check draws only from the seed.
func TestKShortestViolations_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 1, 7, 42, 1000} {
		a := kshortestViolations(tick)
		b := kshortestViolations(tick)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("tick=%d nondeterministic: %v vs %v", tick, a, b)
		}
	}
}

// --- Reference-correctness: prove OUR brute-force enumerator is right on
// hand-checked instances, before trusting it to judge the library. ---

// TestBruteForceKShortest_HandChecked verifies the brute-force enumerator on
// instances whose k-shortest costs are obvious by inspection, including the
// canonical hand graph and a couple of edge shapes.
func TestBruteForceKShortest_HandChecked(t *testing.T) {
	t.Parallel()

	hand := kshortestHandGraph()
	tests := []struct {
		name string
		g    *kshortestGraph
		k    int
		want []int
	}{
		{"hand-k1", hand, 1, []int{3}},
		{"hand-k2", hand, 2, []int{3, 5}},
		{"hand-k3", hand, 3, []int{3, 5, 6}},
		{"hand-k5-only-3-paths", hand, 5, []int{3, 5, 6}}, // k exceeds path count

		{
			// Single path 0->1->2 (n=3, src=0, dst=2): cost 2+3=5.
			name: "single-path",
			g: &kshortestGraph{n: 3, adj: [][]kshortestEdge{
				{{to: 1, weight: 2}},
				{{to: 2, weight: 3}},
				nil,
			}},
			k: 3, want: []int{5},
		},
		{
			// Two equal-cost parallel routes (a classic non-unique-path case):
			// 0->1->3 = 1+1 = 2 and 0->2->3 = 1+1 = 2. The multiset is [2,2].
			name: "tie-costs",
			g: &kshortestGraph{n: 4, adj: [][]kshortestEdge{
				{{to: 1, weight: 1}, {to: 2, weight: 1}},
				{{to: 3, weight: 1}},
				{{to: 3, weight: 1}},
				nil,
			}},
			k: 4, want: []int{2, 2},
		},
		{
			// dst unreachable from src: no paths.
			name: "unreachable",
			g: &kshortestGraph{n: 3, adj: [][]kshortestEdge{
				{{to: 1, weight: 1}},
				nil, // 1 has no edge to 2
				nil,
			}},
			k: 3, want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := kshortestRefSortedCosts(tt.g, 0, tt.g.n-1, tt.k)
			if !kshortestIntsEqual(got, tt.want) {
				t.Fatalf("bruteForce(%s)=%v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestBruteForceKShortest_AgreesWithYen pins the brute-force reference to
// YenKShortest on the hand graph — the production cross-check's positive
// control. Costs must match exactly.
func TestBruteForceKShortest_AgreesWithYen(t *testing.T) {
	t.Parallel()
	g := kshortestHandGraph()
	c := kshortestBuildCSR(g)
	for k := 1; k <= 5; k++ {
		ref := kshortestRefSortedCosts(g, 0, g.n-1, k)
		yen := kshortestCosts(search.YenKShortest(c, 0, graph.NodeID(g.n-1), k))
		if !kshortestIntsEqual(yen, ref) {
			t.Fatalf("k=%d: YenKShortest costs %v, brute-force %v", k, yen, ref)
		}
	}
}

// TestBruteForceKShortest_AgreesWithLoopless pins the brute-force reference to
// the bounded best-first loopless entry on the hand graph (the budget is large
// enough that it never truncates here), and to the Eppstein alias.
func TestBruteForceKShortest_AgreesWithLoopless(t *testing.T) {
	t.Parallel()
	g := kshortestHandGraph()
	c := kshortestBuildCSR(g)
	for k := 1; k <= 5; k++ {
		ref := kshortestRefSortedCosts(g, 0, g.n-1, k)

		ksp, err := search.KShortestPathsLooplessCtxWithOpts(
			context.Background(), c, 0, graph.NodeID(g.n-1), k,
			search.KShortestPathsLooplessOpts{MaxPops: kshortestMaxPops},
		)
		if err != nil {
			t.Fatalf("k=%d: bounded loopless error: %v", k, err)
		}
		if got := kshortestCosts(ksp); !kshortestIntsEqual(got, ref) {
			t.Fatalf("k=%d: loopless costs %v, brute-force %v", k, got, ref)
		}

		// Eppstein is a deprecated alias of the loopless entry => same answer.
		epp := kshortestCosts(search.EppsteinKShortest(c, 0, graph.NodeID(g.n-1), k)) //nolint:staticcheck // intentionally exercising the deprecated public alias
		if !kshortestIntsEqual(epp, ref) {
			t.Fatalf("k=%d: Eppstein(alias) costs %v, brute-force %v", k, epp, ref)
		}
	}
}

// --- Negative controls: prove the comparison DETECTS a deliberate mismatch. ---

// TestKShortestCompareExact_DetectsMismatch feeds the exact-comparison a result
// whose sorted-cost multiset differs from the reference and asserts it is
// flagged — both a wrong cost value and a wrong cardinality.
func TestKShortestCompareExact_DetectsMismatch(t *testing.T) {
	t.Parallel()
	g := kshortestHandGraph()
	ref := kshortestHandCosts // [3, 5, 6]

	// A path list whose costs are [3, 5, 7]: the third cost is wrong.
	wrongCost := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
		{Nodes: []graph.NodeID{0, 1, 3}, Cost: 7}, // truth is 6
	}
	if vs := kshortestCompareExact(7, "YenKShortest", g, 0, 3, 3, ref, wrongCost); len(vs) == 0 {
		t.Fatal("kshortestCompareExact accepted a cost multiset that differs from the reference")
	}

	// A path list with too few costs ([3, 5]) against a 3-element reference.
	wrongLen := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
	}
	if vs := kshortestCompareExact(7, "YenKShortest", g, 0, 3, 3, ref, wrongLen); len(vs) == 0 {
		t.Fatal("kshortestCompareExact accepted a result with the wrong number of paths")
	}

	// Sanity: the predicate accepts the genuinely-correct multiset.
	correct := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
		{Nodes: []graph.NodeID{0, 1, 3}, Cost: 6},
	}
	if vs := kshortestCompareExact(7, "YenKShortest", g, 0, 3, 3, ref, correct); len(vs) != 0 {
		t.Fatalf("kshortestCompareExact rejected the correct multiset: %v", vs)
	}
}

// TestKShortestCompareBounded_PrefixTolerance asserts the bounded comparison
// accepts a correct truncated prefix but rejects one that is not a prefix of the
// reference or that over-reports.
func TestKShortestCompareBounded_PrefixTolerance(t *testing.T) {
	t.Parallel()
	g := kshortestHandGraph()
	ref := kshortestHandCosts // [3, 5, 6]

	// A correct truncated prefix [3, 5] with truncated=true must be ACCEPTED.
	prefix := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
	}
	if vs := kshortestCompareBounded(7, "KShortestPathsLoopless", g, 0, 3, 3, ref, prefix, true); len(vs) != 0 {
		t.Fatalf("bounded comparison rejected a correct truncated prefix: %v", vs)
	}

	// A prefix that is NOT a prefix of the reference ([3, 6]) must be flagged
	// even when truncated.
	badPrefix := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 1, 3}, Cost: 6}, // ref[1] is 5, not 6
	}
	if vs := kshortestCompareBounded(7, "KShortestPathsLoopless", g, 0, 3, 3, ref, badPrefix, true); len(vs) == 0 {
		t.Fatal("bounded comparison accepted a result that is not a prefix of the reference")
	}

	// Over-reporting (more paths than the reference) is always a violation.
	over := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
		{Nodes: []graph.NodeID{0, 1, 3}, Cost: 6},
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3}, // a 4th, beyond the 3-element ref
	}
	if vs := kshortestCompareBounded(7, "KShortestPathsLoopless", g, 0, 3, 3, ref, over, true); len(vs) == 0 {
		t.Fatal("bounded comparison accepted a result with more paths than the reference")
	}
}

// TestKShortestValidatePaths_DetectsBadPath asserts per-path validation catches
// a non-existent edge, a wrong Cost, a repeated node, and a sort-order break.
func TestKShortestValidatePaths_DetectsBadPath(t *testing.T) {
	t.Parallel()
	g := kshortestHandGraph()

	// Non-existent edge 0->3 (no such edge in the hand graph).
	badEdge := []search.YenPath[int]{{Nodes: []graph.NodeID{0, 3}, Cost: 0}}
	if vs := kshortestValidatePaths(7, "YenKShortest", g, 0, 3, badEdge); len(vs) == 0 {
		t.Fatal("validation accepted a path using a non-existent edge")
	}

	// Real path 0->2->3 (true cost 5) but with a lie: Cost reported as 99.
	badCost := []search.YenPath[int]{{Nodes: []graph.NodeID{0, 2, 3}, Cost: 99}}
	if vs := kshortestValidatePaths(7, "YenKShortest", g, 0, 3, badCost); len(vs) == 0 {
		t.Fatal("validation accepted a path whose Cost contradicts its edges")
	}

	// A repeated node makes the path non-loopless. Build a graph with a 2-cycle
	// so the edges actually exist, then feed a walk that repeats node 1.
	cyc := &kshortestGraph{n: 4, adj: [][]kshortestEdge{
		{{to: 1, weight: 1}},
		{{to: 2, weight: 1}, {to: 0, weight: 1}}, // 1->0 closes a cycle
		{{to: 3, weight: 1}},
		nil,
	}}
	loopy := []search.YenPath[int]{{Nodes: []graph.NodeID{0, 1, 0, 1, 2, 3}, Cost: 5}}
	if vs := kshortestValidatePaths(7, "YenKShortest", cyc, 0, 3, loopy); len(vs) == 0 {
		t.Fatal("validation accepted a path that repeats a node (not loopless)")
	}

	// Out-of-order costs: two valid paths but the second is cheaper than the
	// first, which the cheapest-first contract forbids.
	misordered := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3}, // out of order: 3 < 5
	}
	if vs := kshortestValidatePaths(7, "YenKShortest", g, 0, 3, misordered); len(vs) == 0 {
		t.Fatal("validation accepted a path list that is not sorted by cost ascending")
	}

	// Sanity: the three genuine paths, correctly ordered, pass clean.
	good := []search.YenPath[int]{
		{Nodes: []graph.NodeID{0, 1, 2, 3}, Cost: 3},
		{Nodes: []graph.NodeID{0, 2, 3}, Cost: 5},
		{Nodes: []graph.NodeID{0, 1, 3}, Cost: 6},
	}
	if vs := kshortestValidatePaths(7, "YenKShortest", g, 0, 3, good); len(vs) != 0 {
		t.Fatalf("validation rejected three genuine, correctly-ordered paths: %v", vs)
	}
}

// TestEppsteinIsLooplessAlias documents and pins the semantic finding that
// EppsteinKShortest is a deprecated alias forwarding to the loopless entry: on a
// graph with a cycle reachable from src, BOTH return only loopless paths and
// produce identical cost lists. (If Eppstein allowed loops it could return a
// path that revisits a node; it does not.)
func TestEppsteinIsLooplessAlias(t *testing.T) {
	t.Parallel()
	// A graph with a 1<->2 cycle plus a route to dst=3.
	cyc := &kshortestGraph{n: 4, adj: [][]kshortestEdge{
		{{to: 1, weight: 1}},
		{{to: 2, weight: 1}, {to: 3, weight: 5}},
		{{to: 1, weight: 1}, {to: 3, weight: 1}}, // 2->1 closes a cycle
		nil,
	}}
	c := kshortestBuildCSR(cyc)
	k := 5
	epp := search.EppsteinKShortest(c, 0, 3, k) //nolint:staticcheck // intentionally exercising the deprecated public alias
	ksp, err := search.KShortestPathsLooplessCtxWithOpts(
		context.Background(), c, 0, 3, k,
		search.KShortestPathsLooplessOpts{MaxPops: kshortestMaxPops},
	)
	if err != nil {
		t.Fatalf("bounded loopless error: %v", err)
	}
	if !kshortestIntsEqual(kshortestCosts(epp), kshortestCosts(ksp)) {
		t.Fatalf("Eppstein alias %v != loopless %v", kshortestCosts(epp), kshortestCosts(ksp))
	}
	// And every Eppstein path is loopless (no repeated node), confirming it does
	// not return loop-bearing walks.
	for pi, p := range epp {
		seen := make(map[graph.NodeID]struct{}, len(p.Nodes))
		for _, node := range p.Nodes {
			if _, dup := seen[node]; dup {
				t.Fatalf("Eppstein path %d repeats node %d: %v (alias is NOT loopless?)", pi, node, p.Nodes)
			}
			seen[node] = struct{}{}
		}
	}
}
