package cypher

// parallel_scan_min_label_precedence_test.go — the morsel-parallel scan must not
// pre-empt the min-cardinality label re-anchor (rmp #2431).
//
// # What went wrong
//
// [tryBuildColumnarFilterChain] already yields to the re-anchor, and says why:
// the re-anchor reduces the number of rows SCANNED, while columnar execution only
// removes a constant factor from each scanned row. The identical argument applies
// to the morsel-parallel scan, which divides the cost of each scanned row by the
// worker count — also a constant factor — and it was NOT making it.
//
// The consequence was worse than a missed optimisation. The columnar chain is
// tried FIRST and declines exactly the `(n:A:B)` shapes the re-anchor exists for;
// [tryBuildParallelScanProject] was tried immediately after and claimed every one
// of them, anchored on Labels[0]. So the yield was INERT: it handed the shape
// straight to an operator that ignored the same rule.
//
// MEASURED on 100 000 :Common of which 1 000 also carry :Rare, running
// `MATCH (n:Common:Rare) RETURN n.k`: the default configuration took 4.611 ms
// choosing ParallelScanProject, against 0.186 ms for the serial re-anchored plan
// — and worse than the 4.280 ms legacy full-:Common scan the re-anchor replaces.
// Holding |Rare| at 1 000 and sweeping |Common|, the plan flipped at exactly
// |Common| > 50 000, the parallel threshold, which is what proved the gate was
// judging the FIRST label rather than the anchor.
//
// # What this gate asserts, and why it asserts a PLAN
//
// It compares PLANS, never wall-clock. A timing gate for this would have to
// separate a 25x planning difference from machine load, and this sprint is
// largely a cleanup of gates that could not (see #2517, #2572, #2589). The plan
// is the decision under test; the time is only its consequence.
//
// Every arm carries its own non-vacuity control, because the natural way for this
// gate to rot is for the fixture to stop being eligible for the parallel path at
// all — at which point "the plan is not parallel" becomes true for the wrong
// reason and the gate silently stops testing anything.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mlpCommon is the large label's population and mlpRare the small subset that
// also carries the second label. Both are tiny: the parallel tier's eligibility
// is controlled by lowering ParallelScanThreshold rather than by seeding past the
// 50 000 default, so the gate costs milliseconds instead of seconds and still
// exercises the same decision.
const (
	mlpCommon = 400
	mlpRare   = 20
	// mlpThreshold sits between mlpRare and mlpCommon, so the parallel tier is
	// eligible on :Common and not on :Rare — the exact relationship the 100 000 /
	// 1 000 / 50 000 fixture had.
	mlpThreshold = 100
	// mlpBigRare is above mlpThreshold, so the re-anchored label is ITSELF large
	// enough to parallelise. This is the shape the parallel scan legitimately
	// serves and must keep.
	mlpBigRare = 300
)

// mlpQuery isolates the PARALLEL tier's decision.
//
// The obvious query, `RETURN n.k`, does not: [tryBuildColumnarFilterChain] is
// tried first and claims a plain scalar-property projection whenever it does not
// yield, so disabling the re-anchor to build a control also unblocks the columnar
// chain, and the control then plans a ColumnarFilter instead of reaching the
// parallel tier at all. MEASURED — that is exactly what the first version of this
// gate did, and its control failed for that reason rather than for the one it was
// written to detect.
//
// `n.k + 1` is not a plain property access, so the columnar chain declines it at
// every setting and the parallel tier is the only operator left deciding. It is
// still scalar, so [projectionItemsAreScalar] admits it.
const mlpQuery = "MATCH (n:Common:Rare) RETURN n.k + 1 AS k"

// mlpPlainQuery is the shape the defect was REPORTED on, asserted end to end
// alongside the isolated one so the gate also covers what a user actually wrote.
const mlpPlainQuery = "MATCH (n:Common:Rare) RETURN n.k AS k"

// seedMLPGraph builds common nodes labelled :Common, of which the first rare also
// carry :Rare, each with a unique integer property.
func seedMLPGraph(t *testing.T, common, rare int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < common; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Common"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if i < rare {
			if err := g.SetNodeLabel(k, "Rare"); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if err := g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	g.SetIndexManager(index.NewManager())
	return g
}

// mlpPlanAndRows returns the physical plan for mlpQuery and the number of rows it
// actually produces, so no arm can assert a plan without also proving the plan
// answers the query.
func mlpPlanAndRows(t *testing.T, g *lpg.Graph[string, float64], opts *EngineOptions) (string, int) {
	t.Helper()
	return mlpPlanAndRowsFor(t, g, opts, mlpQuery)
}

