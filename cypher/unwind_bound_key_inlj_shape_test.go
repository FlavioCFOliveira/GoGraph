package cypher

// unwind_bound_key_inlj_shape_test.go — the SPIKE probe for rmp #2813.
//
// # The question
//
// A MATCH whose equi-join key comes from UNWIND was observed rendering
// NodeByLabelScan + HashJoin rather than an index seek, with an index present on
// the label and property. #2813 asks WHICH of three candidate causes explains it:
// the cost gate ([indexNestedLoopWins]), the population floor
// ([indexNestedLoopMinPopulation]), or a structural miss in
// [tryBuildIndexNestedLoopJoin]'s trigger.
//
// # Why the probe replays the trigger instead of asserting a plan name
//
// "The seek did not fire" is one bit, and the three causes are indistinguishable
// from it. So [inljTriggerStage] walks the trigger's own conditions in the order
// tryBuildIndexNestedLoopJoin evaluates them and names the FIRST one that fails.
// That turns the counter's one bit into an attribution.
//
// The counter remains the authority on what actually ran — #2233 AC4, and #2222's
// finding that Engine.Explain can agree with the planner's intent while the built
// operator differs. Every case here reads indexNestedLoopBuildCount and
// hashJoinBuildCount, snapshotting them around the query rather than resetting
// them, and the stage walk is cross-checked against the counter in
// TestUnwindBoundKey_StageWalkAgreesWithTheCounter: a stage walk that drifted from
// the code it models would be a second opinion nobody had validated.
//
// # Why an oracle case that FIRES is mandatory here
//
// A probe that only ever reports "declined" cannot distinguish a decline from a
// blind probe. TestUnwindBoundKey_ProbeCanObserveTheSeekFiring is that control: it
// shows the counter moving on a shape the planner does admit, using the same
// fixture, the same parameter shape and the same assertion path as the cases that
// report a decline.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The two spellings under investigation. They are semantically identical
// openCypher; #2813 asks whether they lower to the same IR.
const (
	inljQueryWhere  = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN b.id AS bid`
	inljQueryInline = `UNWIND $rows AS r MATCH (b:P {age: r.a}) RETURN b.id AS bid`
)

// The same two spellings projecting a count. The grid needs them because the
// row-returning form emits B·(N/mod) rows, which trips the engine's result-row
// cap at the large-B cells and hides the plan behind an error. The aggregation
// does not change the Selection-over-Apply shape the trigger reads — asserted,
// not assumed, by TestUnwindBoundKey_CountProjectionKeepsTheTriggerShape.
const (
	inljCountWhere  = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN count(*) AS n`
	inljCountInline = `UNWIND $rows AS r MATCH (b:P {age: r.a}) RETURN count(*) AS n`
)

// inljQueryNodeReturn is #2813's query VERBATIM, projecting the node itself
// rather than a property. It is kept separate because a projection is not
// self-evidently neutral to the join shape, and the task's report is about this
// exact spelling.
const inljQueryNodeReturn = `UNWIND $rows AS r MATCH (b:P {age: r.a}) RETURN b`

