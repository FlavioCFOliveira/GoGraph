package cypher

// join_reorder_stats_gate_test.go — the PER-SITE gates for the property-statistics
// reorder (rmp #2766).
//
// join_reorder_stats_test.go asserts the behaviour a caller can see. This file
// asserts each individual decision site, because a behavioural test can pass while
// a site inside it is never reached or never load-bearing: the first mutation
// sweep of this change killed 19 of 48 per-site mutations and left 29 standing,
// and every gate below exists to close one of those, or to record — with the
// argument — why the mutation was semantically equivalent and no gate can exist.
//
// Seven of the 49 mutations still survive, and each one is EQUIVALENT or
// STRUCTURALLY UNREACHABLE rather than uncovered. The argument for each is
// recorded here so a later reader does not mistake it for a gap:
//
//   - The `sel.PredicateExpr == nil` guard in [reorderFilteredScan] can be deleted
//     with no observable effect: both extractors begin with a type assertion on the
//     predicate, and a type assertion on a nil interface yields ok == false.
//     TestReorderFilteredScan_StringOnlySelectionIsNotAComponent holds the OUTCOME
//     (a string-only Selection is never a component) whichever guard produces it.
//   - reorderComponentCardinality's Apply DRAIN product may be written as the ROWS
//     product with no observable difference, because [isReorderBareComponent]
//     admits only arms whose drain equals their rows, and the products preserve
//     that. TestReorderComponentCardinality_ApplyDeclinesACertifiedArm is what
//     keeps that premise true.
//   - Both null-operand guards in [reorderFilteredRows] can be deleted without
//     changing a plan: a null literal misses the MCV list (null is never a stored
//     property value) and yields the 1/NDV heuristic, and it has no histogram
//     domain, so both providers already return a verdict the veto rejects. The
//     guards make the reason explicit instead of accidental.
//     TestJoinReorderStats_NullOperandIsInert holds the outcome.
//   - The freshness screen on the RANGE path can be bypassed without changing a
//     plan, and the reason is a coupling rather than a coverage gap: the certified
//     error [statsRangeEstimate] returns is 1/B + Delta/N, so by the time the write
//     fraction Delta/N reaches the screen's b - 1/B threshold the interval has
//     already widened by ~0.096*N rows, which is far more than any selective range
//     estimate — rule 1 then declines on the interval alone. It remains in place
//     because it is the only thing applying the SMALLER denominator to a range
//     estimate, which is the one reading that sees a snapshot invalidated by growth
//     (TestJoinReorderStats_GrowthDemotesWhereTheProviderDoesNot), and aligning the
//     range provider on that denominator was out of scope at rmp #2772.
//     Until rmp #2772 the screen was ALSO load-bearing for the equality path, whose
//     MCV count carries no error term at all. It is not any more: the provider now
//     applies the identical rule itself, where the RENDERER can see it too, so the
//     equality call site was removed as a proven no-op
//     (TestReorderStatsFreshness_EqualityScreenIsRedundant).
//     TestJoinReorderStats_StaleStatistic_NoSwap and its two isolating companions
//     still hold the equality OUTCOME, wherever the screen lives.
//   - reorderFilteredRows' final "unrecognised shape" return is unreachable from
//     its only caller: [reorderFilteredScan] has already matched the same two
//     extractors, and passing real parameters instead of nil can only change a
//     resolved VALUE, never whether a shape resolves.
//   - reorderStatsFreshness' missing-bundle return is unreachable single-threaded:
//     the screen runs only on a trustworthy estimate, and an estimate can only be
//     trustworthy if the provider already found the bundle. It survives a
//     concurrent Publish between the two lookups, which no deterministic test can
//     schedule.

import (
	"context"
	"fmt"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

func eqPredOn(nodeVar, key string, v int64) ast.Expression {
	return &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: nodeVar}, Key: key},
		Right:    &ast.IntLiteral{Value: v},
	}
}

