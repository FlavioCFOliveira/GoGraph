package cypher

// profile_estimate_test.go — the planner's estimate beside the measured row count
// (rmp #2765).
//
// Divergence D3 of docs/explain-profile-honesty-audit-2026-09-03.md §5 — "the
// largest gap" — was that GoGraph could show what the planner PREDICTED and what
// the query MEASURED, but never on the same line: the two lived in two tables over
// two different plans (logical vs physical), whose rows do not correspond one to
// one. A reader therefore could not tell a plan chosen on a good guess from one
// chosen on a bad guess.
//
// These gates hold the fix, and each one rules out a different wrong
// implementation:
//
//   - the two columns are ADJACENT and in the incumbents' order (estimate left of
//     measurement), because a comparison a reader has to make across a table is not
//     the comparison this task exists to enable;
//   - a plan whose estimate is RIGHT shows the two agreeing, and a plan whose
//     estimate is WRONG shows them visibly apart on one line — which is the signal,
//     so a rendering that could only ever show agreement would be worthless;
//   - an operator with no estimate renders "-" and never a number, and an
//     approximate one keeps its tilde, because a fabricated or unqualified figure is
//     the defect this whole surface exists to prevent;
//   - the collection costs the ordinary Run path nothing, and EXPLAIN still
//     executes nothing.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// estTableColumn returns the index of the column whose header cell is exactly
// header, within a row split on "|" after the outer bars are trimmed.
//
// Cells are located by NAME rather than by position because two of this table's
// columns are conditional — Removed (rmp #2764) and Est.Rows (rmp #2765) — so a
// counted index silently reads a different column depending on what the plan
// contains, and a harness that changes what it asserts is worse than one that
// fails. ok is false when there is no header row or no such column.
func estTableColumn(table, header string) (int, bool) {
	for _, l := range strings.Split(table, "\n") {
		if !strings.HasPrefix(l, "| Operator") {
			continue
		}
		for i, c := range strings.Split(strings.Trim(l, "|"), "|") {
			if strings.TrimSpace(c) == header {
				return i, true
			}
		}
		return 0, false
	}
	return 0, false
}

// estTableCell returns the trimmed cell in column col of the first data row whose
// Operator cell contains operator, and whether such a row was found.
//
// found=false is a HARNESS failure — the query did not plan what the test believed
// — and every caller reports it as such rather than treating a missing row as a
// satisfied assertion.
func estTableCell(table, operator string, col int) (string, bool) {
	for _, l := range strings.Split(table, "\n") {
		if !strings.HasPrefix(l, "|") || strings.Contains(l, "| Operator") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) <= col || !strings.Contains(cells[0], operator) {
			continue
		}
		return strings.TrimSpace(cells[col]), true
	}
	return "", false
}

// estProfileTable runs ProfileTable and fails the test on any error.
func estProfileTable(t *testing.T, e *Engine, q string) string {
	t.Helper()
	out, err := e.ProfileTable(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("ProfileTable(%q): %v", q, err)
	}
	return out
}