// inljLowerPlan reproduces the engine's plan pipeline up to the point the
// physical builder sees: parse, ir.FromAST, then the #2182 fold, which MUTATES
// the plan in place (correlated_seek_plan.go wraps apply.Inner in a Selection).
// The fold is included because a probe that skipped it would describe an IR shape
// no builder ever receives.
func inljLowerPlan(t *testing.T, q string) ir.LogicalPlan {
	t.Helper()
	astNode, _, err := parser.ParseStatement(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	plan, err := ir.FromAST(astNode)
	if err != nil {
		t.Fatalf("translate %q: %v", q, err)
	}
	foldBoundSeekKeys(plan)
	return plan
}

// inljFindSelectionOverApply returns the first Selection whose child is an Apply
// — the shape tryBuildIndexNestedLoopJoin triggers on — or nil.
func inljFindSelectionOverApply(plan ir.LogicalPlan) (*ir.Selection, *ir.Apply) {
	var foundSel *ir.Selection
	var foundApply *ir.Apply
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil || foundSel != nil {
			return
		}
		if sel, ok := p.(*ir.Selection); ok {
			if ap, ok := sel.Child.(*ir.Apply); ok {
				foundSel, foundApply = sel, ap
				return
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return foundSel, foundApply
}

// inljTriggerStage replays tryBuildIndexNestedLoopJoin's structural and
// cost conditions in evaluation order over a lowered plan, and names the first
// one that fails. "admitted" means every condition the probe can evaluate without
// a live index manager holds.
//
// It deliberately stops short of the index conditions (findBoundNumericBTree,
// numericIndexCoversScan), which need the live graph; those are covered by the
// counter-based cases, and by the existing
// TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage.
func inljTriggerStage(plan ir.LogicalPlan, params map[string]expr.Value, labelPop int) string {
	sel, apply := inljFindSelectionOverApply(plan)
	if sel == nil {
		return "no Selection over a plain Apply (structural)"
	}
	if sel.PredicateExpr == nil {
		return "Selection has no PredicateExpr (structural)"
	}
	innerVar, innerLabel, ok := scanLeafNodeVar(apply.Inner)
	if !ok {
		return fmt.Sprintf("Apply.Inner is not a bare scan leaf, it is %s (structural)",
			ir.OperatorName(apply.Inner))
	}
	if innerLabel == "" {
		return "Apply.Inner is an unlabelled scan (structural)"
	}
	outerVars := collectPlanVars(apply.Outer)
	innerVars := collectPlanVars(apply.Inner)
	conjuncts := splitConjuncts(sel.PredicateExpr)
	keyIdx, key := findEquiJoinKey(conjuncts, outerVars, innerVars)
	if keyIdx < 0 {
		return fmt.Sprintf("no equi-join key in the predicate %q (structural)", sel.Predicate)
	}
	if _, ok := propertyKeyOf(key.innerKey, innerVar); !ok {
		return "the inner key is not exactly innerVar.<prop> (structural)"
	}
	if labelPop < indexNestedLoopMinPopulation {
		return fmt.Sprintf("population %d is below indexNestedLoopMinPopulation=%d (floor)",
			labelPop, indexNestedLoopMinPopulation)
	}
	outerRows, ok := inljEstimateOuter(apply.Outer, params)
	if !ok {
		return "no bind-time estimate for B (cost gate input missing)"
	}
	if !indexNestedLoopWins(outerRows, labelPop) {
		return fmt.Sprintf("cost gate declined at B=%d, N=%d (cost gate)", outerRows, labelPop)
	}
	return "admitted"
}

// inljEstimateOuter is estimateOuterRows restricted to the UNWIND source, which
// is the only one this probe's queries use. Using the UNWIND path directly keeps
// the probe independent of a live label resolver.
func inljEstimateOuter(arm ir.LogicalPlan, params map[string]expr.Value) (int, bool) {
	return unwindRowCount(arm, params)
}

// inljParams wraps B integer keys into the `$rows` list-of-maps shape both
// spellings read, and converts them to the expr.Value form the planner's
// bind-time estimate sees.
func inljParams(b, mod int) map[string]expr.Value {
	rows := make(expr.ListValue, 0, b)
	for i := 0; i < b; i++ {
		rows = append(rows, expr.MapValue{"a": expr.IntegerValue(int64(i % mod))})
	}
	return map[string]expr.Value{"rows": rows}
}

// inljAnyParams is the same batch in the map[string]any form RunAny takes.
func inljAnyParams(b, mod int) map[string]any {
	keys := make([]any, b)
	for i := range keys {
		keys[i] = int64(i % mod)
	}
	return map[string]any{"rows": inljKeyRows(keys)}
}

// inljObserve runs q and reports which substitution the planner actually built,
// read from the process-global counters snapshotted around the call. Per #2233
// AC4 this — not Engine.Explain — is the authority on the chosen plan.
func inljObserve(t *testing.T, eng *Engine, q string, params map[string]any) (seek, hash bool) {
	t.Helper()
	beforeSeek := indexNestedLoopBuildCount.Load()
	beforeHash := hashJoinBuildCount.Load()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	seek = indexNestedLoopBuildCount.Load() > beforeSeek
	hash = hashJoinBuildCount.Load() > beforeHash
	if seek && hash {
		t.Fatalf("both substitutions fired for %q; they must be exclusive", q)
	}
	return seek, hash
}

// TestUnwindBoundKey_ProbeCanObserveTheSeekFiring is the probe's CONTROL.
//
// Every other case in this file reports that the seek did not fire. That report is
// worthless unless the same assertion path can be shown to observe the seek
// firing, on the same fixture and the same parameter shape — otherwise "declined"
// and "my probe is blind" are the same output.
func TestUnwindBoundKey_ProbeCanObserveTheSeekFiring(t *testing.T) {
	const (
		n   = 400
		b   = 20
		mod = 20
	)
	eng := inljFixture(t, n, mod)
	seek, hash := inljObserve(t, eng, inljQueryWhere, inljAnyParams(b, mod))
	if !seek {
		t.Fatalf("the WHERE spelling did not build the index nested-loop join at B=%d, N=%d. "+
			"Until this case passes, no 'the seek was declined' result in this file can be "+
			"trusted: the probe would be blind rather than the planner declining", b, n)
	}
	if hash {
		t.Fatal("the hash join also fired")
	}
	// And the stage walk must agree that this shape is admitted, so the two
	// instruments are cross-checked on the positive case as well as the negatives.
	plan := inljLowerPlan(t, inljQueryWhere)
	if got := inljTriggerStage(plan, inljParams(b, mod), n); got != "admitted" {
		t.Fatalf("the counter says the seek fired but the stage walk says %q — the stage walk "+
			"has drifted from tryBuildIndexNestedLoopJoin", got)
	}
}

// TestUnwindBoundKey_IRShapeOfBothSpellings records the exact IR each spelling
// produces, and is the case that answers #2813's central question.
func TestUnwindBoundKey_IRShapeOfBothSpellings(t *testing.T) {
	const (
		n   = 400
		b   = 20
		mod = 20
	)
	cases := []struct {
		name  string
		query string
		// wantStage is the trigger stage this spelling must reach. Pinning it makes
		// the finding a REGRESSION GATE: were the inline map later lowered to the
		// same shape as WHERE, this case would fail and say so.
		wantStage string
	}{
		{"WHERE b.age = r.a", inljQueryWhere, "admitted"},
		{"inline map {age: r.a}", inljQueryInline, "admitted"},
		{"#2813 verbatim: inline map, RETURN b", inljQueryNodeReturn, "admitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := inljLowerPlan(t, tc.query)
			rendered := ir.Explain(plan)
			sel, apply := inljFindSelectionOverApply(plan)
			shape := "no Selection over Apply"
			if sel != nil {
				shape = fmt.Sprintf("Selection(%q) over Apply(outer=%s, inner=%s)",
					sel.Predicate, ir.OperatorName(apply.Outer), ir.OperatorName(apply.Inner))
			}
			stage := inljTriggerStage(plan, inljParams(b, mod), n)
			t.Logf("query:  %s\nIR:\n%s\ntrigger shape: %s\ntrigger stage: %s",
				tc.query, rendered, shape, stage)
			if stage != tc.wantStage {
				t.Errorf("trigger stage = %q, want %q", stage, tc.wantStage)
			}
		})
	}
}

// TestUnwindBoundKey_StageWalkAgreesWithTheCounter cross-checks the stage walk
// against the counter over the whole B×N grid, so the attribution the walk gives
// is never trusted on its own.
func TestUnwindBoundKey_StageWalkAgreesWithTheCounter(t *testing.T) {
	const mod = 20
	type cell struct{ n, b int }
	// The grid spans both sides of the population floor and of the cost gate.
	cells := []cell{
		{40, 4}, {40, 20}, {63, 20}, {64, 20},
		{400, 20}, {400, 400}, {400, 4000},
		{2000, 1}, {2000, 20}, {2000, 2000}, {2000, 200000},
	}
	for _, spelling := range []struct {
		name  string
		query string
	}{
		{"where", inljCountWhere},
		{"inline", inljCountInline},
	} {
		for _, c := range cells {
			t.Run(fmt.Sprintf("%s/N=%d/B=%d", spelling.name, c.n, c.b), func(t *testing.T) {
				eng := inljFixture(t, c.n, mod)
				seek, hash := inljObserve(t, eng, spelling.query, inljAnyParams(c.b, mod))
				stage := inljTriggerStage(inljLowerPlan(t, spelling.query), inljParams(c.b, mod), c.n)
				t.Logf("N=%d B=%d spelling=%s: seek=%v hash=%v stage=%q",
					c.n, c.b, spelling.name, seek, hash, stage)
				if wantSeek := stage == "admitted"; seek != wantSeek {
					t.Errorf("counter says seek=%v but the stage walk says %q (want seek=%v). "+
						"The walk models tryBuildIndexNestedLoopJoin; a disagreement means it has "+
						"drifted and its attribution cannot be trusted", seek, stage, wantSeek)
				}
			})
		}
	}
}

// TestUnwindBoundKey_InlinePropertyMapKeyShape isolates WHY the inline spelling
// behaves as it does, by recording the AST kind of each side of the predicate the
// two spellings produce. It is the detail #2813 asks to be recorded exactly.
func TestUnwindBoundKey_InlinePropertyMapKeyShape(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"where", inljQueryWhere},
		{"inline", inljQueryInline},
		{"node-return", inljQueryNodeReturn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := inljLowerPlan(t, tc.query)
			sel, apply := inljFindSelectionOverApply(plan)
			if sel == nil {
				t.Logf("query %s: NO Selection over Apply at all; full IR:\n%s", tc.query, ir.Explain(plan))
				return
			}
			var b strings.Builder
			fmt.Fprintf(&b, "predicate string: %q\n", sel.Predicate)
			fmt.Fprintf(&b, "predicate AST:    %s\n", inljDescribeExpr(sel.PredicateExpr))
			fmt.Fprintf(&b, "apply.Outer:      %s\n", ir.OperatorName(apply.Outer))
			fmt.Fprintf(&b, "apply.Inner:      %s\n", ir.OperatorName(apply.Inner))
			for i, c := range splitConjuncts(sel.PredicateExpr) {
				fmt.Fprintf(&b, "conjunct[%d]:      %s\n", i, inljDescribeExpr(c))
			}
			t.Logf("query %s\n%s", tc.query, b.String())
		})
	}
}

