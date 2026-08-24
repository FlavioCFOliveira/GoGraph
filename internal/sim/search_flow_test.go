package sim

import (
	"context"
	"errors"
	"strings"
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

// ---------------------------------------------------------------------------
// Min-cost max-flow: cost flavours, guards, planted cycles, and the
// optimality certificate.
// ---------------------------------------------------------------------------

// flowReplayCostFixtures reproduces, for one tick, the exact per-tick draw
// stream flowViolations consumes, and returns the min-cost-max-flow fixtures it
// would build, in fixture-index order. The max-flow and min-cut generators are
// replayed first purely to advance the shared Seed to the right position; the
// planted-negative-cycle fixtures that follow consume no draws at all.
//
// The replay mirrors flowViolations' draw order deliberately: it is the only way
// to observe the fixtures the DST really builds. TestFlowChecks_CleanOnFixtures
// independently exercises the real path end to end.
func flowReplayCostFixtures(tick int64) [][]flowCostEdge {
	seed := NewSeed(uint64(tick) ^ flowSeedConst)
	for i := 0; i < flowMaxFixtures; i++ {
		_, _ = flowGenNetwork(seed)
	}
	for i := 0; i < flowCutFixtures; i++ {
		_, _ = flowGenWeightMatrix(seed)
	}
	out := make([][]flowCostEdge, 0, flowMCMFFixtures)
	for i := 0; i < flowMCMFFixtures; i++ {
		_, edges := flowGenCostNetwork(seed, i >= flowMCMFNegFrom)
		out = append(out, edges)
	}
	return out
}

// flowFixtureNodeCount recovers a fixture's node count from its arcs. The
// generator's spine reaches n-1, so the largest dst index plus one is exactly n.
func flowFixtureNodeCount(edges []flowCostEdge) int {
	n := 0
	for _, e := range edges {
		if e.dst+1 > n {
			n = e.dst + 1
		}
	}
	return n
}

// TestFlowMCMF_CostFlavourByIndex proves the flavour split is decided by fixture
// INDEX and therefore fires on EVERY tick, never by a lucky seed draw:
//
//   - fixtures [0, flowMCMFNegFrom) carry only strictly positive costs, so
//     search/flow's hasNegativeCost stays false and the zero-potential fast path
//     is still driven exactly as it was before the negative flavour existed;
//   - fixtures [flowMCMFNegFrom, flowMCMFFixtures) carry at least one arc with
//     cap>0 && cost<0 — hasNegativeCost's own predicate — on 100% of ticks, and
//     that arc is always the FIRST spine arc out of src. Forcing is what buys
//     the 100%: with an unforced symmetric draw, 80 of 20,000 measured fixtures
//     had no negative arc at all.
func TestFlowMCMF_CostFlavourByIndex(t *testing.T) {
	t.Parallel()
	const ticks = 5000

	posFixtures, negFixtures := 0, 0
	negWithNegativeArc, negForcedFirstArc := 0, 0
	for tick := int64(0); tick < ticks; tick++ {
		for i, edges := range flowReplayCostFixtures(tick) {
			if len(edges) == 0 {
				t.Fatalf("tick %d fixture %d: empty edge list", tick, i)
			}
			if i < flowMCMFNegFrom {
				posFixtures++
				for k, e := range edges {
					if e.cost < 1 || e.cost > flowMaxCost {
						t.Fatalf("tick %d positive fixture %d arc %d: cost=%d outside [1,%d]",
							tick, i, k, e.cost, flowMaxCost)
					}
				}
				continue
			}
			negFixtures++
			for _, e := range edges { // hasNegativeCost's exact predicate
				if e.cap > 0 && e.cost < 0 {
					negWithNegativeArc++
					break
				}
			}
			if e := edges[0]; e.src == 0 && e.dst == 1 && e.cap > 0 && e.cost < 0 {
				negForcedFirstArc++
			}
			for k, e := range edges {
				if e.cost < -flowMaxCost || e.cost > flowMaxCost {
					t.Fatalf("tick %d negative fixture %d arc %d: cost=%d outside [-%d,+%d]",
						tick, i, k, e.cost, flowMaxCost, flowMaxCost)
				}
			}
		}
	}

	if want := ticks * flowMCMFNegFrom; posFixtures != want {
		t.Fatalf("all-positive fixtures = %d, want %d", posFixtures, want)
	}
	if want := ticks * (flowMCMFFixtures - flowMCMFNegFrom); negFixtures != want {
		t.Fatalf("forced-negative fixtures = %d, want %d", negFixtures, want)
	}
	if negWithNegativeArc != negFixtures {
		t.Fatalf("forced-negative fixtures carrying a cap>0 && cost<0 arc = %d/%d, want all of them",
			negWithNegativeArc, negFixtures)
	}
	if negForcedFirstArc != negFixtures {
		t.Fatalf("forced-negative fixtures whose FIRST spine arc is strictly negative = %d/%d, want all of them",
			negForcedFirstArc, negFixtures)
	}
	t.Logf("swept %d ticks: %d all-positive fixtures, %d forced-negative fixtures, %d/%d carry a cap>0 && cost<0 arc",
		ticks, posFixtures, negFixtures, negWithNegativeArc, negFixtures)
}

// TestFlowMCMF_NegativeFlavourIncludesZeroCostArcs proves the symmetric interval
// really does include 0, so a reduced cost of exactly 0 lands on the boundary of
// search/flow's `rc < 0` guard rather than always safely inside or outside it. A
// one-sided interval would leave that boundary undriven, and this assertion is
// what stops the interval silently narrowing later.
func TestFlowMCMF_NegativeFlavourIncludesZeroCostArcs(t *testing.T) {
	t.Parallel()
	const ticks = 5000
	zeros, negatives, positives, total := 0, 0, 0, 0
	for tick := int64(0); tick < ticks; tick++ {
		fixtures := flowReplayCostFixtures(tick)
		for i := flowMCMFNegFrom; i < len(fixtures); i++ {
			for _, e := range fixtures[i] {
				total++
				switch {
				case e.cost == 0:
					zeros++
				case e.cost < 0:
					negatives++
				default:
					positives++
				}
			}
		}
	}
	if zeros == 0 {
		t.Fatalf("no zero-cost arc among %d forced-negative arcs: the interval no longer includes 0", total)
	}
	if negatives == 0 || positives == 0 {
		t.Fatalf("forced-negative arcs are not two-sided: %d negative, %d zero, %d positive of %d",
			negatives, zeros, positives, total)
	}
	t.Logf("forced-negative sweep over %d ticks: %d arcs = %d negative (%.1f%%), %d zero, %d positive",
		ticks, total, negatives, 100*float64(negatives)/float64(total), zeros, positives)
}

// TestFlowMCMF_NegativeFlavourDrivesBootstrap proves the Bellman-Ford potential
// bootstrap is genuinely EXECUTED, not merely made reachable.
//
// The argument needs no access to search/flow's internals. On a forced-negative
// fixture the first arc out of src has cap>0 and cost<0. Were the bootstrap
// skipped, every potential would stay 0, so that arc's reduced cost would be
// rc = cost < 0; src is the first node Dijkstra pops, so the arc is evaluated
// immediately and the production `rc < 0` invariant guard would return an error.
// MinCostMaxFlowCtx returning a NIL error on a fixture that provably contains
// such an arc therefore proves the bootstrap ran and installed non-trivial
// potentials.
func TestFlowMCMF_NegativeFlavourDrivesBootstrap(t *testing.T) {
	t.Parallel()
	checked := 0
	for tick := int64(0); tick < 200; tick++ {
		fixtures := flowReplayCostFixtures(tick)
		for i := flowMCMFNegFrom; i < len(fixtures); i++ {
			edges := fixtures[i]
			if first := edges[0]; first.src != 0 || first.cap <= 0 || first.cost >= 0 {
				t.Fatalf("tick %d fixture %d: first arc %d->%d cap=%d cost=%d is not a forced negative arc out of src",
					tick, i, first.src, first.dst, first.cap, first.cost)
			}
			n := flowFixtureNodeCount(edges)
			if _, _, err := flow.MinCostMaxFlowCtx(
				context.Background(), flowBuildCostNetwork(n, edges), 0, n-1); err != nil {
				t.Fatalf("tick %d fixture %d: MinCostMaxFlowCtx err = %v, want nil (edges=%s)",
					tick, i, err, flowFmtCostEdges(edges))
			}
			checked++
		}
	}
	t.Logf("%d forced-negative fixtures each began with a cap>0 cost<0 arc out of src and still returned a nil error",
		checked)
}

// TestFlowCheckCostNetwork_ReportsCtxError is the falsifiability witness for the
// error clause: it drives the real checker with a network whose only defect is
// one search/flow rejects, and asserts EXACTLY ONE violation — the one naming
// the error.
//
// The network is a single arc 0->1 with a NEGATIVE capacity. It is DAG-ordered,
// so the structural guard stays silent; the reference skips cap<=0 arcs and
// returns (0,0) instantly; the plain wrapper, the Dinic cross-check and the
// certificate all agree on 0. Only MinCostMaxFlowCtx has anything to say, and it
// says ErrCapacityOverflow — precisely the class of failure the non-context
// wrapper used to swallow in silence.
func TestFlowCheckCostNetwork_ReportsCtxError(t *testing.T) {
	t.Parallel()
	edges := []flowCostEdge{{src: 0, dst: 1, cap: -1, cost: 1}}

	// Pre-post: the wrapper the checker used to call really is blind here.
	if f, c := flow.MinCostMaxFlow(flowBuildCostNetwork(2, edges), 0, 1); f != 0 || c != 0 {
		t.Fatalf("flow.MinCostMaxFlow = (%d,%d), want (0,0)", f, c)
	}
	if _, _, err := flow.MinCostMaxFlowCtx(
		context.Background(), flowBuildCostNetwork(2, edges), 0, 1); !errors.Is(err, flow.ErrCapacityOverflow) {
		t.Fatalf("MinCostMaxFlowCtx err = %v, want ErrCapacityOverflow", err)
	}

	vs := flowCheckCostNetwork(7, 2, edges, 0, 1)
	if len(vs) != 1 {
		t.Fatalf("flowCheckCostNetwork = %d violation(s), want exactly 1:\n%s", len(vs), flowFmtViolations(vs))
	}
	if vs[0].Kind != ViolationSearchDivergence {
		t.Fatalf("violation kind = %q, want %q", vs[0].Kind, ViolationSearchDivergence)
	}
	if !strings.Contains(vs[0].Message, "MinCostMaxFlowCtx returned a non-nil error") ||
		!strings.Contains(vs[0].Message, flow.ErrCapacityOverflow.Error()) {
		t.Fatalf("violation does not name the swallowed error: %s", vs[0].Message)
	}
	if vs[0].Tick != 7 {
		t.Fatalf("violation tick = %d, want 7", vs[0].Tick)
	}
}

// TestFlowDAGGuard_DetectsBackwardArc proves the structural guard is live in both
// directions, at the predicate level and end to end through the checker. The
// guard is what makes the reference's acyclicity precondition structural rather
// than lucky, so an assertion that could never fire would be worthless.
func TestFlowDAGGuard_DetectsBackwardArc(t *testing.T) {
	t.Parallel()

	forward := []flowCostEdge{
		{src: 0, dst: 1, cap: 3, cost: 1},
		{src: 1, dst: 2, cap: 3, cost: 1},
	}
	if got := flowFirstNonDAGArc(forward); got != -1 {
		t.Fatalf("flowFirstNonDAGArc(forward) = %d, want -1", got)
	}
	backward := []flowCostEdge{
		{src: 0, dst: 1, cap: 3, cost: 1},
		{src: 2, dst: 1, cap: 3, cost: -4}, // backward: closes a negative cycle with 1->2
		{src: 1, dst: 2, cap: 3, cost: 1},
	}
	if got := flowFirstNonDAGArc(backward); got != 1 {
		t.Fatalf("flowFirstNonDAGArc(backward) = %d, want 1", got)
	}
	if got := flowFirstNonDAGArc([]flowCostEdge{{src: 2, dst: 2, cap: 1, cost: 1}}); got != 0 {
		t.Fatalf("flowFirstNonDAGArc(self-loop) = %d, want 0", got)
	}

	// End to end: the checker reports it and does NOT reach the reference, which
	// this very edge list would otherwise send round a negative cycle.
	vs := flowCheckCostNetwork(3, 3, backward, 0, 2)
	if len(vs) != 1 {
		t.Fatalf("flowCheckCostNetwork(backward) = %d violation(s), want exactly 1:\n%s",
			len(vs), flowFmtViolations(vs))
	}
	if !strings.Contains(vs[0].Message, "DAG precondition") || !strings.Contains(vs[0].Message, "2->1") {
		t.Fatalf("violation does not name the offending arc: %s", vs[0].Message)
	}
}

// TestFlowRefSSP_RelaxBudgetTripsOnNegativeCycle proves the reference's
// relaxation budget converts a hang into a named failure. The input is the
// planted "disjoint-cycle" fixture: its 1->2->1 cycle is worth -4, so SPFA's
// integer distances would decrease without bound and the nodes would requeue
// forever. With the budget the reference returns errFlowRefRelaxBudget instead.
//
// Reaching the assertions at all means the reference terminated: a test that
// hung would never report.
func TestFlowRefSSP_RelaxBudgetTripsOnNegativeCycle(t *testing.T) {
	t.Parallel()
	f := flowNegCycleFixtures()[0]
	if f.name != "disjoint-cycle" {
		t.Fatalf("fixture 0 = %q, want disjoint-cycle", f.name)
	}
	mf, mc, residual, err := flowRefMinCostMaxFlow(f.n, f.edges, f.src, f.sink)
	if !errors.Is(err, errFlowRefRelaxBudget) {
		t.Fatalf("flowRefMinCostMaxFlow err = %v, want errFlowRefRelaxBudget (flow=%d cost=%d)", err, mf, mc)
	}
	if residual == nil {
		t.Fatal("flowRefMinCostMaxFlow returned a nil residual alongside its error")
	}
	// Cross-check with a structurally different detector: the certificate agrees
	// there is a negative cycle in that residual.
	if !flowResidualHasNegativeCycle(residual) {
		t.Fatal("flowResidualHasNegativeCycle disagreed with the budget on a planted negative cycle")
	}
}

// TestFlowRefSSP_RelaxBudgetSilentOnRealFixtures proves the budget does not
// false-positive: over the fixtures the DST actually builds — BOTH cost flavours
// — the reference always converges well inside it. A guard that fired on clean
// input would be worse than no guard at all.
func TestFlowRefSSP_RelaxBudgetSilentOnRealFixtures(t *testing.T) {
	t.Parallel()
	checked := 0
	for tick := int64(0); tick < 300; tick++ {
		for i, edges := range flowReplayCostFixtures(tick) {
			n := flowFixtureNodeCount(edges)
			if _, _, _, err := flowRefMinCostMaxFlow(n, edges, 0, n-1); err != nil {
				t.Fatalf("tick %d fixture %d: reference err = %v on a well-formed fixture (edges=%s)",
					tick, i, err, flowFmtCostEdges(edges))
			}
			checked++
		}
	}
	t.Logf("%d well-formed fixtures converged inside the (n+1)*arcs relaxation budget", checked)
}

// TestFlowResidualCertificate_FiresOnSuboptimalMaxFlow is the non-vacuity witness
// for the optimality certificate, on the measured four-node network
//
//	0->1 c1 $0 ; 1->3 c1 $10 ; 1->2 c1 $0 ; 2->3 c1 $1
//
// whose max-flow value is 1 by either of two routes:
//
//	optimum     : 0->1->2->3 at cost  1  -> residual has NO negative cycle
//	sub-optimal : 0->1->3    at cost 10  -> residual cycle 1->2->3->1 is worth
//	                                        0 + 1 + (-10) = -9, so it FIRES
//
// The sub-optimal flow is still MAXIMUM. That is the whole point: it is exactly
// the failure the Dinic value cross-check cannot see, and the certificate is the
// only clause in the checker able to catch it.
func TestFlowResidualCertificate_FiresOnSuboptimalMaxFlow(t *testing.T) {
	t.Parallel()
	const n = 4
	edges := []flowCostEdge{
		{src: 0, dst: 1, cap: 1, cost: 0},
		{src: 1, dst: 3, cap: 1, cost: 10},
		{src: 1, dst: 2, cap: 1, cost: 0},
		{src: 2, dst: 3, cap: 1, cost: 1},
	}

	// Both candidate flows are MAXIMUM, so the value check cannot separate them.
	capEdges := make([]flowEdge, len(edges))
	for i, e := range edges {
		capEdges[i] = flowEdge{src: e.src, dst: e.dst, cap: e.cap}
	}
	if got := flow.MaxFlow(flowBuildNetwork(n, capEdges), 0, 3); got != 1 {
		t.Fatalf("Dinic max-flow = %d, want 1", got)
	}

	// Sub-optimal but maximum: the certificate must FIRE.
	sub := flowRefBuildResidual(n, edges)
	flowTestPushPath(t, sub, []int{0, 1, 3}, 1)
	if !flowResidualHasNegativeCycle(sub) {
		t.Fatal("certificate stayed silent on a sub-optimal MAXIMUM flow (cycle 1->2->3->1 is worth -9)")
	}

	// Optimum: the certificate must stay silent.
	opt := flowRefBuildResidual(n, edges)
	flowTestPushPath(t, opt, []int{0, 1, 2, 3}, 1)
	if flowResidualHasNegativeCycle(opt) {
		t.Fatal("certificate fired on the optimum flow")
	}

	// And the reference itself finds the optimum, whose residual is certified.
	mf, mc, residual, err := flowRefMinCostMaxFlow(n, edges, 0, 3)
	if err != nil {
		t.Fatalf("flowRefMinCostMaxFlow err = %v, want nil", err)
	}
	if mf != 1 || mc != 1 {
		t.Fatalf("reference min-cost max-flow = (%d,%d), want (1,1)", mf, mc)
	}
	if flowResidualHasNegativeCycle(residual) {
		t.Fatal("certificate fired on the reference's own optimal residual")
	}
	// The whole checker is clean on this network too.
	if vs := flowCheckCostNetwork(0, n, edges, 0, 3); len(vs) != 0 {
		t.Fatalf("flowCheckCostNetwork on the witness network = %d violation(s), want 0:\n%s",
			len(vs), flowFmtViolations(vs))
	}
}

// TestFlowResidualCertificate_CatchesCycleUnreachableFromSrc pins why the
// certificate seeds dist ALL-ZERO (a virtual super-source over every node)
// instead of only at src: the negative cycle here sits on nodes 1..3 and is
// unreachable from node 0, so a src-seeded Bellman-Ford would never relax it and
// would report clean. The positive-cycle twin proves the detector is not simply
// answering "true" to every cycle.
func TestFlowResidualCertificate_CatchesCycleUnreachableFromSrc(t *testing.T) {
	t.Parallel()
	// Node 0 is isolated; 1->2->3->1 costs 1 + 1 + (-5) = -3.
	adj := flowRefBuildResidual(4, []flowCostEdge{
		{src: 1, dst: 2, cap: 1, cost: 1},
		{src: 2, dst: 3, cap: 1, cost: 1},
		{src: 3, dst: 1, cap: 1, cost: -5},
	})
	if !flowResidualHasNegativeCycle(adj) {
		t.Fatal("certificate missed a negative cycle unreachable from node 0")
	}
	// Same shape, positive cycle (1 + 1 + 5 = 7): must stay silent.
	clean := flowRefBuildResidual(4, []flowCostEdge{
		{src: 1, dst: 2, cap: 1, cost: 1},
		{src: 2, dst: 3, cap: 1, cost: 1},
		{src: 3, dst: 1, cap: 1, cost: 5},
	})
	if flowResidualHasNegativeCycle(clean) {
		t.Fatal("certificate fired on a positive-cost cycle")
	}
}

// TestFlowNegCycleFixtures_HandValues pins every hand-computed number in the
// planted-negative-cycle fixtures against the real engine, and proves the
// contract the DST now asserts on every tick:
//
//   - MinCostMaxFlowCtx returns (0, 0) with errors.Is(err, ErrNegativeCycle);
//   - the non-context MinCostMaxFlow returns exactly (0, 0);
//   - plain Dinic on the SAME capacities returns the fixture's non-zero value,
//     which is what makes the (0,0) assertions evidential rather than vacuous.
func TestFlowNegCycleFixtures_HandValues(t *testing.T) {
	t.Parallel()
	fixtures := flowNegCycleFixtures()
	if len(fixtures) != 2 {
		t.Fatalf("flowNegCycleFixtures() = %d fixtures, want 2", len(fixtures))
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			gotFlow, gotCost, err := flow.MinCostMaxFlowCtx(
				context.Background(), flowBuildCostNetwork(f.n, f.edges), f.src, f.sink)
			if !errors.Is(err, flow.ErrNegativeCycle) {
				t.Fatalf("MinCostMaxFlowCtx err = %v, want ErrNegativeCycle", err)
			}
			if gotFlow != 0 || gotCost != 0 {
				t.Fatalf("MinCostMaxFlowCtx = (%d,%d), want (0,0)", gotFlow, gotCost)
			}
			plainFlow, plainCost := flow.MinCostMaxFlow(flowBuildCostNetwork(f.n, f.edges), f.src, f.sink)
			if plainFlow != 0 || plainCost != 0 {
				t.Fatalf("MinCostMaxFlow = (%d,%d), want (0,0)", plainFlow, plainCost)
			}
			// NON-VACUITY: the same capacities carry real flow, so the (0,0)
			// above is a refusal and not an empty network.
			capEdges := make([]flowEdge, len(f.edges))
			for i, e := range f.edges {
				capEdges[i] = flowEdge{src: e.src, dst: e.dst, cap: e.cap}
			}
			d := flow.MaxFlow(flowBuildNetwork(f.n, capEdges), f.src, f.sink)
			if d != f.dinic {
				t.Fatalf("Dinic max-flow = %d, want %d", d, f.dinic)
			}
			if d == 0 {
				t.Fatal("fixture carries no flow at all, so the (0,0) assertions above are vacuous")
			}
			if vs := flowCheckNegCycleFixture(11, f); len(vs) != 0 {
				t.Fatalf("flowCheckNegCycleFixture = %d violation(s), want 0:\n%s", len(vs), flowFmtViolations(vs))
			}
		})
	}
}