// TestProfileEstimate_EstimateSitsBesideTheMeasurementAndAgreesWhenExact is the
// headline acceptance gate: for a plan whose estimate is an exact maintained count,
// the prediction and the outcome appear in ADJACENT columns of one table and agree.
//
// Three separate assertions, because a single "the number 25 appears twice" check
// would pass on a table that printed the row count in both columns:
//
//   - Est.Rows exists and sits IMMEDIATELY LEFT of Rows, which is the arrangement
//     Neo4j chose (renderAsTreeTable.scala:212 orders ESTIMATED_ROWS before ROWS,
//     asserted verbatim at RenderAsTreeTableTest.scala:277, 5.26.16) and the reason
//     the pairing is legible at all;
//   - the scan's two cells are both the seeded label count, so the estimate is the
//     planner's exact count and not a copy of anything;
//   - the operator ABOVE it has no estimate and still reports rows, which proves the
//     two columns are independently sourced.
func TestProfileEstimate_EstimateSitsBesideTheMeasurementAndAgreesWhenExact(t *testing.T) {
	const n = 25
	e, _, _ := seedPersonGraph(t, n, 0.0)
	table := estProfileTable(t, e, "MATCH (p:Person) RETURN p")

	estCol, okEst := estTableColumn(table, "Est.Rows")
	rowsCol, okRows := estTableColumn(table, "Rows")
	if !okEst || !okRows {
		t.Fatalf("ProfileTable has no Est.Rows and Rows header pair:\n%s", table)
	}
	if estCol != rowsCol-1 {
		t.Errorf("Est.Rows is column %d and Rows is column %d; the estimate must sit "+
			"IMMEDIATELY LEFT of the measurement so the two read as a pair on one line — "+
			"Neo4j orders them that way (renderAsTreeTable.scala:212, 5.26.16) and a "+
			"reader who has to scan across the table for the comparison has not been "+
			"given it:\n%s", estCol, rowsCol, table)
	}

	gotEst, foundEst := estTableCell(table, "NodeByLabelScan", estCol)
	gotRows, foundRows := estTableCell(table, "NodeByLabelScan", rowsCol)
	if !foundEst || !foundRows {
		t.Fatalf("no NodeByLabelScan row in the table, so this gate covers nothing:\n%s", table)
	}
	want := strconv.Itoa(n)
	if gotEst != want {
		t.Errorf("the scan's Est.Rows cell is %q, want %q — the label's live count is a "+
			"maintained EXACT figure, so it must render as a bare number with no "+
			"approximation marker:\n%s", gotEst, want, table)
	}
	if gotRows != want {
		t.Errorf("the scan's Rows cell is %q, want %q; the fixture no longer has %d "+
			"labelled nodes and the agreement below would be measured against the wrong "+
			"input:\n%s", gotRows, want, n, table)
	}

	// The projection above the scan has no estimate. If it printed one — or printed
	// its row count into the estimate column — the two columns would not be
	// independently sourced and the agreement asserted above would prove nothing.
	projEst, foundProj := estTableCell(table, "Project", estCol)
	if !foundProj {
		t.Fatalf("no Project row in the table:\n%s", table)
	}
	if projEst != exec.EstRowsUnknown {
		t.Errorf("the Project's Est.Rows cell is %q, want %q. No logical node the "+
			"planner estimates lowers to that operator, so there is no figure — and a "+
			"column that filled it in from the row count beside it would make every "+
			"agreement in this table unfalsifiable:\n%s", projEst, exec.EstRowsUnknown, table)
	}
}

// seedSkewedGroupGraph builds a graph whose `grp` property is skewed in a way the
// planner's statistics CANNOT capture, so the estimate for one value is provably
// and visibly wrong.
//
// The construction is deliberate. The most-common-value list is an EXACT top-k with
// k = 32 (graph/index/stats/doc.go:20), so a value inside it is estimated exactly
// and no disagreement is possible. The fixture therefore fills all 32 slots with
// values MORE common than the one queried:
//
//	hot0 … hot31   100 nodes each   → the 32 MCV slots
//	warm            90 nodes        → the 33rd most common: NOT an MCV
//	cold0 … cold399  1 node each    → distinct values, to inflate the NDV
//
// `warm` therefore falls to the 1/NDV × N average, which with ~433 distinct values
// over 3690 rows predicts single digits for a value that occurs 90 times. Both
// numbers are real: the estimate is what the planner actually derived, and the row
// count is what the query actually returned.
func seedSkewedGroupGraph(t *testing.T) (e *Engine, warmCount int) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	add := func(key, grp string) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "grp", lpg.StringValue(grp)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", key, err)
		}
	}
	for h := 0; h < 32; h++ {
		for i := 0; i < 100; i++ {
			add(fmt.Sprintf("hot%d-%d", h, i), fmt.Sprintf("hot%d", h))
		}
	}
	warmCount = 90
	for i := 0; i < warmCount; i++ {
		add(fmt.Sprintf("warm-%d", i), "warm")
	}
	for i := 0; i < 400; i++ {
		add(fmt.Sprintf("cold-%d", i), fmt.Sprintf("cold%d", i))
	}
	e = NewEngine(g)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	return e, warmCount
}