// inljDescribeExpr renders an AST expression as its Go type plus its operands'
// types, one level deep — enough to tell `Property(Variable) = Property(Variable)`
// from `Property(Variable) = Variable`, which is the distinction the #2182 fold
// and this trigger both turn on.
func inljDescribeExpr(e ast.Expression) string {
	switch n := e.(type) {
	case nil:
		return "<nil>"
	case *ast.BinaryOp:
		return fmt.Sprintf("BinaryOp(%s){L: %s, R: %s}", n.Operator,
			inljDescribeExpr(n.Left), inljDescribeExpr(n.Right))
	case *ast.Property:
		return fmt.Sprintf("Property(.%s of %s)", n.Key, inljDescribeExpr(n.Receiver))
	case *ast.Variable:
		return fmt.Sprintf("Variable(%s)", n.Name)
	case *ast.Parameter:
		return fmt.Sprintf("Parameter($%s)", n.Name)
	default:
		return fmt.Sprintf("%T", e)
	}
}

// TestUnwindBoundKey_CountProjectionKeepsTheTriggerShape validates the grid's own
// instrument. The grid projects count(*) to stay under the result-row cap; that is
// only sound if the aggregation leaves the Selection-over-Apply shape the trigger
// reads intact. Asserted rather than assumed.
func TestUnwindBoundKey_CountProjectionKeepsTheTriggerShape(t *testing.T) {
	const (
		n   = 400
		b   = 20
		mod = 20
	)
	for _, tc := range []struct{ name, rows, count string }{
		{"where", inljQueryWhere, inljCountWhere},
		{"inline", inljQueryInline, inljCountInline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rowsStage := inljTriggerStage(inljLowerPlan(t, tc.rows), inljParams(b, mod), n)
			countStage := inljTriggerStage(inljLowerPlan(t, tc.count), inljParams(b, mod), n)
			if rowsStage != countStage {
				t.Fatalf("the count projection changed the trigger stage: rows %q, count %q — the "+
					"grid's substitution of count(*) for the row projection would then be measuring "+
					"a different plan shape than the query #2813 names", rowsStage, countStage)
			}
			eng := inljFixture(t, n, mod)
			if seek, _ := inljObserve(t, eng, tc.count, inljAnyParams(b, mod)); !seek {
				t.Fatal("the count spelling did not build the seek, so the grid's positive cells " +
					"would be unobservable")
			}
		})
	}
}