func rangePredOn(nodeVar, key string, v int64) ast.Expression {
	return &ast.BinaryOp{
		Operator: ">",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: nodeVar}, Key: key},
		Right:    &ast.IntLiteral{Value: v},
	}
}

// liveResolver returns the engine's own label resolver, the concrete type the
// planner gate is handed on the read path. It is the only resolver that satisfies
// statsSource, which some gates below depend on and one deliberately does not.
func liveResolver(e *Engine) *lpgLabelResolver {
	return &lpgLabelResolver{g: e.g.ReadAt(nil), eng: e}
}

// bareLabelResolver satisfies labelResolverIface and NOTHING else — in particular
// not statsSource. It stands for any resolver that cannot serve statistics.
type bareLabelResolver struct{ counts map[string]uint64 }

func (r bareLabelResolver) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	bm := roaring64.New()
	for i := uint64(0); i < r.counts[name]; i++ {
		bm.Add(i)
	}
	return bm
}

func (r bareLabelResolver) ResolveLabelsBitmap(names []string) *roaring64.Bitmap {
	if len(names) == 0 {
		return roaring64.New()
	}
	return r.ResolveLabelBitmap(names[0])
}

// TestReorderFilteredScan_StringOnlySelectionIsNotAComponent gates the
// PredicateExpr guard: an ir.Selection built by [ir.NewSelection] carries only the
// opaque predicate STRING, which no estimator can read.
func TestReorderFilteredScan_StringOnlySelectionIsNotAComponent(t *testing.T) {
	sel := ir.NewSelection("(a.x = 1)", &ir.NodeByLabelScan{NodeVar: "a", Label: "A"})
	if _, ok := reorderFilteredScan(sel); ok {
		t.Fatal("a Selection with no parsed predicate must not be a filtered component")
	}
	if isReorderComponent(sel) {
		t.Fatal("isReorderComponent admitted a string-only Selection")
	}
}

// TestReorderComponentCardinality_DeclinesAnUnsupportedOperator gates the default
// arm: anything that is not one of the four recognised shapes must decline, so a
// zero-value componentCost — whose estSource zero value is estExact — can never
// reach the cost rule.
func TestReorderComponentCardinality_DeclinesAnUnsupportedOperator(t *testing.T) {
	for name, plan := range map[string]ir.LogicalPlan{
		"argument":  ir.NewArgument([]string{"a"}),
		"limit":     ir.NewLimit(1, &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}),
		"expand":    &ir.Expand{FromVar: "a", ToVar: "b", Child: &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}},
		"nil child": nil,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := reorderComponentCardinality(plan, bareLabelResolver{}, nil, 10)
			if ok {
				t.Fatalf("admitted %s as a component: %+v", name, got)
			}
		})
	}
}

// TestReorderComponentCardinality_ApplyTakesTheWeakerProvenance gates
// [weakerSource]: a composed component is only as trustworthy as its weakest arm.
// A nil resolver makes the label arm estFallback while the all-nodes arm stays
// estExact, so combining them must yield estFallback — the combination that makes
// [planStaysDefault] keep the written order.
func TestReorderComponentCardinality_ApplyTakesTheWeakerProvenance(t *testing.T) {
	ap := &ir.Apply{
		Outer: &ir.AllNodesScan{NodeVar: "a"},
		Inner: &ir.NodeByLabelScan{NodeVar: "b", Label: "B"},
	}
	got, ok := reorderComponentCardinality(ap, nil, nil, 10)
	if !ok {
		t.Fatal("an Apply of two bare scans must be a component")
	}
	if got.drain.source != estFallback || got.rows.source != estFallback {
		t.Fatalf("provenance = (drain %v, rows %v), want both fallback: a nil resolver's "+
			"unknowable label count must not be laundered into an exact composed estimate",
			got.drain.source, got.rows.source)
	}
	if planStaysDefault(got.drain, got.rows) == false {
		t.Fatal("the veto did not reject the composed estimate")
	}
}