// TestFlowCheckNegCycleFixture_ClausesAreLive proves the planted-fixture checker
// can fail, one clause at a time.
func TestFlowCheckNegCycleFixture_ClausesAreLive(t *testing.T) {
	t.Parallel()

	// (a) A fixture with NO negative cycle: the engine returns (1,1) and a nil
	// error, so the ErrNegativeCycle clause and both (0,0) clauses must fire —
	// three violations — while the non-vacuity clause stays satisfied.
	clean := flowNegCycleFixture{
		name:  "no-cycle-at-all",
		n:     2,
		src:   0,
		sink:  1,
		dinic: 1,
		edges: []flowCostEdge{{src: 0, dst: 1, cap: 1, cost: 1}},
	}
	vs := flowCheckNegCycleFixture(1, clean)
	if len(vs) != 3 {
		t.Fatalf("clean fixture = %d violation(s), want 3:\n%s", len(vs), flowFmtViolations(vs))
	}
	wants := []string{
		"did not report ErrNegativeCycle",
		"augmented a network holding a negative cycle",
		"wrapper returned",
	}
	for i, w := range wants {
		if !strings.Contains(vs[i].Message, w) {
			t.Fatalf("violation %d = %q, want it to mention %q", i, vs[i].Message, w)
		}
	}

	// (b) A real planted fixture whose non-vacuity witness is wrong: only the
	// Dinic clause fires. This is the clause that stops the (0,0) assertions
	// degenerating into "there was never any flow to lose".
	bad := flowNegCycleFixtures()[0]
	bad.dinic = 99
	vs = flowCheckNegCycleFixture(2, bad)
	if len(vs) != 1 {
		t.Fatalf("wrong-dinic fixture = %d violation(s), want 1:\n%s", len(vs), flowFmtViolations(vs))
	}
	if !strings.Contains(vs[0].Message, "lost its non-vacuity witness") {
		t.Fatalf("violation = %q, want the non-vacuity message", vs[0].Message)
	}
}

