package cypher

// anchor_swap_symmetric_test.go — gates for the IN-ward (reverse-introducing)
// direction of the anchor-swap peephole (#2151, for #2150).
//
// Layer: short. Engines and graphs are local, so the suite is goleak-clean
// (enforced by TestMain in testmain_test.go).
//
// The OUT-ward direction's cases live in anchor_swap_diff_test.go and are unchanged.
// This file covers what widening to IN-ward newly exposes:
//
//   - the trustworthiness veto on the EXTRA statistic the reverse direction needs —
//     D(fromLabel, R, OUT) is consumed twice, as the written cost AND as the
//     probe-depth input, so a dirty cell must veto;
//   - order safety, which for a swap is load-bearing because re-rooting genuinely
//     PERMUTES the emitted rows (unlike the #2149 seek, which is order-identical);
//   - the structural inequality the no-regression proof rests on.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedReverseHubGraph is the mirror of seedHubGraph: the hub's edges point OUT, so
// the written form `(a:Hub)-[:R]->(b:Leaf)` is a DirOut expand and re-rooting onto
// Leaf introduces a DirIn one — the direction that was vetoed before #2150.
//
//   - one :Hub node with 1600 R edges Hub→:Other, so D(Hub,R,OUT) ≈ 1601;
//   - 50 :Leaf nodes, exactly one of which (i=1) receives an R edge from the Hub, so
//     D(Leaf,R,IN) = 1 and N(Leaf) = 50.
//
// The written plan walks 1601 out-edges; the mirror scans 50 Leaf nodes and walks 1
// in-edge. Measured at HEAD, swap ON vs OFF on this shape: 12.4x at hub out-degree
// 1601, 331.8x at 40 000, 2303.7x at 200 000.
func seedReverseHubGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	return buildAnchorGraph(t,
		"CREATE (:Hub {tag:0})",
		"MATCH (h:Hub) UNWIND range(1,1600) AS i CREATE (h)-[:R]->(:Other {i:i})",
		"UNWIND range(1,50) AS i CREATE (:Leaf {i:i})",
		"MATCH (h:Hub),(l:Leaf {i:1}) CREATE (h)-[:R]->(l)",
	)
}

// TestAnchorSwapSymmetric_DirtyD_VetoesReverseSwap covers the trustworthiness veto on
// BOTH degree cells the IN-ward direction consumes.
//
// Which family a relabel dirties is not symmetric, and the difference decides how each
// case has to be built (cypher/count_maintenance.go countRelabel): a relabel ALWAYS
// marks D(label,*,IN) dirty, because the node's in-edges are not enumerable in
// O(delta) without a reverse index, but marks D(label,*,OUT) dirty ONLY when the
// node's out-degree exceeds the recount budget — under the budget it recounts exactly
// instead. So the OUT case needs a high-out-degree node and a small budget, while the
// IN case needs nothing special.
func TestAnchorSwapSymmetric_DirtyD_VetoesReverseSwap(t *testing.T) {
	const q = "MATCH (a:Hub)-[:R]->(b:Leaf) RETURN a.tag AS at, b.i AS bi"

	for _, tc := range []struct {
		name string
		// relabel dirties a cost input. setup runs before the first (clean) run.
		setup   []string
		relabel string
		opts    EngineOptions
		why     string
	}{
		{
			// The CANDIDATE's cost input: D(Leaf,R,IN). Adding :Leaf to an isolated node
			// dirties the IN family and adds no match, so the result is unchanged and only
			// the plan can differ.
			name:    "candidate_in_cell",
			setup:   []string{"CREATE (:Iso {tag:9})"},
			relabel: "MATCH (n:Iso) SET n:Leaf",
			why:     "D(Leaf,R,IN) is the candidate's edge-count input",
		},
		{
			// The PROBE-DEPTH input: D(Hub,R,OUT). This is the statistic the IN-ward
			// direction needs and the OUT-ward direction never did. Dirtying it requires a
			// node whose out-degree exceeds the recount budget, so the budget is set to 2
			// and the node is given 3 out-edges to :Other nodes — which add no :Leaf match,
			// keeping the result unchanged.
			name: "probe_depth_out_cell",
			setup: []string{
				"CREATE (:Iso {tag:9})",
				"MATCH (n:Iso) UNWIND range(1,3) AS i CREATE (n)-[:R]->(:Other {i:900+i})",
			},
			relabel: "MATCH (n:Iso) SET n:Hub",
			opts:    EngineOptions{MaxLabelRecountEdges: 2},
			why:     "D(Hub,R,OUT) is consumed twice — as the written cost AND as the probe depth",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := seedReverseHubGraph(t)
			setupEng := NewEngineWithOptions(g, tc.opts)
			for _, s := range tc.setup {
				runWrite(t, setupEng, s)
			}

			e := NewEngineWithOptions(g, tc.opts)

			before := anchorSwapBuildCount.Load()
			firstRows := drainRows(t, e, q)
			if anchorSwapBuildCount.Load() == before {
				t.Fatalf("expected the reverse-introducing swap to fire on the CLEAN run, but it " +
					"did not — without that this case cannot show the veto")
			}

			runWrite(t, e, tc.relabel)

			before = anchorSwapBuildCount.Load()
			secondRows := drainRows(t, e, q)
			if anchorSwapBuildCount.Load() != before {
				t.Fatalf("expected the reverse-introducing swap to be VETOED once the relabel "+
					"dirtied the cost input, but it fired (%s): an inexact input means the "+
					"candidate is not costable and the written order must stand", tc.why)
			}
			if len(firstRows) != len(secondRows) {
				t.Fatalf("the relabel changed the result (%d rows then %d), so this case no "+
					"longer isolates the plan change from a result change",
					len(firstRows), len(secondRows))
			}
			for i := range firstRows {
				if firstRows[i] != secondRows[i] {
					t.Fatalf("row %d differs across the relabel: %s then %s",
						i, firstRows[i], secondRows[i])
				}
			}
		})
	}
}

