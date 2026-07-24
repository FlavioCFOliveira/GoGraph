package cypher

// parallel_aggregate_diff_test.go — engine-level differential + determinism tests
// for the morsel-parallel aggregate scan (#2111): min / max / count and their
// GROUP BY forms.
//
// Each test runs a query with the parallel aggregate path ENABLED (threshold
// lowered so it engages on the small test graph) and DISABLED, and asserts a
// BYTE-IDENTICAL result. The load-bearing cases embed a mixed int/float extremum
// tie whose two members render differently ("9007199254740992" vs
// "9.007199254740992e+15") and a ±0.0 tie, so a value-only combine — one that did
// not carry the scan position of the extremum — would diverge the moment the
// non-first member won a partition. A diagnostic build counter confirms the
// parallel aggregate path actually engaged, so the test cannot pass by silently
// staying serial.

import (
	"fmt"
	"math"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pow2_53 is 2^53: the smallest magnitude at which an int64 and its float64 image
// render differently ("9007199254740992" vs "9.007199254740992e+15") while still
// comparing EQUAL under the openCypher Number-tier total order — the canonical
// String-distinguishable mixed-type tie.
const pow2_53 = int64(1) << 53

// addAggNode inserts one :Item node named k with property v (an lpg PropertyValue)
// and group property g. Insertion order fixes the scan order, hence which member
// of a Compare-tie is first-seen.
func addAggNode(t *testing.T, g *lpg.Graph[string, float64], k string, v lpg.PropertyValue, grp int64) {
	t.Helper()
	if err := g.AddNode(k); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel(k, "Item"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty(k, "v", v); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty(k, "g", lpg.Int64Value(grp)); err != nil {
		t.Fatal(err)
	}
}

// assertAggParallelDiff runs q on the parallel-enabled and parallel-disabled
// engines, asserts the sorted result multisets are byte-identical, and asserts the
// parallel aggregate path engaged for the enabled run (never for the disabled run).
func assertAggParallelDiff(t *testing.T, on, off *Engine, q string) []string {
	t.Helper()
	before := parallelAggregateScanBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	engaged := parallelAggregateScanBuildCount.Load() > before
	if !engaged {
		t.Fatalf("expected the parallel aggregate scan to engage for %q, but it did not", q)
	}
	beforeOff := parallelAggregateScanBuildCount.Load()
	gotOff := drainSortedPS(t, off, q)
	if parallelAggregateScanBuildCount.Load() != beforeOff {
		t.Fatalf("parallel aggregate scan unexpectedly engaged on the DISABLED engine for %q", q)
	}
	assertEqualRows(t, q, gotOn, gotOff)
	return gotOn
}

// TestParallelAggregate_ScalarTie_Differential proves scalar min/max over a graph
// whose extremum is a String-distinguishable mixed int/float tie ("9007199254740992"
// vs "9.007199254740992e+15") is byte-identical to serial. The parallel path
// collects node IDs in the SAME WalkNodeIDs order the serial scan uses and carries
// each extremum's scan position, so it keeps EXACTLY the representative serial
// keeps — whichever of the two tied members the scan sees first. This test asserts
// that byte-identity (on == off) plus that the extremum really is the distinguishable
// tie; the exact position-carrying representative under partition/worker variation
// is pinned by the exec-level TestParallelAggregateScan_TieRepresentative, which
// controls the scan order directly.
func TestParallelAggregate_ScalarTie_Differential(t *testing.T) {
	intForm := "m=" + fmt.Sprintf("%v", pow2_53)            // "m=9007199254740992"
	floatForm := "m=" + fmt.Sprintf("%v", float64(pow2_53)) // "m=9.007199254740992e+15"
	if intForm == floatForm {
		t.Fatalf("test precondition broken: int and float 2^53 render identically (%q)", intForm)
	}
	isTieForm := func(s string) bool { return s == intForm || s == floatForm }

	// --- min tie: int and float 2^53 tie at the minimum; fillers strictly above. ---
	gMin := lpg.New[string, float64](adjlist.Config{Directed: true})
	addAggNode(t, gMin, "min_float", lpg.Float64Value(float64(pow2_53)), 0)
	for i := 1; i <= 118; i++ { // fillers above the tie
		addAggNode(t, gMin, fmt.Sprintf("fill%d", i), lpg.Int64Value(pow2_53+int64(i)), int64(i%3))
	}
	addAggNode(t, gMin, "min_int", lpg.Int64Value(pow2_53), 0)
	onMin, offMin := engines(gMin)

	got := assertAggParallelDiff(t, onMin, offMin, `MATCH (n) RETURN min(n.v) AS m`)
	if len(got) != 1 || !isTieForm(got[0]) {
		t.Fatalf("min tie result = %v, want the 2^53 tie (%q or %q)", got, intForm, floatForm)
	}

	// --- max tie: int and float 2^53 tie at the maximum; fillers strictly below. ---
	gMax := lpg.New[string, float64](adjlist.Config{Directed: true})
	addAggNode(t, gMax, "max_int", lpg.Int64Value(pow2_53), 0)
	for i := 1; i <= 118; i++ {
		addAggNode(t, gMax, fmt.Sprintf("fill%d", i), lpg.Int64Value(pow2_53-int64(i)), int64(i%3))
	}
	addAggNode(t, gMax, "max_float", lpg.Float64Value(float64(pow2_53)), 0)
	onMax, offMax := engines(gMax)

	got = assertAggParallelDiff(t, onMax, offMax, `MATCH (n) RETURN max(n.v) AS m`)
	if len(got) != 1 || !isTieForm(got[0]) {
		t.Fatalf("max tie result = %v, want the 2^53 tie (%q or %q)", got, intForm, floatForm)
	}
}

// TestParallelAggregate_SignedZeroTie_Differential proves a ±0.0 minimum tie keeps
// the first-seen signed zero byte-identically to serial.
func TestParallelAggregate_SignedZeroTie_Differential(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	addAggNode(t, g, "neg_zero", lpg.Float64Value(math.Copysign(0, -1)), 0) // first-seen -0.0
	for i := 1; i <= 118; i++ {
		addAggNode(t, g, fmt.Sprintf("fill%d", i), lpg.Float64Value(float64(i)), int64(i%3))
	}
	addAggNode(t, g, "pos_zero", lpg.Float64Value(0), 0) // ties at zero, seen later
	on, off := engines(g)

	assertAggParallelDiff(t, on, off, `MATCH (n) RETURN min(n.v) AS m`)
}

// TestParallelAggregate_GroupBy_Differential proves the grouped min/max/count forms
// are byte-identical to serial, including per-group tie representatives, group
// membership, and emission order.
func TestParallelAggregate_GroupBy_Differential(t *testing.T) {
	// Two groups, each with a mixed int/float tie at its min and max. Interleave so
	// the first-seen order of the groups (and of each tie member) is fixed.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	addAggNode(t, g, "a_min_f", lpg.Float64Value(float64(pow2_53)), 0) // group 0 min, float first
	addAggNode(t, g, "b_max_i", lpg.Int64Value(pow2_53), 1)            // group 1 max, int first
	for i := 1; i <= 60; i++ {
		addAggNode(t, g, fmt.Sprintf("a_fill%d", i), lpg.Int64Value(pow2_53+int64(i)), 0)
		addAggNode(t, g, fmt.Sprintf("b_fill%d", i), lpg.Int64Value(pow2_53-int64(i)), 1)
	}
	addAggNode(t, g, "a_min_i", lpg.Int64Value(pow2_53), 0)            // group 0 min tie, int later
	addAggNode(t, g, "b_max_f", lpg.Float64Value(float64(pow2_53)), 1) // group 1 max tie, float later
	on, off := engines(g)

	assertAggParallelDiff(t, on, off, `MATCH (n) RETURN n.g AS grp, min(n.v) AS m`)
	assertAggParallelDiff(t, on, off, `MATCH (n) RETURN n.g AS grp, max(n.v) AS m`)
	assertAggParallelDiff(t, on, off, `MATCH (n) RETURN n.g AS grp, count(*) AS c`)
	assertAggParallelDiff(t, on, off, `MATCH (n) RETURN n.g AS grp, count(n.v) AS c, min(n.v) AS lo, max(n.v) AS hi`)

	// Emission ORDER (no ORDER BY): the parallel path must reproduce the serial
	// group-creation order exactly, not merely the same multiset.
	const q = `MATCH (n) RETURN n.g AS grp, count(*) AS c`
	gotOn := drainOrderedPS(t, on, q)
	gotOff := drainOrderedPS(t, off, q)
	if len(gotOn) != len(gotOff) {
		t.Fatalf("ordered row-count mismatch: parallel=%d serial=%d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("emission ORDER differs at row %d:\n parallel = %q\n serial   = %q", i, gotOn[i], gotOff[i])
		}
	}
}

// TestParallelAggregate_WorkerSweep_Differential proves byte-identity across a
// worker-count sweep (GOMAXPROCS 1 .. 2×default) at the engine level, exercising
// the governor's worker-budget path end-to-end.
func TestParallelAggregate_WorkerSweep_Differential(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	addAggNode(t, g, "min_float", lpg.Float64Value(float64(pow2_53)), 0)
	for i := 1; i <= 200; i++ {
		addAggNode(t, g, fmt.Sprintf("fill%d", i), lpg.Int64Value(pow2_53+int64(i)), int64(i%4))
	}
	addAggNode(t, g, "min_int", lpg.Int64Value(pow2_53), 0)

	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})
	want := drainSortedPS(t, off, `MATCH (n) RETURN n.g AS grp, min(n.v) AS m`)

	for _, gomax := range []int{1, 2, 3, 5, 8, 16} {
		prev := runtime.GOMAXPROCS(gomax)
		on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
		got := drainSortedPS(t, on, `MATCH (n) RETURN n.g AS grp, min(n.v) AS m`) // already sorted
		runtime.GOMAXPROCS(prev)
		if len(got) != len(want) {
			t.Fatalf("gomaxprocs=%d row-count mismatch: %d vs %d", gomax, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("gomaxprocs=%d row %d differs:\n parallel = %q\n serial   = %q", gomax, i, got[i], want[i])
			}
		}
	}
}
