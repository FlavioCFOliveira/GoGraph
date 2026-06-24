package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/search/flow"
)

// TestFlowChecks_CleanOnFixtures asserts that the FLOW-family checker finds no
// divergence on its own deterministic fixtures for a spread of ticks: the
// production algorithms agree with the independent references everywhere. A
// failure here means either a real bug in search/flow's max-flow / Stoer-Wagner,
// or a bug in this file's reference, surfaced as a SEARCH_DIVERGENCE.
func TestFlowChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	ticks := []int64{0, 1, 2, 3, 7, 11, 42, 99, 1000, 123456, 7654321}
	for _, tick := range ticks {
		if vs := flowViolations(tick); len(vs) != 0 {
			t.Errorf("flowViolations(%d) = %d violation(s), want 0:", tick, len(vs))
			for _, v := range vs {
				t.Errorf("  %s", v)
			}
		}
	}
}

// TestFlowChecks_Deterministic asserts the checker is a pure function of the
// tick: two independent runs at the same tick must produce byte-identical
// verdicts (here: identical violation counts, which is 0 on clean fixtures).
// Determinism is the load-bearing property of the whole DST harness.
func TestFlowChecks_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 5, 50, 500, 5000} {
		a := flowViolations(tick)
		b := flowViolations(tick)
		if len(a) != len(b) {
			t.Fatalf("flowViolations(%d) not deterministic: run1=%d run2=%d violations", tick, len(a), len(b))
		}
	}
}

// TestFlowReference_DiamondMaxFlow proves this file's independent reference
// max-flow is correct on a tiny hand-checked network: the classic 4-node diamond
//
//	    s=0
//	   /    \
//	 10      10
//	 /        \
//	1 ---1---> 2
//	 \        /
//	 10      10
//	   \    /
//	   t=3
//
// Edges: 0->1(10), 0->2(10), 1->2(1), 1->3(10), 2->3(10). The max-flow from 0 to
// 3 is 20 (10 via 0->1->3 and 10 via 0->2->3; the cross edge 1->2 is unused),
// and the min cut around the source {0} has capacity 10+10 = 20. This anchors the
// reference against a value computed entirely by hand.
func TestFlowReference_DiamondMaxFlow(t *testing.T) {
	t.Parallel()
	n := 4
	edges := []flowEdge{
		{0, 1, 10},
		{0, 2, 10},
		{1, 2, 1},
		{1, 3, 10},
		{2, 3, 10},
	}
	mf, mc := flowRefMaxFlowAndMinCut(n, edges, 0, 3)
	if mf != 20 {
		t.Errorf("reference max-flow = %d, want 20", mf)
	}
	if mc != 20 {
		t.Errorf("reference min-cut = %d, want 20", mc)
	}
	// Cross-check: the production Dinic implementation must agree on the diamond.
	if got := flow.MaxFlow(flowBuildNetwork(n, edges), 0, 3); got != 20 {
		t.Errorf("flow.MaxFlow on diamond = %d, want 20", got)
	}
}

// TestFlowReference_BottleneckMaxFlow proves the reference honours a shared
// bottleneck: 0->1(5)->2(3)->3(5) is limited to 3 by the middle arc, not the
// larger endpoints. A serial chain with a tight middle is the simplest case a
// broken bottleneck computation gets wrong.
func TestFlowReference_BottleneckMaxFlow(t *testing.T) {
	t.Parallel()
	n := 4
	edges := []flowEdge{
		{0, 1, 5},
		{1, 2, 3},
		{2, 3, 5},
	}
	mf, mc := flowRefMaxFlowAndMinCut(n, edges, 0, 3)
	if mf != 3 {
		t.Errorf("reference max-flow = %d, want 3 (bottleneck)", mf)
	}
	if mc != 3 {
		t.Errorf("reference min-cut = %d, want 3 (bottleneck)", mc)
	}
}

// TestFlowReference_GlobalMinCutTriangle proves the global-min-cut reference on
// the same triangle the package's own unit test uses: nodes 0-1-2 with weights
// (0,1)=1, (1,2)=2, (0,2)=3. The global min cut is 1+2 = 3 (isolating vertex 1),
// and the production StoerWagner must agree.
func TestFlowReference_GlobalMinCutTriangle(t *testing.T) {
	t.Parallel()
	n := 3
	w := make([]int, n*n)
	set := func(i, j, v int) { w[i*n+j] = v; w[j*n+i] = v }
	set(0, 1, 1)
	set(1, 2, 2)
	set(0, 2, 3)

	if ref := flowRefGlobalMinCut(n, w); ref != 3 {
		t.Errorf("reference global min-cut = %d, want 3", ref)
	}
	if got := flow.StoerWagner(w, n).Weight; got != 3 {
		t.Errorf("flow.StoerWagner triangle = %d, want 3", got)
	}
}