// TestAnchorSwapSymmetric_OrderSafety covers the suppression cases for the IN-ward
// direction. A swap re-roots the scan and therefore PERMUTES the emitted rows, so
// SuppressReorder is load-bearing here in a way it is not for the #2149 seek, which
// emits the identical sequence.
//
// Each case names an operator that observes arrival order, so the swap must NOT fire;
// the covering-ORDER-BY case is the enabler, where a total sort masks the permutation
// and the swap is admitted again.
func TestAnchorSwapSymmetric_OrderSafety(t *testing.T) {
	g := seedReverseHubGraph(t)
	for _, tc := range []struct {
		name        string
		q           string
		wantTrigger bool
		ordered     bool
	}{
		{
			// A bare LIMIT with no dominating total sort selects WHICH rows survive, so a
			// permutation is a multiset change, not merely a reorder.
			name: "bare_limit_suppressed",
			q:    "MATCH (a:Hub)-[:R]->(b:Leaf) RETURN a.tag AS at, b.i AS bi LIMIT 1",
		},
		{
			name: "bare_skip_suppressed",
			q:    "MATCH (a:Hub)-[:R]->(b:Leaf) RETURN a.tag AS at, b.i AS bi SKIP 1",
		},
		{
			// collect() builds a list in arrival order — a value trap even under bag
			// comparison, because an unordered comparison still compares list CELLS.
			name: "collect_suppressed",
			q:    "MATCH (a:Hub)-[:R]->(b:Leaf) RETURN collect(b.i) AS c",
		},
		{
			// A total ORDER BY is the RESET enabler: it re-establishes a complete order,
			// masking the permutation, so the swap fires AND the exact row SEQUENCE is
			// identical on and off. Totality needs every flowing column pinned, so the
			// relationship is named and keyed too.
			name:        "covering_order_by_permits",
			q:           "MATCH (a:Hub)-[r:R]->(b:Leaf) RETURN a, b, r ORDER BY id(a), id(b), id(r)",
			wantTrigger: true,
			ordered:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertAnchorIdentical(t, g, tc.q, tc.wantTrigger, tc.ordered)
		})
	}
}

// TestAnchorSwapSymmetric_ProbeDepthEstimate pins the depth function's contract. The
// cost model consumes it directly, and a wrong depth is not a wrong answer but a
// wrong DECISION, which no result comparison can detect.
func TestAnchorSwapSymmetric_ProbeDepthEstimate(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{0, 0},     // nothing to probe
		{-5, 0},    // a negative estimate cannot happen, but must not produce NaN
		{1, 1},     // a single slot is one level
		{3, 2},     // ceil(log2(4))
		{4, 3},     // ceil(log2(5))
		{1023, 10}, // ceil(log2(1024)) exactly
		{1024, 11}, // ceil(log2(1025)) — one slot past a power of two costs a level
		{1025, 11},
	} {
		if got := probeDepthEstimate(tc.in); got != tc.want {
			t.Fatalf("probeDepthEstimate(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The saturation branch: a non-finite degree-sum must yield the structural maximum
	// rather than NaN, which would poison every comparison it entered.
	if got := probeDepthEstimate(math.Inf(1)); got != 64 {
		t.Fatalf("probeDepthEstimate(+Inf) = %v, want the structural maximum 64", got)
	}
}

// TestAnchorSwapSymmetric_ProbePenaltyStaysUnderOneEdge re-states, as a runtime
// assertion, the inequality the IN-ward no-regression proof depends on: the largest
// possible per-in-edge probe penalty must stay below one edge's modelled cost, so a
// true in-edge costs under twice a modelled out-edge and the 2x margin absorbs it.
//
// The same inequality is enforced at COMPILE time in anchor_swap_plan.go, which is the
// real guard — a test can be skipped. This case exists so the numbers appear in a
// failure message rather than only as a type error.
func TestAnchorSwapSymmetric_ProbePenaltyStaysUnderOneEdge(t *testing.T) {
	const maxPenalty = anchorSwapProbeCost*64 + anchorSwapProbeBase
	if maxPenalty >= anchorSwapEdgeCost {
		t.Fatalf("the probe penalty ceiling (%d = %d*64 + %d) has reached one edge's cost (%d): "+
			"the IN-ward swap's no-regression argument no longer holds, because a true in-edge "+
			"could cost more than twice a modelled out-edge and the %dx margin would not absorb it",
			maxPenalty, anchorSwapProbeCost, anchorSwapProbeBase, anchorSwapEdgeCost, anchorSwapMargin)
	}
}