// TestProfileEstimate_AWrongEstimateIsVisiblyWrongOnItsOwnLine is the gate the
// whole task turns on: the pairing must be able to EXPOSE a bad guess, not merely
// display a good one.
//
// A rendering that could only ever show the two columns agreeing would be
// decoration. Here the planner has no most-common-value entry for the queried value
// and falls back to the 1/NDV average, which under-predicts by an order of
// magnitude — and the table says so, on one line, without arithmetic.
func TestProfileEstimate_AWrongEstimateIsVisiblyWrongOnItsOwnLine(t *testing.T) {
	e, warmCount := seedSkewedGroupGraph(t)
	table := estProfileTable(t, e, `MATCH (p:Person) WHERE p.grp = 'warm' RETURN p`)

	estCol, okEst := estTableColumn(table, "Est.Rows")
	rowsCol, okRows := estTableColumn(table, "Rows")
	if !okEst || !okRows {
		t.Fatalf("ProfileTable has no Est.Rows and Rows header pair:\n%s", table)
	}
	estCell, foundEst := estTableCell(table, "Filter", estCol)
	rowsCell, foundRows := estTableCell(table, "Filter", rowsCol)
	if !foundEst || !foundRows {
		t.Fatalf("no Filter row in the table, so this gate covers nothing:\n%s", table)
	}

	// The measurement half must be the real answer, or the disagreement below could
	// be a defect in the measurement rather than in the estimate.
	if rowsCell != strconv.Itoa(warmCount) {
		t.Fatalf("the Filter's Rows cell is %q, want %q — the fixture no longer has %d "+
			"'warm' nodes:\n%s", rowsCell, strconv.Itoa(warmCount), warmCount, table)
	}
	// The estimate half must be an APPROXIMATION, marked as one. An exact tag here
	// would mean the value reached the most-common-value list after all, and the
	// fixture would no longer be testing a fallible estimate.
	if !strings.HasPrefix(estCell, "~") {
		t.Fatalf("the Filter's Est.Rows cell is %q, want a tilde-marked approximation. "+
			"'warm' must fall outside the exact 32-entry most-common-value list for this "+
			"gate to exercise a fallible estimate at all:\n%s", estCell, table)
	}
	estN, err := strconv.ParseInt(strings.TrimPrefix(estCell, "~"), 10, 64)
	if err != nil {
		t.Fatalf("the Filter's Est.Rows cell %q is not a number:\n%s", estCell, table)
	}
	if estN*3 >= int64(warmCount) {
		t.Errorf("the estimate (%d) and the measurement (%d) are close, so this gate no "+
			"longer shows that a WRONG estimate is visible. The fixture's whole purpose is "+
			"a predicate the 1/NDV fallback badly under-predicts:\n%s", estN, warmCount, table)
	}

	// And the two must be on ONE line, which is what makes the discrepancy legible
	// without cross-referencing a second table over a different plan (divergence D3).
	for _, l := range strings.Split(table, "\n") {
		if !strings.Contains(l, "Filter") || strings.Contains(l, "| Operator") {
			continue
		}
		if !strings.Contains(l, estCell) || !strings.Contains(l, rowsCell) {
			t.Errorf("the Filter's estimate %q and measurement %q are not both on its own "+
				"line %q:\n%s", estCell, rowsCell, l, table)
		}
		return
	}
	t.Fatalf("no Filter line found for the one-line assertion:\n%s", table)
}