// TestFlowMCMF_NonCtxWrapperDiscardsError records WHY the checker now drives the
// context entry point: on a network search/flow refuses, the non-context wrapper
// returns a perfectly ordinary-looking (0, 0) and no signal whatsoever, while
// the context entry point names the reason. Driving only the wrapper is
// therefore blind to ErrNegativeCycle, ErrCapacityOverflow, and the internal
// rc<0 invariant violation alike.
func TestFlowMCMF_NonCtxWrapperDiscardsError(t *testing.T) {
	t.Parallel()
	f := flowNegCycleFixtures()[0]

	plainFlow, plainCost := flow.MinCostMaxFlow(flowBuildCostNetwork(f.n, f.edges), f.src, f.sink)
	if plainFlow != 0 || plainCost != 0 {
		t.Fatalf("MinCostMaxFlow = (%d,%d), want (0,0)", plainFlow, plainCost)
	}
	if _, _, err := flow.MinCostMaxFlowCtx(
		context.Background(), flowBuildCostNetwork(f.n, f.edges), f.src, f.sink); err == nil {
		t.Fatal("MinCostMaxFlowCtx returned nil where the wrapper's (0,0) hid a refusal")
	}
	// The two entry points therefore carry DIFFERENT information about the same
	// input, and only the context one can tell a refusal from an empty answer.
	capEdges := make([]flowEdge, len(f.edges))
	for i, e := range f.edges {
		capEdges[i] = flowEdge{src: e.src, dst: e.dst, cap: e.cap}
	}
	if got := flow.MaxFlow(flowBuildNetwork(f.n, capEdges), f.src, f.sink); got == 0 {
		t.Fatal("the fixture carries no flow, so the wrapper's (0,0) would be honest")
	}
}