// TestUnwindBoundKey_TheThreeDeclineCauses enumerates every condition that can
// send this shape to the hash join, and pins WHICH one each case exercises.
//
// The distinction matters because #2813's observation — NodeByLabelScan +
// HashJoin — is produced by the LAST case here and by no other. A numeric key at
// N ≥ 64 takes the seek, as the grid shows, so a report of "the seek is not
// planned" is a report about the KEY TYPE, not about the trigger or the gate.
func TestUnwindBoundKey_TheThreeDeclineCauses(t *testing.T) {
	const mod = 20
	cases := []struct {
		name string
		// n, b select the regime; ageIsString swaps the indexed property's kind.
		n, b        int
		ageIsString bool
		// wantSeek / wantHash pin the plan, so a case cannot pass by falling
		// through to a third one.
		wantSeek, wantHash bool
		because            string
	}{
		{
			name: "numeric key, population above the floor: SEEK",
			n:    400, b: 20,
			wantSeek: true, wantHash: false,
			because: "the control: this is the regime the operator exists for",
		},
		{
			name: "numeric key, population below the floor: neither",
			n:    40, b: 20,
			wantSeek: false, wantHash: false,
			because: "indexNestedLoopMinPopulation = 64, and hashJoinSizeFloor is the same 64, " +
				"so a sub-floor population is the plain nested loop — NOT the hash join",
		},
		{
			name: "STRING key, population above the floor: HASH JOIN",
			n:    400, b: 20,
			ageIsString: true,
			wantSeek:    false, wantHash: true,
			because: "exec.NumericPointLookup is LookupAppend(float64, …): the operator is " +
				"numeric-only BY CONSTRUCTION, so a string-valued property has an empty numeric " +
				"companion, numericIndexCoversScan declines, and the hash join serves the shape. " +
				"THIS is the NodeByLabelScan + HashJoin rendering #2813 reports",
		},
	}
	for _, spelling := range []struct{ name, query string }{
		{"where", inljCountWhere},
		{"inline", inljCountInline},
	} {
		for _, tc := range cases {
			t.Run(spelling.name+"/"+tc.name, func(t *testing.T) {
				eng := inljFixtureKeyed(t, tc.n, mod, tc.ageIsString)
				seek, hash := inljObserve(t, eng, spelling.query, inljAnyParams(tc.b, mod))
				t.Logf("N=%d B=%d stringKey=%v: seek=%v hash=%v — %s",
					tc.n, tc.b, tc.ageIsString, seek, hash, tc.because)
				if seek != tc.wantSeek || hash != tc.wantHash {
					t.Errorf("seek=%v hash=%v, want seek=%v hash=%v (%s)",
						seek, hash, tc.wantSeek, tc.wantHash, tc.because)
				}
			})
		}
	}
}