// TestProfileEstimate_StaleStatisticRendersDashNotAFabricatedNumber holds the
// honesty half against the state that is easiest to get wrong: a statistic that
// EXISTS but has drifted past its firing threshold.
//
// The estimate provider demotes it to estFallback, which still carries a number —
// and printing that number would be the exact fabrication this surface exists to
// prevent, because it would look like every other estimate while standing on data
// the engine has already disowned. Both renderings must show the absence.
func TestProfileEstimate_StaleStatisticRendersDashNotAFabricatedNumber(t *testing.T) {
	const n = 400
	e, _, _ := seedPersonGraph(t, n, 0.30)
	ctx := context.Background()
	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	// Fresh: the range predicate is estimated from the histogram, so the cell is a
	// tilde-marked approximation. Without this arm the assertion below would pass on
	// a build that never estimated a range at all.
	const q = "MATCH (p:Person) WHERE p.age < 30 RETURN p"
	fresh := estProfileTable(t, e, q)
	estCol, ok := estTableColumn(fresh, "Est.Rows")
	if !ok {
		t.Fatalf("no Est.Rows column in the fresh table:\n%s", fresh)
	}
	freshCell, found := estTableCell(fresh, "Filter", estCol)
	if !found {
		t.Fatalf("no Filter row in the fresh table:\n%s", fresh)
	}
	if !strings.HasPrefix(freshCell, "~") {
		t.Fatalf("the fresh range estimate is %q, want a tilde-marked approximation — a "+
			"histogram estimate is not exact and must not render as one:\n%s", freshCell, fresh)
	}

	// Drive engine SET writes past the Δ/N firing threshold, exactly as
	// TestExplainEstimate_StaleStatFallback does for the logical rendering.
	threshold := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	crossAt := int64(threshold*float64(n)) + 1
	for i := int64(0); i < crossAt+2; i++ {
		if _, err := e.RunInTx(ctx, fmt.Sprintf(
			"MATCH (p:Person {name:'name-%d'}) SET p.age = %d", i, 200+i), nil); err != nil {
			t.Fatalf("SET write %d: %v", i, err)
		}
	}

	stale := estProfileTable(t, e, q)
	estCol, ok = estTableColumn(stale, "Est.Rows")
	if !ok {
		t.Fatalf("no Est.Rows column in the stale table; the label scan still has an "+
			"exact estimate, so the column must still exist:\n%s", stale)
	}
	staleCell, found := estTableCell(stale, "Filter", estCol)
	if !found {
		t.Fatalf("no Filter row in the stale table:\n%s", stale)
	}
	if staleCell != exec.EstRowsUnknown {
		t.Errorf("the stale range estimate rendered %q, want %q. The statistic behind it "+
			"has drifted past its firing threshold and the engine has already disowned it; "+
			"printing its number would put a figure the planner will not act on beside a "+
			"measurement, which reads as a planner error that never happened:\n%s",
			staleCell, exec.EstRowsUnknown, stale)
	}
	// The label scan's count is unaffected by property staleness, so its exact
	// estimate must survive. Without this the test would pass on a build that
	// dropped every estimate the moment anything went stale.
	scanCell, found := estTableCell(stale, "NodeByLabelScan", estCol)
	if !found {
		t.Fatalf("no NodeByLabelScan row in the stale table:\n%s", stale)
	}
	if scanCell != strconv.Itoa(n) {
		t.Errorf("the scan's Est.Rows cell is %q, want %q: a stale PROPERTY statistic does "+
			"not make the label's live count stale, and demoting it too would hide a figure "+
			"that is still exactly right:\n%s", scanCell, strconv.Itoa(n), stale)
	}
}

// TestProfileEstimate_ColumnIsAbsentWhenNothingWasEstimated holds Neo4j's rule for
// an argument no plan node carries: the column is dropped, not filled with dashes
// (renderAsTreeTable.scala:47, 5.26.16, `Header.ALL.filter(columnLengths.contains)`).
//
// It matters because it is what keeps every existing reader of this table seeing
// the columns it always had for a plan that has nothing to say about estimates.
func TestProfileEstimate_ColumnIsAbsentWhenNothingWasEstimated(t *testing.T) {
	e, _, _ := seedPersonGraph(t, 40, 0.0)

	// A labelled count is answered by the count store: the aggregation subsumes the
	// scan, so no operator in the physical plan is the lowering of a logical node the
	// planner estimated.
	countTable := estProfileTable(t, e, "MATCH (p:Person) RETURN count(p) AS c")
	if _, ok := estTableColumn(countTable, "Est.Rows"); ok {
		t.Errorf("a plan in which nothing was estimated still rendered an Est.Rows "+
			"column. It must be omitted entirely, so a reader of a plan with no estimates "+
			"sees exactly the columns this table always had:\n%s", countTable)
	}
	if !strings.Contains(countTable, "LabelCountScan") {
		t.Fatalf("the count query did not plan a LabelCountScan, so this gate is not "+
			"testing the estimate-free plan it believes it is:\n%s", countTable)
	}

	// The control: a plan that DOES estimate something renders the column. Without it
	// this test would pass on a build that never rendered the column at all.
	scanTable := estProfileTable(t, e, "MATCH (p:Person) RETURN p")
	if _, ok := estTableColumn(scanTable, "Est.Rows"); !ok {
		t.Errorf("a plan whose scan carries an exact estimate rendered NO Est.Rows "+
			"column, so the omission above proves nothing:\n%s", scanTable)
	}
}