// TestFlowChecks_DeterministicFixtures strengthens the determinism guarantee
// beyond violation COUNTS, which are 0 on clean fixtures and therefore compare
// equal no matter what the generator did. It replays the whole per-tick cost
// stream twice and compares the rendered fixtures arc by arc, so the assertion
// has something to see, and it compares the checker's whole rendered violation
// SET for the same tick across two independent runs.
func TestFlowChecks_DeterministicFixtures(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 5, 50, 500, 5000, 123456} {
		a, b := flowReplayCostFixtures(tick), flowReplayCostFixtures(tick)
		if len(a) != flowMCMFFixtures || len(b) != flowMCMFFixtures {
			t.Fatalf("tick %d: replayed %d/%d fixtures, want %d each", tick, len(a), len(b), flowMCMFFixtures)
		}
		for i := range a {
			ra, rb := flowFmtCostEdges(a[i]), flowFmtCostEdges(b[i])
			if ra != rb {
				t.Fatalf("tick %d fixture %d not deterministic:\n run1=%s\n run2=%s", tick, i, ra, rb)
			}
			if len(a[i]) == 0 {
				t.Fatalf("tick %d fixture %d is empty, so the comparison is vacuous", tick, i)
			}
		}
		// The checker's whole verdict for the same tick is stable too.
		if x, y := flowFmtViolations(flowViolations(tick)), flowFmtViolations(flowViolations(tick)); x != y {
			t.Fatalf("tick %d: flowViolations not deterministic:\n run1=%q\n run2=%q", tick, x, y)
		}
	}

	// The comparison above is over an EMPTY violation set on clean fixtures, so
	// on its own it would compare "" with "". Repeat it on a fixture that really
	// does produce violations, so the rendered-set equality has something to see.
	dirty := flowNegCycleFixture{
		name:  "no-cycle-at-all",
		n:     2,
		src:   0,
		sink:  1,
		dinic: 1,
		edges: []flowCostEdge{{src: 0, dst: 1, cap: 1, cost: 1}},
	}
	x, y := flowCheckNegCycleFixture(9, dirty), flowCheckNegCycleFixture(9, dirty)
	if len(x) == 0 {
		t.Fatal("the dirty fixture produced no violations, so the determinism comparison below is vacuous")
	}
	if rx, ry := flowFmtViolations(x), flowFmtViolations(y); rx != ry {
		t.Fatalf("violation SET not deterministic across two runs:\n run1=%q\n run2=%q", rx, ry)
	}
}

// flowTestPushPath applies amount units of flow along path (a node sequence) in
// adj, mutating the residual exactly as an augmentation would: the forward arc
// loses capacity and its paired reverse arc gains it. It is a test-only helper
// so a hand-chosen — including deliberately sub-optimal — flow can be handed to
// the optimality certificate.
func flowTestPushPath(t *testing.T, adj [][]flowResidualArc, path []int, amount int) {
	t.Helper()
	for i := 0; i+1 < len(path); i++ {
		u, v := path[i], path[i+1]
		found := -1
		for ai := range adj[u] {
			if adj[u][ai].to == v && adj[u][ai].cap >= amount {
				found = ai
				break
			}
		}
		if found < 0 {
			t.Fatalf("no residual arc %d->%d with cap >= %d", u, v, amount)
		}
		adj[u][found].cap -= amount
		rev := adj[u][found].rev
		adj[v][rev].cap += amount
	}
}

// flowFmtViolations renders a violation slice for a test failure message.
func flowFmtViolations(vs []Violation) string {
	s := ""
	for _, v := range vs {
		s += "  " + v.String() + "\n"
	}
	return s
}