// TestReorderComponentCardinality_ApplyDeclinesACertifiedArm gates the rowsErr
// guard on the composed case, and with it the premise that lets the drain product
// be written either way: a component carrying a certified interval must never be
// composed, because the product model is only exact for arms whose drain equals
// their rows.
func TestReorderComponentCardinality_ApplyDeclinesACertifiedArm(t *testing.T) {
	g := buildRangeSkewGraph(t, 2000, 50)
	e := NewEngine(g)
	refreshStats(t, e)
	src := liveResolver(e)

	rangeArm := ir.NewSelectionExpr("(a.x > 1980)", rangePredOn("a", "x", 1980),
		&ir.NodeByLabelScan{NodeVar: "a", Label: "A"})
	solo, ok := reorderComponentCardinality(rangeArm, src, nil, 2050)
	if !ok {
		t.Fatal("a range-filtered label scan must be a component on its own")
	}
	if solo.rowsErr <= 0 {
		t.Fatalf("expected a certified error on a histogram range estimate, got %g "+
			"(the interval gate has nothing to widen)", solo.rowsErr)
	}
	if solo.rows.source != estStats {
		t.Fatalf("rows provenance = %v, want stats", solo.rows.source)
	}

	ap := &ir.Apply{Outer: &ir.NodeByLabelScan{NodeVar: "b", Label: "B"}, Inner: rangeArm}
	if _, ok := reorderComponentCardinality(ap, src, nil, 2050); ok {
		t.Fatal("composed an interval-carrying arm into an Apply component")
	}
}

// TestReorderFilteredRows_CertifiedErrorIsScaledByTheLabelCount gates the rowsErr
// computation: an equality carries no error and a range carries delta * N.
func TestReorderFilteredRows_CertifiedErrorIsScaledByTheLabelCount(t *testing.T) {
	const nA = 2000
	g := buildRangeSkewGraph(t, nA, 50)
	e := NewEngine(g)
	refreshStats(t, e)
	src := liveResolver(e)
	scan := &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}
	drain := labelCardinalityEstimate(src, "A")
	if drain.rows != nA {
		t.Fatalf("label count = %g, want %d", drain.rows, nA)
	}

	eq := ir.NewSelectionExpr("(a.x = 7)", eqPredOn("a", "x", 7), scan)
	if _, err := reorderFilteredRows(eq, scan, src, nil, drain); err != 0 {
		t.Fatalf("equality rowsErr = %g, want 0 (an MCV count is exact)", err)
	}
	rg := ir.NewSelectionExpr("(a.x > 1980)", rangePredOn("a", "x", 1980), scan)
	_, rgErr := reorderFilteredRows(rg, scan, src, nil, drain)
	// delta is at least 1/B, so the row error is at least N/B.
	if want := float64(nA) / float64(statsHistogramBuckets); rgErr < want {
		t.Fatalf("range rowsErr = %g, want at least N/B = %g", rgErr, want)
	}
}

// TestReorderFilteredRows_NonStatsResolverIsInert gates the statsSource assertion:
// a resolver that cannot serve statistics must yield estFallback, not a fabricated
// count. The production resolver always can, so this site is unreachable through
// the engine and only a direct call can hold it.
func TestReorderFilteredRows_NonStatsResolverIsInert(t *testing.T) {
	scan := &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}
	sel := ir.NewSelectionExpr("(a.x = 1)", eqPredOn("a", "x", 1), scan)
	src := bareLabelResolver{counts: map[string]uint64{"A": 500}}
	got, ok := reorderComponentCardinality(sel, src, nil, 500)
	if !ok {
		t.Fatal("the shape is a component regardless of the resolver")
	}
	if got.rows.source != estFallback {
		t.Fatalf("rows provenance = %v, want fallback", got.rows.source)
	}
	if !planStaysDefault(got.drain, got.rows) {
		t.Fatal("the veto did not reject an estimate from a stats-free resolver")
	}
}