// TestProfileEstimate_PhysicalAndLogicalAgreeAboutTheSameNode is the soundness gate
// for the MAPPING itself.
//
// The physical estimate is not a second estimation model: it is the number the
// logical walk derives for the logical node whose lowering produced the operator.
// This asserts that identity end to end, on the two shapes where the correspondence
// is exact — a labelled scan, and a Selection over one — so a future change that
// starts deriving physical estimates independently fails here rather than silently
// putting two different numbers on two surfaces describing one plan.
func TestProfileEstimate_PhysicalAndLogicalAgreeAboutTheSameNode(t *testing.T) {
	const n = 3000
	e, heavyCount, _ := seedPersonGraph(t, n, 0.40)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	const q = "MATCH (p:Person) WHERE p.age = 30 RETURN p"

	logical, err := e.ExplainTable(q, nil)
	if err != nil {
		t.Fatalf("ExplainTable: %v", err)
	}
	physical := estProfileTable(t, e, q)

	lEst, okL := estTableColumn(logical, "Est.Rows")
	pEst, okP := estTableColumn(physical, "Est.Rows")
	if !okL || !okP {
		t.Fatalf("both tables must carry an Est.Rows column\nlogical:\n%s\nphysical:\n%s", logical, physical)
	}
	for _, pair := range []struct{ logicalOp, physicalOp string }{
		{"NodeByLabelScan", "NodeByLabelScan"},
		{"Selection", "Filter"},
	} {
		lCell, foundL := estTableCell(logical, pair.logicalOp, lEst)
		pCell, foundP := estTableCell(physical, pair.physicalOp, pEst)
		if !foundL || !foundP {
			t.Fatalf("missing row: logical %q found=%v, physical %q found=%v\nlogical:\n%s\nphysical:\n%s",
				pair.logicalOp, foundL, pair.physicalOp, foundP, logical, physical)
		}
		if lCell != pCell {
			t.Errorf("the logical %s renders Est.Rows %q and the physical %s renders %q. "+
				"They describe ONE node of ONE plan and there is only one estimate for it; "+
				"two different numbers mean a second estimation model has appeared:\n"+
				"logical:\n%s\nphysical:\n%s", pair.logicalOp, lCell, pair.physicalOp, pCell, logical, physical)
		}
	}
	// Non-vacuity: the shared figure must be the real MCV count, not "-" on both
	// sides, which would make the equality above trivially true.
	lCell, _ := estTableCell(logical, "Selection", lEst)
	if lCell != strconv.FormatInt(heavyCount, 10) {
		t.Errorf("the Selection's estimate is %q, want the exact most-common-value count "+
			"%d; if both surfaces rendered %q the agreement asserted above would be vacuous",
			lCell, heavyCount, exec.EstRowsUnknown)
	}
}