func mlpPlanAndRowsFor(
	t *testing.T, g *lpg.Graph[string, float64], opts *EngineOptions, query string,
) (string, int) {
	t.Helper()
	opts.MaxResultRows = MaxResultRowsUnlimited
	if opts.ParallelScanThreshold == 0 {
		opts.ParallelScanThreshold = mlpThreshold
	}
	eng := NewEngineWithOptions(g, *opts)

	plan, err := eng.Explain(query, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return plan, rows
}

func mlpIsParallel(plan string) bool { return strings.Contains(plan, "ParallelScanProject") }

// TestParallelScanYieldsToMinLabelAnchor is the regression gate proper.
func TestParallelScanYieldsToMinLabelAnchor(t *testing.T) {
	g := seedMLPGraph(t, mlpCommon, mlpRare)

	// The CONTROL, asserted FIRST: with the re-anchor disabled the very same
	// fixture MUST choose the parallel plan. Without this, the assertion below
	// would also pass on a fixture the parallel tier was never eligible for, which
	// is the way this gate would rot.
	ctrlPlan, ctrlRows := mlpPlanAndRows(t, g, &EngineOptions{DisableMinLabelScan: true})
	if !mlpIsParallel(ctrlPlan) {
		t.Fatalf("the control did not reach the parallel tier, so this gate proves nothing: "+
			"with the re-anchor disabled, %d :Common nodes against a threshold of %d must plan a "+
			"ParallelScanProject, got:\n%s", mlpCommon, mlpThreshold, ctrlPlan)
	}
	if ctrlRows != mlpRare {
		t.Fatalf("the control returned %d rows, want %d", ctrlRows, mlpRare)
	}

	// The assertion: with the re-anchor ENABLED — the default — the parallel tier
	// must not claim the shape, because the anchor scans |Rare| rows where the
	// parallel plan scans |Common| of them.
	plan, rows := mlpPlanAndRows(t, g, &EngineOptions{})
	if mlpIsParallel(plan) {
		t.Errorf("the default configuration planned a ParallelScanProject over :Common (%d nodes) "+
			"when the min-cardinality re-anchor would scan :Rare (%d nodes). Parallel execution "+
			"divides the cost of each scanned row by the worker count; the re-anchor removes the "+
			"rows. A constant factor must not pre-empt a cardinality reduction (#2431). Plan:\n%s",
			mlpCommon, mlpRare, plan)
	}
	if !strings.Contains(plan, "NodeByLabelScan [Rare]") {
		t.Errorf("the default plan does not scan :Rare, so the re-anchor did not take effect:\n%s", plan)
	}
	if rows != mlpRare {
		t.Errorf("the default configuration returned %d rows, want %d", rows, mlpRare)
	}

	// Both plans must answer the query identically — the substitution is a choice
	// of strategy, never of result.
	if rows != ctrlRows {
		t.Errorf("the re-anchored plan returned %d rows and the parallel plan %d; a label "+
			"conjunction is commutative and the two must agree", rows, ctrlRows)
	}

	// And the shape the defect was REPORTED on, end to end. It reaches the same
	// decision by a different route — the columnar chain yields to the re-anchor
	// and the parallel tier must not then claim what the yield gave up — so it is
	// asserted as well as, not instead of, the isolated query above.
	plainPlan, plainRows := mlpPlanAndRowsFor(t, g, &EngineOptions{}, mlpPlainQuery)
	if mlpIsParallel(plainPlan) {
		t.Errorf("the reported query planned a ParallelScanProject over :Common (%d nodes) when "+
			"the re-anchor would scan :Rare (%d). Plan:\n%s", mlpCommon, mlpRare, plainPlan)
	}
	if !strings.Contains(plainPlan, "NodeByLabelScan [Rare]") {
		t.Errorf("the reported query does not scan :Rare:\n%s", plainPlan)
	}
	if plainRows != mlpRare {
		t.Errorf("the reported query returned %d rows, want %d", plainRows, mlpRare)
	}
}

// TestParallelScanStillWinsWhenTheAnchorIsLarge pins the other side of the rule.
// Yielding to the anchor must not mean abandoning the parallel tier: when the
// re-anchored label is ITSELF above the threshold there are enough rows to
// amortise the workers, and the parallel path must still be chosen — now over the
// smaller label.
func TestParallelScanStillWinsWhenTheAnchorIsLarge(t *testing.T) {
	g := seedMLPGraph(t, mlpCommon, mlpBigRare)

	plan, rows := mlpPlanAndRows(t, g, &EngineOptions{})
	if !mlpIsParallel(plan) {
		t.Errorf("the re-anchored label holds %d nodes, above the threshold of %d, so the parallel "+
			"tier must still serve it; yielding to the anchor means anchoring the parallel scan on "+
			"the smaller label, not declining to parallelise at all. Plan:\n%s",
			mlpBigRare, mlpThreshold, plan)
	}
	if rows != mlpBigRare {
		t.Errorf("returned %d rows, want %d", rows, mlpBigRare)
	}

	// The CONTROL for this arm: the same query on the same shape with the parallel
	// tier off must return the identical rows, so "it planned a parallel scan" is
	// never accepted on its own.
	serialPlan, serialRows := mlpPlanAndRows(t, g, &EngineOptions{DisableParallelScan: true})
	if mlpIsParallel(serialPlan) {
		t.Fatalf("DisableParallelScan still planned a parallel scan:\n%s", serialPlan)
	}
	if serialRows != rows {
		t.Errorf("the parallel plan returned %d rows and the serial plan %d", rows, serialRows)
	}
}

// TestSingleLabelScanIsUnaffectedByThePrecedenceRule pins the boundary: a pattern
// with ONE label has no conjunction to re-anchor, so the rule must not reach it
// and the parallel tier must keep serving it.
func TestSingleLabelScanIsUnaffectedByThePrecedenceRule(t *testing.T) {
	const q = "MATCH (n:Common) RETURN n.k AS k"
	g := seedMLPGraph(t, mlpCommon, 0)
	eng := NewEngineWithOptions(g, EngineOptions{
		ParallelScanThreshold: mlpThreshold,
		MaxResultRows:         MaxResultRowsUnlimited,
	})
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !mlpIsParallel(plan) {
		t.Errorf("a single-label scan of %d nodes above a threshold of %d must still reach the "+
			"parallel tier; there is no conjunction for the re-anchor to act on. Plan:\n%s",
			mlpCommon, mlpThreshold, plan)
	}

	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rows != mlpCommon {
		t.Errorf("returned %d rows, want %d", rows, mlpCommon)
	}
}