// TestComponentCostRowInterval gates both interval ends and both clamps.
func TestComponentCostRowInterval(t *testing.T) {
	mk := func(drain, rows, err float64) componentCost {
		return componentCost{
			drain:   estimate{rows: drain, source: estExact},
			rows:    estimate{rows: rows, source: estStats},
			rowsErr: err,
		}
	}
	for _, c := range []struct {
		name           string
		cost           componentCost
		wantLo, wantHi float64
	}{
		{"no error is a point", mk(100, 50, 0), 50, 50},
		{"error widens both ways", mk(100, 50, 10), 40, 60},
		{"low end clamps at zero", mk(100, 5, 20), 0, 25},
		{"high end clamps at the drain", mk(100, 95, 20), 75, 100},
		{"a stale count above the drain is clamped", mk(10, 40, 0), 10, 10},
		{"an empty component is zero", mk(0, 0, 0), 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cost.rowsLo(); got != c.wantLo {
				t.Fatalf("rowsLo = %g, want %g", got, c.wantLo)
			}
			if got := c.cost.rowsHi(); got != c.wantHi {
				t.Fatalf("rowsHi = %g, want %g", got, c.wantHi)
			}
		})
	}
}

// TestReorderSwapWins_TieIsNotAStrictImprovement gates the strict-improvement
// requirement of rule 2 (and, with it, the drain terms in the gain expression):
// the two orders cost EXACTLY the same, so the written order must stand. The
// margin comparison alone admits it, because it permits equality.
func TestReorderSwapWins_TieIsNotAStrictImprovement(t *testing.T) {
	exact := func(n float64) estimate { return estimate{rows: n, source: estExact} }
	// Both orders cost exactly 20: written is 8 + 3*4, swapped is 4 + 2*8.
	outer := componentCost{drain: exact(8), rows: exact(3)}
	inner := componentCost{drain: exact(4), rows: exact(2)}
	if reorderSwapWins(outer, inner) {
		t.Fatal("swapped an exactly-equal pair; a swap must be a STRICT improvement")
	}
	// One row cheaper on the candidate side and it is a strict win again.
	// One row cheaper on the candidate side costs 4 + 1*8 = 12 against the same 20.
	inner.rows = exact(1)
	if !reorderSwapWins(outer, inner) {
		t.Fatal("declined a strict improvement")
	}
}

// TestReorderSwapWins_FractionalTieIsDeclined gates rule 1's strictness. A
// histogram estimate below one row makes rule 2's gain positive at a tie, so
// without rule 1's strict comparison an equal-cardinality pair would swap.
func TestReorderSwapWins_FractionalTieIsDeclined(t *testing.T) {
	half := estimate{rows: 0.5, source: estExact}
	outer := componentCost{drain: estimate{rows: 100, source: estExact}, rows: half}
	inner := componentCost{drain: estimate{rows: 20, source: estExact}, rows: half}
	if reorderSwapWins(outer, inner) {
		t.Fatal("swapped two components with identical emitted-row estimates")
	}
}

// TestReorderSwapWins_StatsMarginDemandsAThreefoldWin gates the stats-margin
// selection, [reorderPathHasStats], and the final margin comparison. The SAME
// numbers swap under an exact provenance and must not swap under a histogram one,
// because a 1.95x modelled win does not clear the 3x margin the activation design
// requires of an estimated selectivity.
func TestReorderSwapWins_StatsMarginDemandsAThreefoldWin(t *testing.T) {
	drain := estimate{rows: 2000, source: estExact}
	one := estimate{rows: 1, source: estExact}
	inner := componentCost{drain: one, rows: one}

	exactOuter := componentCost{drain: drain, rows: estimate{rows: 1900, source: estExact}}
	if !reorderSwapWins(exactOuter, inner) {
		t.Fatal("an exact 1.95x win must swap under the 1.0 margin")
	}
	statsOuter := componentCost{drain: drain, rows: estimate{rows: 1900, source: estStats}}
	if reorderSwapWins(statsOuter, inner) {
		t.Fatalf("a 1.95x win on a histogram estimate must not clear the %gx stats margin",
			joinReorderStatsMargin)
	}
}