// TestProfileEstimate_ExplainCollectsEstimatesAndStillExecutesNothing holds the
// property the estimate collection could most plausibly have broken.
//
// Collecting estimates makes the EXPLAIN build read the count store, the label
// index and the statistics collector. Those are reads, but EXPLAIN's contract is
// that NOTHING happens — and the control arm is a statement that would be
// catastrophic if it ran. The non-vacuity half matters just as much: an EXPLAIN
// that collected no estimates would also mutate nothing, and would satisfy a
// weaker version of this test while delivering none of the feature.
func TestProfileEstimate_ExplainCollectsEstimatesAndStillExecutesNothing(t *testing.T) {
	const n = 60
	e, _, _ := seedPersonGraph(t, n, 0.0)
	g := e.g
	beforeOrder := g.LiveOrder()

	// The subject: a read whose plan carries estimates.
	plan, err := e.Explain("MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	want := fmt.Sprintf("(est. rows=%d exact)", n)
	if !strings.Contains(plan, want) {
		t.Fatalf("the EXPLAIN rendering does not carry %q, so the no-execution assertion "+
			"below would hold for a build that collects no estimates at all:\n%s", want, plan)
	}

	// The control: a statement that would delete the whole graph if EXPLAIN executed.
	if _, err := e.Explain("MATCH (p:Person) DETACH DELETE p", nil); err != nil {
		t.Fatalf("Explain(DETACH DELETE): %v", err)
	}
	if _, err := e.ExplainTable("MATCH (p:Person) DETACH DELETE p", nil); err != nil {
		t.Fatalf("ExplainTable(DETACH DELETE): %v", err)
	}
	if order := g.LiveOrder(); order != beforeOrder {
		t.Fatalf("EXPLAIN changed the graph: %d live nodes before, %d after. A "+
			"diagnostic that executes its own control arm is not a diagnostic",
			beforeOrder, order)
	}
}

// TestProfileEstimate_RunCarriesNoEstimates is the cost-when-off gate.
//
// The estimate providers read the count store, the label index and the statistics
// collector, and none of that may happen on the query path. The collection is
// keyed on a map the caller supplies, so a build that is handed none does not
// consult a provider — and the observable consequence is that the captured tree
// carries no estimate at all.
//
// It is asserted against the BUILDER rather than against a rendering because a
// rendering could hide the work: the estimate might be computed and then not
// printed, which is precisely the cost the ULTRA EFFICIENT mandate forbids.
func TestProfileEstimate_RunCarriesNoEstimates(t *testing.T) {
	const n = 30
	e, _, _ := seedPersonGraph(t, n, 0.0)
	ctx := context.Background()
	entry, _, err := e.parseAndAnalyse("MATCH (p:Person) RETURN p")
	if err != nil {
		t.Fatalf("parseAndAnalyse: %v", err)
	}
	snap := e.g.BeginRead()
	defer e.g.EndRead(snap)
	queryReg := newNowAwareRegistry(e.reg, time.Now())

	// The Run path: no estimates map, so nothing is collected.
	op, _, err := e.buildReadPhysical(ctx, entry, entry.plan, nil, queryReg, nil, snap, nil)
	if err != nil {
		t.Fatalf("buildReadPhysical (run path): %v", err)
	}
	bare := exec.PlanTree(op)
	if countEstimatedNodes(&bare) != 0 {
		t.Errorf("a build given no estimates map still produced %d estimated nodes:\n%s",
			countEstimatedNodes(&bare), exec.RenderPlanNode(&bare))
	}

	// The control: the same build WITH a map collects them. Without it this test
	// would pass on a build that never collects estimates on any path.
	est := planEstimatesFor(entry.plan)
	op2, _, err := e.buildReadPhysical(ctx, entry, entry.plan, nil, queryReg, nil, snap, est)
	if err != nil {
		t.Fatalf("buildReadPhysical (explain path): %v", err)
	}
	withEst := exec.PlanTreeWithEstimates(op2, est)
	if countEstimatedNodes(&withEst) == 0 {
		t.Fatalf("a build given an estimates map collected none, so the assertion above "+
			"proves nothing:\n%s", exec.RenderPlanNode(&withEst))
	}
}

// countEstimatedNodes counts the nodes of a captured tree that carry an estimate.
func countEstimatedNodes(n *exec.PlanNode) int {
	c := 0
	if n.Est.Source.Known() {
		c++
	}
	for i := range n.Children {
		c += countEstimatedNodes(&n.Children[i])
	}
	return c
}

// TestProfileEstimate_EveryEstimatedNodeShapeReachesThePhysicalPlan covers the
// three logical node types the two headline gates above do not reach.
//
// Four node types carry an estimate, and each is a separate SITE in
// physicalPlanEstimate. Deleting one changes nothing a test of the other three can
// see, so each needs a query that plans it: a Selection is covered above, and an
// AllNodesScan, a NodeByLabelScan and an Expand are covered here.
//
// The Expand arm carries the additional property that a genuine estimate of ZERO
// renders "0" and not "-". An operator the planner expects to emit no rows has a
// real estimate, and usually the interesting one; collapsing it into the
// no-estimate glyph would hide exactly the prediction a reader of an empty result
// most wants to check.
func TestProfileEstimate_EveryEstimatedNodeShapeReachesThePhysicalPlan(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := NewEngine(g)
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()
	if _, err := e.RunInTx(ctx, `CREATE (a:Person {n:'a'}), (b:Person {n:'b'}),
		(c:Person {n:'c'}), (d:Person {n:'d'}),
		(a)-[:KNOWS]->(b), (b)-[:KNOWS]->(c), (c)-[:KNOWS]->(d)`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	for _, arm := range []struct {
		name     string
		query    string
		operator string
		want     string
		why      string
	}{
		{
			name: "AllNodesScan", query: "MATCH (n) RETURN n",
			operator: "AllNodesScan", want: "4",
			why: "the exact live-node total; an unlabelled scan reads every live node",
		},
		{
			name: "NodeByLabelScan", query: "MATCH (n:Person) RETURN n",
			operator: "NodeByLabelScan", want: "4",
			why: "the label's exact live count from the label index",
		},
		{
			name: "Expand", query: "MATCH (n:Person)-[:KNOWS]->(m) RETURN n, m",
			operator: "Expand", want: "3",
			why: "the exact count-store degree D(Person, KNOWS, Out) — the rows the " +
				"expansion emits when driven by every Person",
		},
		{
			name: "ExpandZero", query: "MATCH (n:Person)-[:FOLLOWS]->(m) RETURN n, m",
			operator: "Expand", want: "0",
			why: "a degree cell of zero is a REAL estimate and renders \"0\"; rendering " +
				"it as \"-\" would hide the planner's prediction that nothing matches",
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			table := estProfileTable(t, e, arm.query)
			col, ok := estTableColumn(table, "Est.Rows")
			if !ok {
				t.Fatalf("no Est.Rows column for %q:\n%s", arm.query, table)
			}
			got, found := estTableCell(table, arm.operator, col)
			if !found {
				t.Fatalf("%q did not plan a %s, so this gate covers nothing:\n%s",
					arm.query, arm.operator, table)
			}
			if got != arm.want {
				t.Errorf("the %s's Est.Rows cell is %q, want %q — %s:\n%s",
					arm.operator, got, arm.want, arm.why, table)
			}
		})
	}
}

// TestProfileEstimate_AnUnderivableShapeRendersDashNotZero holds the boundary
// between "the planner predicted nothing" and "the planner predicted zero".
//
// A Selection whose predicate the estimator does not recognise — here an IS NOT
// NULL, and a two-sided AND range for which no single-operator histogram estimate
// is defined — has NO estimate. The estimate helpers report that by returning
// ok=false, and the physical plan must carry it as an absence. Turning that into a
// number would put a confident-looking "0" beside a measured 200, which reads as a
// catastrophic planner error rather than as the silence it is.
func TestProfileEstimate_AnUnderivableShapeRendersDashNotZero(t *testing.T) {
	const n = 200
	e, _, _ := seedPersonGraph(t, n, 0.30)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	for _, arm := range []struct{ name, query string }{
		{"is-not-null", "MATCH (p:Person) WHERE p.name IS NOT NULL RETURN p"},
		{"two-sided-range", "MATCH (p:Person) WHERE p.age > 1 AND p.age < 5 RETURN p"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			table := estProfileTable(t, e, arm.query)
			col, ok := estTableColumn(table, "Est.Rows")
			if !ok {
				t.Fatalf("no Est.Rows column (the label scan alone should supply one):\n%s", table)
			}
			got, found := estTableCell(table, "Filter", col)
			if !found {
				t.Fatalf("%q did not plan a Filter:\n%s", arm.query, table)
			}
			if got != exec.EstRowsUnknown {
				t.Errorf("the Filter's Est.Rows cell is %q, want %q. The estimator does not "+
					"recognise this predicate shape and reports no estimate; publishing a "+
					"number for it fabricates a prediction the planner never made:\n%s",
					got, exec.EstRowsUnknown, table)
			}
		})
	}
}