// inljFixtureKeyed is inljFixture with the indexed property's KIND as a
// parameter, so the numeric and string populations come from one builder. A
// second builder is exactly what index_nested_loop_plan_test.go records having
// drifted once already.
func inljFixtureKeyed(t *testing.T, n, mod int, stringKey bool) *Engine {
	t.Helper()
	if !stringKey {
		return inljFixture(t, n, mod)
	}
	g := inljFixtureGraph(t, n, mod)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.SetNodeProperty(key, "age", lpg.StringValue(fmt.Sprintf("s%d", i%mod))); err != nil {
			t.Fatalf("SetNodeProperty(age string): %v", err)
		}
	}
	eng := NewEngine(g)
	mustCreateIndex(t, eng)
	return eng
}

// TestUnwindBoundKey_CostGateCannotDeclineBelow32768 pins the analytic reach of
// the cost gate, which is the finding that rules it out as an explanation.
//
// The gate is B·log₂N < [indexSeekLevelsPerHashRow]·(N+B). Since
// indexSeekLevelsPerHashRow is 15, log₂N < 15 makes B·log₂N < 15·B ≤ 15·(N+B) for
// EVERY positive B: the gate cannot decline at all below N = 2¹⁵ = 32768. So no
// choice of batch size can be the reason the seek is skipped on a population
// smaller than that — which covers the whole regime #2813's report describes.
//
// This is a pure function of two integers, so exercising it directly IS the
// measurement; no fixture can make it more true.
func TestUnwindBoundKey_CostGateCannotDeclineBelow32768(t *testing.T) {
	// 2^indexSeekLevelsPerHashRow is the first population at which log₂N reaches
	// the constant. Derived from the constant rather than written as 32768, so the
	// case follows the constant if it is ever recalibrated.
	threshold := 1
	for i := 0; i < indexSeekLevelsPerHashRow; i++ {
		threshold *= 2
	}
	if threshold != 32768 {
		t.Fatalf("indexSeekLevelsPerHashRow = %d puts the threshold at N = %d, not 32768; the "+
			"finding recorded on #2813 was measured against 32768 and must be restated",
			indexSeekLevelsPerHashRow, threshold)
	}
	// Below the threshold, no B declines. The batch sizes span nine orders of
	// magnitude, including absurd ones, precisely to show the gate is insensitive
	// to B in this regime.
	for _, n := range []int{64, 400, 2000, 20000, threshold - 1} {
		for _, b := range []int{1, 20, 500, 5000, 500000, 40_000_000, 1_000_000_000} {
			if !indexNestedLoopWins(b, n) {
				t.Errorf("indexNestedLoopWins(B=%d, N=%d) = false, but log₂N < %d makes the "+
					"comparison hold for every positive B", b, n, indexSeekLevelsPerHashRow)
			}
		}
	}
	// At and above the threshold a decline becomes reachable, and the B it needs is
	// B ≥ 15N/(log₂N − 15) — enormous, and the reason the gate is a boundary guard
	// rather than an everyday decision.
	if indexNestedLoopWins(1_000_000_000, 80000) {
		t.Error("the gate admitted B=1e9 against N=80000; it must decline beyond the measured " +
			"range, which is the whole reason it is kept")
	}
	if !indexNestedLoopWins(500, 80000) {
		t.Error("the gate declined a small batch against a deep index; the seek's advantage is " +
			"largest exactly there")
	}
}