// TestComputeReorderSwaps_DeclinesANonComponentArm gates the ok1/ok2 guard. The
// candidate collector never produces such a candidate, so only a direct call can
// hold the site — and the site matters because estExact is estSource's ZERO value,
// so a zero componentCost would sail through the veto as an exact zero.
func TestComputeReorderSwaps_DeclinesANonComponentArm(t *testing.T) {
	src := bareLabelResolver{counts: map[string]uint64{"B": 1}}
	// The non-component arm is the INNER one, and that side is the one that
	// matters: a zero componentCost reads as "0 rows, exactly", which is smaller
	// than any real outer and therefore looks like the ideal driver. With the arm
	// as the OUTER instead, the zero makes the candidate look WORSE and the swap is
	// declined by the cost rule anyway — so an outer-side test cannot hold the site.
	inner := &ir.Apply{
		Outer: &ir.NodeByLabelScan{NodeVar: "b", Label: "B"},
		Inner: ir.NewArgument([]string{"a"}), // not a component
	}
	if swaps := computeReorderSwaps([]*ir.Apply{inner}, src, nil, 100); len(swaps) != 0 {
		t.Fatalf("promoted a non-component arm whose zero-value estimate reads as an "+
			"exact zero: %v", swaps)
	}
	outer := &ir.Apply{
		Outer: ir.NewArgument([]string{"a"}),
		Inner: &ir.NodeByLabelScan{NodeVar: "b", Label: "B"},
	}
	if swaps := computeReorderSwaps([]*ir.Apply{outer}, src, nil, 100); len(swaps) != 0 {
		t.Fatalf("swapped a candidate with a non-component outer arm: %v", swaps)
	}
}

// TestJoinReorderStats_ThreeComponentsUseTheProductCardinality gates the composed
// rows product: the top-level pair compares a 6x6 = 36-row product against a
// 10-row label, so a composition that reported only its outer arm's 6 rows would
// keep the written order instead of swapping.
func TestJoinReorderStats_ThreeComponentsUseTheProductCardinality(t *testing.T) {
	g := buildReorderGraph(t, map[string]int{"P": 6, "Q": 6, "S": 10})
	assertReorderIdentical(t, g,
		"MATCH (a:P), (b:Q), (c:S) RETURN a.k AS ak, b.k AS bk, c.k AS ck", true, false)
}

// TestJoinReorderStats_NullOperandIsInert gates both null guards. A parameter bound
// to null matches nothing under three-valued logic, which is not a row count any
// distribution statistic describes, so it must not drive a swap.
func TestJoinReorderStats_NullOperandIsInert(t *testing.T) {
	g := buildRangeSkewGraph(t, 2000, 50)
	on, off := statsReorderPair(t, g)
	params := map[string]expr.Value{"p": expr.Null}
	for _, q := range []string{
		"MATCH (b:B), (a:A {x: $p}) RETURN a.x AS ax, b.y AS bv",
		"MATCH (a:A) WHERE a.x > $p MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv",
	} {
		before := joinReorderBuildCount.Load()
		res, err := on.Run(context.Background(), q, params)
		if err != nil {
			t.Fatalf("Run(%q): %v", q, err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Close(); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%q returned %d rows; a null operand matches nothing", q, n)
		}
		if joinReorderBuildCount.Load() != before {
			t.Fatalf("a null operand drove a swap for %q", q)
		}
		if got, want := mustExplainTable(t, on, q, params), mustExplainTable(t, off, q, params); got != want {
			t.Fatalf("plan deviated from the default for %q:\n%s", q, got)
		}
	}
}

// TestJoinReorderStats_ReadPathSeesItsParameters gates the params argument on the
// READ path specifically. The EXPLAIN path is gated by
// TestJoinReorderStats_ParameterAndLiteralAgree; without this one, passing nil
// parameters to the build-time gate went unnoticed, because the plan a caller can
// see came from the EXPLAIN path.
func TestJoinReorderStats_ReadPathSeesItsParameters(t *testing.T) {
	g := buildSkewGraph(t, 20000, 100, 3)
	on, _ := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: $p}) RETURN a.x AS ax, b.y AS bv"
	params := map[string]expr.Value{"p": expr.IntegerValue(1)}

	before := joinReorderBuildCount.Load()
	res, err := on.Run(context.Background(), q, params)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	if joinReorderBuildCount.Load() == before {
		t.Fatal("the read-path gate did not resolve the bound parameter, so no swap fired")
	}
	if n != 3*100 {
		t.Fatalf("row count = %d, want %d", n, 3*100)
	}
}