// noGraphWalker is a node walker that is not the engine's own, used to reach the
// lowering branch that builds a constant-true pass-through Filter.
type noGraphWalker struct{}

func (noGraphWalker) WalkNodeIDs(func(graph.NodeID) bool) {}

// TestPlanEstimate_AttributionRules pins the two rules that make the
// logical-to-physical mapping SOUND, at the level they are implemented.
//
// They are asserted here rather than through a planned query because no query the
// engine can plan today reaches either one, and that is a measured statement
// rather than an assumption. Three lowerings return an operator they did not build
// — a Selection whose pushed seek hint no seek claimed, a Selection over a
// shortestPath, and an Expand built without a graph — and in all three the parent
// Selection's own estimate is structurally unavailable: a dropped hint's predicate
// is a correlated key equality (a variable, not a literal, on the far side), and
// the other two have a child that is not a scan leaf, so selectionEstimate declines
// before the rule can matter. Applying the mutation that removes first-claim-wins
// and re-running the suite therefore changes no rendered plan.
//
// The rules are still load-bearing, and this is why they are gated: they are what
// stops a FOURTH such lowering — or a widening of selectionEstimate — from
// silently putting a filtered estimate on an operator that filters nothing. A rule
// with no gate is a rule that can be deleted without anything going red.
func TestPlanEstimate_AttributionRules(t *testing.T) {
	const n = 600
	e, _, _ := seedPersonGraph(t, n, 0.0)
	// The statistics have to exist, or the Selection below carries no estimate and
	// every assertion in this test is satisfied by two empty claims.
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	src := &lpgLabelResolver{g: e.g.ReadAt(nil), eng: e}
	walker := &lpgNodeWalker{g: e.g.ReadAt(nil)}

	scan := ir.NewNodeByLabelScan("p", "Person")
	// A Selection the estimator CAN estimate, over that scan. Its estimate is the
	// count the predicate selects, which is emphatically not the count the scan
	// emits — `name` is distinct per node, so the 1/NDV fallback predicts about one
	// row against the scan's exact n.
	sel := ir.NewSelectionExpr("p.name = 'name-0'", &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "p"}, Key: "name"},
		Operator: "=",
		Right:    &ast.StringLiteral{Value: "name-0"},
	}, scan)
	selEst := physicalPlanEstimate(sel, walker, src, nil)
	if !selEst.Source.Known() || selEst.Rows == int64(n) {
		t.Fatalf("the fixture Selection's estimate is %+v; it must be a KNOWN estimate "+
			"DIFFERENT from the scan's %d, or a parent that wrongly re-attributed it "+
			"would be indistinguishable from one that did not", selEst, n)
	}

	t.Run("first claim wins", func(t *testing.T) {
		m := make(exec.PlanEstimates)
		c := &planEstimateCollector{into: m, src: src}
		op := exec.NewNodeByLabelScan("Person", &execLabelAdapter{labelSrc: src})

		recordPlanEstimate(c, scan, op, walker, nil)
		first := m[op]
		if !first.Source.Known() || first.Rows != n {
			t.Fatalf("the scan's own claim is %+v, want an exact %d; without it the "+
				"assertion below cannot distinguish the two claims", first, n)
		}
		// The parent now returns the SAME operator, as a pass-through lowering does.
		recordPlanEstimate(c, sel, op, walker, nil)
		if got := m[op]; got != first {
			t.Errorf("a second logical node re-attributed an already-claimed operator: "+
				"%+v became %+v. The deepest node whose lowering RETURNS an operator owns "+
				"it; a parent that merely passes its child through built nothing, and its "+
				"estimate describes rows that operator does not emit", first, got)
		}
	})

	t.Run("an unestimated node still claims its operator", func(t *testing.T) {
		m := make(exec.PlanEstimates)
		c := &planEstimateCollector{into: m, src: src}
		op := exec.NewSingleRowOperator()

		// A node type with no estimate: the claim it writes is EMPTY, and its whole
		// purpose is to mark the operator as spoken for.
		recordPlanEstimate(c, ir.NewProduceResults(nil, scan), op, walker, nil)
		claim, present := m[op]
		if !present {
			t.Fatalf("an operator whose logical node has no estimate was left UNCLAIMED. " +
				"The empty claim is what tells a pass-through parent the operator is " +
				"already spoken for; without it the parent takes it")
		}
		if claim.Source.Known() {
			t.Fatalf("the empty claim carries an estimate %+v; it must carry none", claim)
		}
		recordPlanEstimate(c, sel, op, walker, nil)
		if got := m[op]; got.Source.Known() {
			t.Errorf("a parent attributed its estimate %+v to an operator a deeper node "+
				"had claimed with no estimate", got)
		}
	})

	t.Run("a pass-through Filter is not given the predicate's estimate", func(t *testing.T) {
		// The lowering builds a constant-true Filter when the walker carries no graph,
		// so the operator applies no predicate at all. Attributing the Selection's
		// selective estimate to it would be the most misleading fabrication available:
		// a small number beside a large measurement, looking like a planner error.
		if got := physicalPlanEstimate(sel, noGraphWalker{}, src, nil); got.Source.Known() {
			t.Errorf("a Selection lowered against a walker with no graph was given the "+
				"estimate %+v; that lowering evaluates no predicate and emits its child's "+
				"rows unchanged", got)
		}
		// The control is selEst above: the same Selection against the real walker IS
		// estimated, so the assertion here is about the walker and not about an
		// inestimable predicate.
	})
}