// TestUnwindBoundKey_ReportedRenderingComesOnlyFromTheStringKey captures the
// rendering #2813's report cites, and pins which regime produces it.
//
// The counter stays the authority on the chosen plan (#2233 AC4). Explain is
// asserted here ONLY as a cross-check that it agrees with the counter for this
// shape: #2222 found a class of divergence where a rendering agreed with the
// planner's intent while the built operator differed, so establishing that this
// shape is NOT such a case is what lets the report's rendering be taken at face
// value — and it is the string-key regime, and no other, that renders
// NodeByLabelScan + HashJoin.
func TestUnwindBoundKey_ReportedRenderingComesOnlyFromTheStringKey(t *testing.T) {
	const mod = 20
	cases := []struct {
		name      string
		n, b      int
		stringKey bool
		// wantOp is the substring the physical rendering must contain, and
		// wantAbsent the one it must not.
		wantOp, wantAbsent string
		wantSeek, wantHash bool
	}{
		{
			name: "numeric key above the floor renders the seek",
			n:    400, b: 20,
			wantOp: "IndexNestedLoopJoin", wantAbsent: "HashJoin",
			wantSeek: true, wantHash: false,
		},
		{
			name: "STRING key above the floor renders NodeByLabelScan + HashJoin",
			n:    400, b: 20, stringKey: true,
			wantOp: "HashJoin", wantAbsent: "IndexNestedLoopJoin",
			wantSeek: false, wantHash: true,
		},
		{
			name: "numeric key below the floor renders the plain Apply",
			n:    40, b: 20,
			wantOp: "Apply", wantAbsent: "HashJoin",
			wantSeek: false, wantHash: false,
		},
	}
	for _, spelling := range []struct{ name, query string }{
		{"where", inljCountWhere},
		{"inline", inljCountInline},
	} {
		for _, tc := range cases {
			t.Run(spelling.name+"/"+tc.name, func(t *testing.T) {
				eng := inljFixtureKeyed(t, tc.n, mod, tc.stringKey)
				// Explain builds the physical plan, so the counters move under it —
				// which is what makes this a genuine cross-check rather than a
				// comparison of two renderings.
				beforeSeek := indexNestedLoopBuildCount.Load()
				beforeHash := hashJoinBuildCount.Load()
				rendered, err := eng.Explain(spelling.query, inljParams(tc.b, mod))
				if err != nil {
					t.Fatalf("Explain: %v", err)
				}
				seek := indexNestedLoopBuildCount.Load() > beforeSeek
				hash := hashJoinBuildCount.Load() > beforeHash
				t.Logf("N=%d B=%d stringKey=%v seek=%v hash=%v\n%s",
					tc.n, tc.b, tc.stringKey, seek, hash, rendered)
				// The counter first: it decides.
				if seek != tc.wantSeek || hash != tc.wantHash {
					t.Fatalf("counter says seek=%v hash=%v, want seek=%v hash=%v",
						seek, hash, tc.wantSeek, tc.wantHash)
				}
				// Then the cross-check.
				if !strings.Contains(rendered, tc.wantOp) {
					t.Errorf("rendering lacks %q:\n%s", tc.wantOp, rendered)
				}
				if strings.Contains(rendered, tc.wantAbsent) {
					t.Errorf("rendering contains %q, which the counter says was not built — that "+
						"is the #2222 divergence class, and this shape must not be in it:\n%s",
						tc.wantAbsent, rendered)
				}
			})
		}
	}
}