// dirtyByReplacement issues n property REPLACEMENTS on :A nodes, which bump both
// the write counter and the delete counter (a replaced value is a value the HLL
// cannot remove). Every write keeps the node's value unchanged, so the query's
// true answer does not move.
func dirtyByReplacement(t *testing.T, e *Engine, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		mustWrite(t, e, fmt.Sprintf("MATCH (a:A {x: %d}) SET a.x = %d", i, i))
	}
}

// TestJoinReorderStats_DeleteToleranceAloneDemotes isolates the delete half of the
// freshness screen: 10 replacements over 500 rows leave the write fraction at 0.02,
// well under the b - 1/B threshold, while the delete count of 10 is already past
// the 1% rebuild tolerance. Only the delete condition can demote here.
func TestJoinReorderStats_DeleteToleranceAloneDemotes(t *testing.T) {
	g := buildSkewGraph(t, 500, 20, 3)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the swap to fire on a fresh statistic")
	}
	dirtyByReplacement(t, on, 100, 10)

	before = joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("10 replaced values over 500 rows exceed the 1% delete rebuild tolerance, " +
			"but the statistic still drove a swap")
	}
	if got, want := explainPlanShape(t, on, q, nil), explainPlanShape(t, off, q, nil); got != want {
		t.Fatalf("plan deviated from the default:\n%s", got)
	}
}

// TestJoinReorderStats_WriteFractionAloneDemotes isolates the write half: every
// dirtied node gains an "x" it did NOT have before, so the delete counter stays at
// zero and only the b - 1/B write fraction can demote. 60 of 500 is 0.12, past the
// 0.0961 threshold.
func TestJoinReorderStats_WriteFractionAloneDemotes(t *testing.T) {
	g := buildSkewGraph(t, 500, 20, 3)
	// 60 further :A nodes that carry NO x at refresh time.
	for i := 0; i < 60; i++ {
		k := fmt.Sprintf("nox%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "A"); err != nil {
			t.Fatal(err)
		}
	}
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the swap to fire on a fresh statistic")
	}
	for i := 0; i < 60; i++ {
		mustWrite(t, on, fmt.Sprintf("MATCH (a:A) WHERE a.x IS NULL WITH a LIMIT 1 SET a.x = %d", 100000+i))
	}

	before = joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("60 first-time writes over a 500-row build-time population pass the " +
			"b - 1/B staleness threshold, but the statistic still drove a swap")
	}
	if got, want := explainPlanShape(t, on, q, nil), explainPlanShape(t, off, q, nil); got != want {
		t.Fatalf("plan deviated from the default:\n%s", got)
	}
}