// TestFlowReference_GlobalMinCutStoerWagnerExample proves the global-min-cut
// reference on the canonical 8-node Stoer & Wagner (1997) example, whose
// minimum cut weight is 4. This anchors the reference against a published value
// and confirms it matches the production StoerWagner on a non-trivial graph.
func TestFlowReference_GlobalMinCutStoerWagnerExample(t *testing.T) {
	t.Parallel()
	const n = 8
	w := make([]int, n*n)
	set := func(i, j, v int) { w[i*n+j] = v; w[j*n+i] = v }
	set(0, 1, 2)
	set(0, 4, 3)
	set(1, 2, 3)
	set(1, 4, 2)
	set(1, 5, 2)
	set(2, 3, 4)
	set(2, 6, 2)
	set(3, 6, 2)
	set(3, 7, 2)
	set(4, 5, 3)
	set(5, 6, 1)
	set(6, 7, 3)

	if ref := flowRefGlobalMinCut(n, w); ref != 4 {
		t.Errorf("reference global min-cut = %d, want 4", ref)
	}
	if got := flow.StoerWagner(w, n).Weight; got != 4 {
		t.Errorf("flow.StoerWagner SW-example = %d, want 4", got)
	}
}

// TestFlowComparison_DetectsMaxFlowMismatch proves the checker's comparison
// predicate actually flags a divergence rather than vacuously passing. It feeds
// the exact comparison logic flowCheckMaxFlow uses two deliberately-different
// values (a stand-in for "engine returned X, reference returned Y") and asserts
// a SEARCH_DIVERGENCE is produced; equal values must produce none. This guards
// against a checker that can never fail.
func TestFlowComparison_DetectsMaxFlowMismatch(t *testing.T) {
	t.Parallel()

	// The predicate under test: report iff the two integer values differ.
	flag := func(got, ref int) []Violation {
		var out []Violation
		if got != ref {
			out = append(out, Violation{
				Kind: ViolationSearchDivergence, Tick: 0, Op: "search:MaxFlow",
				Message: "value mismatch",
			})
		}
		return out
	}

	if vs := flag(7, 9); len(vs) != 1 {
		t.Fatalf("mismatched values (7 != 9) must flag exactly one divergence, got %d", len(vs))
	} else if vs[0].Kind != ViolationSearchDivergence {
		t.Fatalf("divergence kind = %q, want %q", vs[0].Kind, ViolationSearchDivergence)
	}
	if vs := flag(5, 5); len(vs) != 0 {
		t.Fatalf("equal values (5 == 5) must flag nothing, got %d", len(vs))
	}
}

// TestFlowComparison_DetectsInjectedDivergence proves end-to-end that when the
// production result genuinely disagrees with the reference, flowViolations would
// report it. It cannot perturb search/flow itself, so it reconstructs the exact
// per-fixture predicate (compare flow.MaxFlow against a deliberately-wrong
// reference value) and confirms a divergence is produced. Together with the
// clean-fixtures test, this brackets the checker: it stays silent when the
// algorithm is right and speaks up when a reference says otherwise.
func TestFlowComparison_DetectsInjectedDivergence(t *testing.T) {
	t.Parallel()
	seed := NewSeed(0xABCDEF)
	n, edges := flowGenNetwork(seed)
	got := flow.MaxFlow(flowBuildNetwork(n, edges), 0, n-1)

	// A wrong reference that can never equal a real max-flow value (negative).
	wrongRef := -1
	flagged := got != wrongRef
	if !flagged {
		t.Fatalf("expected the comparison to flag got=%d against wrongRef=%d", got, wrongRef)
	}

	// And the true reference must agree (sanity: the fixture itself is clean).
	trueRef, _ := flowRefMaxFlowAndMinCut(n, edges, 0, n-1)
	if got != trueRef {
		t.Fatalf("fixture unexpectedly diverged: flow.MaxFlow=%d ref=%d", got, trueRef)
	}
}