// TestUnwindBoundKey_VerbatimQueryFromTheTask runs #2813's query exactly as the
// task states it — inline property map, RETURN of the node itself — so the
// verdict rests on the reported spelling and not on a paraphrase of it.
func TestUnwindBoundKey_VerbatimQueryFromTheTask(t *testing.T) {
	const (
		n   = 400
		b   = 20
		mod = 20
	)
	eng := inljFixture(t, n, mod)
	seek, hash := inljObserve(t, eng, inljQueryNodeReturn, inljAnyParams(b, mod))
	if !seek || hash {
		t.Fatalf("the verbatim query built seek=%v hash=%v at B=%d, N=%d; #2813's premise is "+
			"that it does NOT plan the seek, and this case is what refutes or confirms that",
			seek, hash, b, n)
	}
	// And with a string-valued key it must render the reported plan instead.
	strEng := inljFixtureKeyed(t, n, mod, true)
	seek, hash = inljObserve(t, strEng, inljQueryNodeReturn, inljAnyParams(b, mod))
	if seek || !hash {
		t.Fatalf("with a STRING key the verbatim query built seek=%v hash=%v; the reported "+
			"NodeByLabelScan + HashJoin rendering must come from exactly this regime", seek, hash)
	}
}

// TestUnwindBoundKey_BareVariableKeySpellings maps the remaining UNWIND-bound
// spellings, so #2813's verdict is not mistaken for a claim about all of them.
//
// A BARE VARIABLE on the key side — `{age: k}` rather than `{age: r.a}` — is what
// [propertyAndVariable] requires, so the #2182 fold (correlated_seek_plan.go) is
// a candidate to claim these before the index nested-loop trigger ever sees them.
// Whether it does turns on ONE thing, and this case measures it rather than
// reasoning about it: [unwoundKeySet] requires the UNWIND's list to be an
// *ast.ListLiteral.
//
//   - `UNWIND $keys AS k` — the list is an *ast.Parameter, so the fold DECLINES,
//     apply.Inner stays a bare scan, and the index nested-loop join fires exactly
//     as it does for the list-of-maps spelling.
//   - `UNWIND [1,2,3] AS k` — the list IS a literal, so the fold rewrites
//     apply.Inner into a Selection carrying an equality disjunction over the key
//     set. scanLeafNodeVar then sees no bare scan leaf and the index nested-loop
//     trigger declines, and the HASH JOIN serves the shape instead. Note what that
//     means: the fold's key-set hint does not reach the physical plan either — the
//     rendering is HashJoin over a plain full NodeByLabelScan — so this spelling
//     gets NEITHER the key-set seek nor the per-row seek. That is an optimisation
//     miss, not a correctness one: the retained outer Selection is what the hash
//     join turns into its key equality, so the answer is right regardless. It is
//     recorded here rather than fixed, being outside #2813's scope.
//
// This was written on the opposite hypothesis (that the fold claimed BOTH) and the
// first run refuted it, which is why the reason is recorded as the measured
// discriminator rather than as a description of the pass's intent.
func TestUnwindBoundKey_BareVariableKeySpellings(t *testing.T) {
	const (
		n   = 400
		mod = 20
		b   = 20
	)
	keys := make([]any, b)
	for i := range keys {
		keys[i] = int64(i % mod)
	}

	cases := []struct {
		name   string
		query  string
		params map[string]any
		// wantBareInner is whether apply.Inner survives the fold as a bare scan
		// leaf, which is precisely what decides whether the trigger is reachable.
		wantBareInner bool
		wantSeek      bool
		wantHash      bool
	}{
		{
			name:   "parameter list: the fold declines, the seek fires",
			query:  `UNWIND $keys AS k MATCH (b:P {age: k}) RETURN count(*) AS n`,
			params: map[string]any{"keys": keys},
			// unwoundKeySet requires an *ast.ListLiteral; a parameter is not one.
			wantBareInner: true, wantSeek: true, wantHash: false,
		},
		{
			name:   "literal list: the fold claims it, so the trigger declines",
			query:  `UNWIND [1, 2, 3] AS k MATCH (b:P {age: k}) RETURN count(*) AS n`,
			params: nil,
			// The fold wraps apply.Inner in a Selection carrying the disjunction, and
			// the hash join — which does not require a bare scan leaf — then claims
			// the shape. MEASURED: this case was written expecting no substitution at
			// all, and the first run showed the hash join firing.
			wantBareInner: false, wantSeek: false, wantHash: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := inljLowerPlan(t, tc.query)
			sel, apply := inljFindSelectionOverApply(plan)
			if sel == nil {
				t.Fatalf("no Selection over Apply for %q; IR:\n%s", tc.query, ir.Explain(plan))
			}
			_, _, bareInner := scanLeafNodeVar(apply.Inner)
			t.Logf("query: %s\nIR after the #2182 fold:\n%s\napply.Inner=%s bareInner=%v",
				tc.query, ir.Explain(plan), ir.OperatorName(apply.Inner), bareInner)
			if bareInner != tc.wantBareInner {
				t.Errorf("apply.Inner bare scan leaf = %v, want %v — this is the single condition "+
					"that decides whether the index nested-loop trigger is reachable for this "+
					"spelling, so a change here changes #2813's verdict", bareInner, tc.wantBareInner)
			}

			eng := inljFixture(t, n, mod)
			seek, hash := inljObserve(t, eng, tc.query, tc.params)
			t.Logf("N=%d B=%d: seek=%v hash=%v", n, len(keys), seek, hash)
			if seek != tc.wantSeek || hash != tc.wantHash {
				t.Errorf("seek=%v hash=%v, want seek=%v hash=%v", seek, hash, tc.wantSeek, tc.wantHash)
			}
		})
	}
}