// TestJoinReorderStats_EmptiedLabelDemotes gates the zero-denominator guard.
//
// The route matters and the obvious one does NOT work: deleting the nodes moves
// both statistics counters (measured: DETACH DELETE of 500 :A nodes reported
// delta=500 deletes=500, because a node's removal removes its properties through
// the same write hook), so the delete-rebuild tolerance demotes first and the
// zero-denominator branch is never reached that way. Removing the LABEL instead
// touches no property, so both counters stay at zero, the statistic is pristine by
// its own measure, and the live label count is nevertheless zero. Without the
// guard the staleness fraction is 0/0 — NaN — and NaN fails every comparison, so a
// stale MCV count would be trusted as exact for a label with no rows in it.
func TestJoinReorderStats_EmptiedLabelDemotes(t *testing.T) {
	g := buildSkewGraph(t, 500, 20, 3)
	on, _ := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the swap to fire on a fresh statistic")
	}
	mustWrite(t, on, "MATCH (a:A) REMOVE a:A")

	src := liveResolver(on)
	if n := labelCardinalityEstimate(src, "A"); n.rows != 0 {
		t.Fatalf("live :A count = %g, want 0", n.rows)
	}
	st, ok := lookupStats(src, "A", "x")
	if !ok {
		t.Fatal("the statistics bundle vanished; the test premise is gone")
	}
	if st.Delta() != 0 || st.Deletes() != 0 {
		t.Fatalf("removing a label moved the statistics counters (delta=%d deletes=%d); "+
			"this test no longer isolates the zero-denominator guard",
			st.Delta(), st.Deletes())
	}
	if st.LabelCount() <= 0 {
		t.Fatalf("build-time label count = %d; the delete tolerance would demote first "+
			"and the guard would not be reached", st.LabelCount())
	}

	before = joinReorderBuildCount.Load()
	got := drainRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("an emptied label's stale MCV count drove a swap")
	}
	if len(got) != 0 {
		t.Fatalf("row count = %d, want 0", len(got))
	}
}

// TestJoinReorderStats_GrowthDemotesWhereTheProviderDoesNot gates the planner's
// choice of denominator — the SMALLER of the statistic's build-time label count and
// the live count.
//
// The window is narrow and deliberate, and the label has to grow AFTER the refresh
// for it to exist at all: a bundle built over 5500 rows has the same build-time and
// live counts, so there is nothing to choose between. Here the bundle is built over
// 5000, then 500 nodes join :A and receive an "x" they did not have. The write
// fraction against the BUILD-TIME count is then 500/5000 = 0.100, past the 0.0961
// threshold; against the now-5500 LIVE count it is 0.0909, under it. So
// [statsRangeEstimateInner]'s own screen, which divides by the live count, still
// says fresh, and only the planner's screen demotes. Taking the larger denominator
// would trust a statistic the smaller one rejects.
func TestJoinReorderStats_GrowthDemotesWhereTheProviderDoesNot(t *testing.T) {
	g := buildRangeSkewGraph(t, 5000, 2000)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the histogram-driven swap to fire on a fresh statistic")
	}

	src := liveResolver(on)
	st, ok := lookupStats(src, "A", "x")
	if !ok {
		t.Fatal("the statistics bundle vanished; the test premise is gone")
	}
	n0 := st.LabelCount()
	if n0 != 5000 {
		t.Fatalf("build-time :A count = %d, want 5000", n0)
	}

	// Grow :A by 500 nodes, each gaining an x it never had, so the delete counter
	// stays at zero and only the write fraction can demote.
	for i := 0; i < 500; i++ {
		mustWrite(t, on, fmt.Sprintf("CREATE (a:A {x: %d})", -1-i))
	}

	live := labelCardinalityEstimate(src, "A")
	if live.rows != 5500 {
		t.Fatalf("live :A count = %g, want 5500", live.rows)
	}
	thr := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	byBuild := float64(st.Delta()) / float64(n0)
	byLive := float64(st.Delta()) / live.rows
	if st.Deletes() != 0 {
		t.Fatalf("deletes = %d, want 0 so only the write fraction can demote", st.Deletes())
	}
	if !(byBuild >= thr && byLive < thr) {
		t.Fatalf("the isolating window is gone: delta=%d n0=%d live=%g -> byBuild=%.4f "+
			"byLive=%.4f threshold=%.4f", st.Delta(), n0, live.rows, byBuild, byLive, thr)
	}

	before = joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatalf("the planner trusted a statistic its own denominator rejects "+
			"(byBuild=%.4f >= %.4f)", byBuild, thr)
	}
	if got, want := mustExplainTable(t, on, q, nil), mustExplainTable(t, off, q, nil); got != want {
		t.Fatalf("plan deviated from the default:\n%s", got)
	}
}

// mustWrite runs a write statement through the engine's write path and drains it.
func mustWrite(t *testing.T, e *Engine, q string) {
	t.Helper()
	res, err := e.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
}
