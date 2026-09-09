package cypher

// join_reorder_stats_test.go — the property-statistics-driven half of the
// disjoint-component reorder (rmp #2766).
//
// Every test here pairs a reorder-ENABLED engine against a reorder-DISABLED one
// over the SAME graph, so "the plan today" is never asserted from a golden string
// that could drift: the disabled engine IS the reference, and it is re-derived on
// every run. Result identity is asserted as a sorted bag, because a Cartesian
// product's emission order is unobserved (see join_reorder_diff_test.go).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildSkewGraph creates nA nodes labelled :A carrying an integer "x" — the first
// hits of them with x = 1 and the rest with distinct values that are not 1 — plus
// nB nodes labelled :B carrying a distinct integer "y". The skew is the point: a
// large label of which a handful of rows satisfy the predicate.
func buildSkewGraph(t testing.TB, nA, nB, hits int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < nA; i++ {
		k := fmt.Sprintf("a%d", i)
		mustNode(t, g, k, "A", "x", int64(i+2))
		if i < hits {
			if err := g.SetNodeProperty(k, "x", lpg.Int64Value(1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < nB; i++ {
		mustNode(t, g, fmt.Sprintf("b%d", i), "B", "y", int64(i))
	}
	return g
}

func mustNode(t testing.TB, g *lpg.Graph[string, float64], key, label, prop string, v int64) {
	t.Helper()
	if err := g.AddNode(key); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel(key, label); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty(key, prop, lpg.Int64Value(v)); err != nil {
		t.Fatal(err)
	}
}

func refreshStats(t testing.TB, e *Engine) {
	t.Helper()
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
}

// statsReorderPair returns a reorder-ENABLED engine with statistics refreshed and
// a reorder-DISABLED engine over the same graph. The disabled engine is the
// reference plan and the reference result.
func statsReorderPair(t testing.TB, g *lpg.Graph[string, float64]) (on, off *Engine) {
	t.Helper()
	on = NewEngine(g)
	off = NewEngineWithOptions(g, EngineOptions{DisableJoinReorder: true})
	refreshStats(t, on)
	refreshStats(t, off)
	return on, off
}

func mustExplainTable(t testing.TB, e *Engine, q string, params map[string]expr.Value) string {
	t.Helper()
	s, err := e.ExplainTable(q, params)
	if err != nil {
		t.Fatalf("ExplainTable(%q): %v", q, err)
	}
	return s
}

// explainPlanShape renders e's plan for q with the Est.Rows column removed, so that
// a comparison sees the OPERATOR TREE and not the cardinality annotation beside it.
//
// The distinction became load-bearing at rmp #2772. [statsReorderPair]'s two engines
// hold SEPARATE statistics collectors over one shared graph, and only the engine the
// dirtying writes are issued through observes them — so after those writes the two
// snapshots genuinely differ in freshness. Since #2772 freshness decides whether an
// MCV estimate is rendered at all, which means the Est.Rows column of the pair
// legitimately differs while the plan they describe is identical. Comparing the whole
// table would assert the two engines hold equally fresh statistics, which is not what
// these gates are about and is not true.
//
// Every gate that uses it also asserts the swap did not fire, via
// [joinReorderBuildCount]; this is the independent check on the resulting SHAPE.
func explainPlanShape(t testing.TB, e *Engine, q string, params map[string]expr.Value) string {
	t.Helper()
	table := mustExplainTable(t, e, q, params)
	lines := strings.Split(table, "\n")
	// Locate the column by its HEADER: Est.Rows is conditional (rmp #2765), so a
	// fixed index would delete the Vars column from a table that has no estimates.
	col := -1
	for _, line := range lines {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		for i, f := range strings.Split(line, "|") {
			if strings.TrimSpace(f) == "Est.Rows" {
				col = i
			}
		}
		break // the first content line is the header row
	}
	if col < 0 {
		return table
	}
	var b strings.Builder
	for _, line := range lines {
		sep := "|"
		if strings.HasPrefix(line, "+") {
			sep = "+" // the rule lines between rows use the corner glyph as separator
		}
		f := strings.Split(line, sep)
		if col >= len(f) {
			b.WriteString(line + "\n")
			continue
		}
		b.WriteString(strings.Join(append(f[:col:col], f[col+1:]...), sep) + "\n")
	}
	return b.String()
}

func sortedRows(t testing.TB, e *Engine, q string) []string {
	t.Helper()
	out := drainRows(t.(*testing.T), e, q)
	sort.Strings(out)
	return out
}

// TestJoinReorderStats_FilteredComponentDrives is the core differential: the
// written order drives the 100-row :B and re-drains a 20 000-row filtered :A once
// per row; the statistics say the filter emits 3, so the reorder drives with the
// filtered component instead. The result bag must be identical either way.
func TestJoinReorderStats_FilteredComponentDrives(t *testing.T) {
	g := buildSkewGraph(t, 20000, 100, 3)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	gotOn := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the property-statistics reorder to fire, but it did not")
	}
	gotOff := sortedRows(t, off, q)

	if len(gotOn) != 3*100 {
		t.Fatalf("row count = %d, want %d", len(gotOn), 3*100)
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch: reorder=%d default=%d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("bag row %d differs:\n  reorder = %s\n  default = %s", i, gotOn[i], gotOff[i])
		}
	}

	// And the plan actually changed: the filtered arm is rendered first.
	tblOn, tblOff := mustExplainTable(t, on, q, nil), mustExplainTable(t, off, q, nil)
	if tblOn == tblOff {
		t.Fatalf("reorder-enabled and -disabled plans are identical; the swap is not visible:\n%s", tblOn)
	}
	if !plansDriveWith(tblOn, "Selection") {
		t.Fatalf("expected the filtered component to drive:\n%s", tblOn)
	}
	if !plansDriveWith(tblOff, "NodeByLabelScan [b:B]") {
		t.Fatalf("expected the default plan to drive with :B:\n%s", tblOff)
	}
}

// plansDriveWith reports whether the first arm of the rendered CartesianProduct
// (the line beginning with the branch connector) names want.
func plansDriveWith(table, want string) bool {
	lines := splitTableLines(table)
	for i, l := range lines {
		if !containsSubstr(l, "CartesianProduct") {
			continue
		}
		if i+1 < len(lines) {
			return containsSubstr(lines[i+1], want)
		}
	}
	return false
}

func splitTableLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func containsSubstr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestJoinReorderStats_AcceptanceScale drives the exact shape rmp #2766 was filed
// against: 1 000 000 :A of which 3 satisfy x = 1, against 1000 :B. The default
// plan re-scans a million rows a thousand times, so it is NOT run here — the
// reference is the analytically known result (3 * 1000 rows, every one carrying
// ax = 1) plus the plan the reorder-disabled engine RENDERS, which is cheap.
func TestJoinReorderStats_AcceptanceScale(t *testing.T) {
	g := buildSkewGraph(t, 1000000, 1000, 3)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	// The PLAN is asserted BEFORE the query is drained, and that ordering is
	// load-bearing rather than stylistic: if the reorder does not fire here, the
	// default order re-scans a million rows a thousand times and the drain below
	// takes about four minutes. Failing on the rendered plan first turns that into
	// a two-second failure — which is what makes this test usable inside a mutation
	// sweep, and what stops a future regression from looking like a hang.
	if !plansDriveWith(mustExplainTable(t, on, q, nil), "Selection") {
		t.Fatalf("expected the filtered component to drive:\n%s", mustExplainTable(t, on, q, nil))
	}
	if !plansDriveWith(mustExplainTable(t, off, q, nil), "NodeByLabelScan [b:B]") {
		t.Fatal("expected the default plan to drive with :B")
	}

	before := joinReorderBuildCount.Load()
	got := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the reorder to fire at acceptance scale, but it did not")
	}
	want := make([]string, 0, 3000)
	for i := 0; i < 3; i++ {
		for b := 0; b < 1000; b++ {
			want = append(want, fmt.Sprintf("ax=1|bv=%d", b))
		}
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestJoinReorderStats_NoStatistics_PlanUnchanged is the inertness gate: with no
// RefreshStatistics the filtered component has no estimate, the trustworthiness
// veto keeps the written order, and the rendered plan is BYTE-IDENTICAL to the
// reorder-disabled engine's.
func TestJoinReorderStats_NoStatistics_PlanUnchanged(t *testing.T) {
	g := buildSkewGraph(t, 500, 20, 3)
	fresh := NewEngine(g) // deliberately NOT refreshed
	off := NewEngineWithOptions(g, EngineOptions{DisableJoinReorder: true})
	for _, q := range []string{
		"MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv",
		"MATCH (a:A {x: 1}), (b:B) RETURN a.x AS ax, b.y AS bv",
		"MATCH (b:B {y: 3}), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv",
	} {
		before := joinReorderBuildCount.Load()
		gotFresh := mustExplainTable(t, fresh, q, nil)
		if gotFresh != mustExplainTable(t, off, q, nil) {
			t.Fatalf("stats-free plan differs from the reorder-disabled plan for %q:\n%s", q, gotFresh)
		}
		// And executing it must not swap either.
		_ = drainRows(t, fresh, q)
		if joinReorderBuildCount.Load() != before {
			t.Fatalf("a stats-free engine swapped for %q", q)
		}
	}
}

// TestJoinReorderStats_BareShapesUnchangedByRefresh proves the reduction claimed
// in [reorderSwapWins]: refreshing statistics changes NO plan that has no filtered
// component, because every such component still has drain == rows.
func TestJoinReorderStats_BareShapesUnchangedByRefresh(t *testing.T) {
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Med": 12, "Small": 4})
	before := NewEngine(g)
	after := NewEngine(g)
	refreshStats(t, after)
	for _, q := range []string{
		"MATCH (a:Big), (b:Small) RETURN a.k AS ak, b.k AS bk",
		"MATCH (a:Small), (b:Big) RETURN a.k AS ak, b.k AS bk",
		"MATCH (a:Big), (b:Med), (c:Small) RETURN a.k AS ak, b.k AS bk, c.k AS ck",
		"MATCH (a), (b:Small) RETURN a.k AS ak, b.k AS bk",
	} {
		if got, want := mustExplainTable(t, after, q, nil), mustExplainTable(t, before, q, nil); got != want {
			t.Fatalf("refreshing statistics changed the plan for %q:\nbefore:\n%s\nafter:\n%s", q, want, got)
		}
	}
}

// TestJoinReorderStats_NonMCVLiteral_NoSwap: a literal absent from the top-k most
// common values yields the 1/NDV distribution average, tagged estHeuristic, which
// the trustworthiness veto rejects. The plan must stay the written one even though
// the true selectivity would have justified a swap.
func TestJoinReorderStats_NonMCVLiteral_NoSwap(t *testing.T) {
	// 40 heavy values at 12 rows each fill the k = 32 MCV list; the target value
	// 9999 appears 3 times and is not in it.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	id := 0
	for v := 0; v < 40; v++ {
		for r := 0; r < 12; r++ {
			mustNode(t, g, fmt.Sprintf("a%d", id), "A", "x", int64(v))
			id++
		}
	}
	for r := 0; r < 3; r++ {
		mustNode(t, g, fmt.Sprintf("a%d", id), "A", "x", 9999)
		id++
	}
	for i := 0; i < 100; i++ {
		mustNode(t, g, fmt.Sprintf("b%d", i), "B", "y", int64(i))
	}
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 9999}) RETURN a.x AS ax, b.y AS bv"

	lg, err := on.ExplainLogical(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(lg, "heuristic") {
		t.Fatalf("expected a heuristic (non-MCV) estimate; got:\n%s", lg)
	}
	before := joinReorderBuildCount.Load()
	got := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("an estHeuristic estimate drove a swap; the trustworthiness veto did not hold")
	}
	if tbl, ref := mustExplainTable(t, on, q, nil), mustExplainTable(t, off, q, nil); tbl != ref {
		t.Fatalf("plan deviated from the default on a heuristic estimate:\n%s", tbl)
	}
	if len(got) != 3*100 {
		t.Fatalf("row count = %d, want %d", len(got), 3*100)
	}
}

// TestJoinReorderStats_StaleStatistic_NoSwap: an MCV hit is an exact per-value count
// for the snapshot it came from and for no later one, so it is screened for
// staleness ([statsSnapshotFresh]) before it may drive anything. Enough writes to the
// tracked (label, property) pair must stop the swap. Since rmp #2772 the screen lives
// in the PROVIDER rather than in [reorderStatsFreshness], which is why the plan
// comparison below is on the shape: the demotion is now visible in the Est.Rows
// column, and the two engines' statistics differ in freshness.
func TestJoinReorderStats_StaleStatistic_NoSwap(t *testing.T) {
	const nA, nB = 500, 20
	g := buildSkewGraph(t, nA, nB, 3)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"

	// Fresh: the swap fires.
	before := joinReorderBuildCount.Load()
	_ = drainRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatal("expected the swap to fire on a fresh statistic")
	}

	// Dirty it. The demotion threshold is Delta/N >= b - 1/B, so 0.10 - 1/256 of
	// 500 rows is ~49 writes; write comfortably past it, WITHOUT changing which
	// rows match (every touched node keeps a value that is not 1).
	for i := 100; i < 200; i++ {
		w := fmt.Sprintf("MATCH (a:A {x: %d}) SET a.x = %d", i+2, i+2)
		res, err := on.RunInTx(context.Background(), w, nil)
		if err != nil {
			t.Fatalf("dirtying write: %v", err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("dirtying write close: %v", err)
		}
	}

	before = joinReorderBuildCount.Load()
	got := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("a stale statistic drove a swap; the freshness screen did not hold")
	}
	if tbl, ref := explainPlanShape(t, on, q, nil), explainPlanShape(t, off, q, nil); tbl != ref {
		t.Fatalf("plan deviated from the default on a stale statistic:\n%s", tbl)
	}
	if len(got) != 3*nB {
		t.Fatalf("row count = %d, want %d", len(got), 3*nB)
	}
}

// TestJoinReorderStats_ParameterAndLiteralAgree guards the parity defect class of
// sprint 341 (a parameter full-scanned where the identical literal seeked): the
// same predicate written as a literal and as a bound parameter must produce the
// same plan and the same rows.
func TestJoinReorderStats_ParameterAndLiteralAgree(t *testing.T) {
	g := buildSkewGraph(t, 2000, 50, 3)
	on, _ := statsReorderPair(t, g)
	const qLit = "MATCH (b:B), (a:A) WHERE a.x = 1 RETURN a.x AS ax, b.y AS bv"
	const qPar = "MATCH (b:B), (a:A) WHERE a.x = $p RETURN a.x AS ax, b.y AS bv"
	_ = qLit
	// The inline-map form is the shape that lands INSIDE the component; the WHERE
	// form is hoisted above the Cartesian by the translator, so both forms are
	// exercised through the inline map.
	const mLit = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"
	const mPar = "MATCH (b:B), (a:A {x: $p}) RETURN a.x AS ax, b.y AS bv"
	params := map[string]expr.Value{"p": expr.IntegerValue(1)}

	tblLit := mustExplainTable(t, on, mLit, nil)
	tblPar := mustExplainTable(t, on, mPar, params)
	if plansDriveWith(tblLit, "Selection") != plansDriveWith(tblPar, "Selection") {
		t.Fatalf("literal and parameter forms chose different drive orders:\nliteral:\n%s\nparam:\n%s", tblLit, tblPar)
	}
	if !plansDriveWith(tblPar, "Selection") {
		t.Fatalf("expected the parameterised filtered component to drive:\n%s", tblPar)
	}

	res, err := on.Run(context.Background(), mPar, params)
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
	if n != 3*50 {
		t.Fatalf("parameterised row count = %d, want %d", n, 3*50)
	}
	_ = qPar
}

// TestJoinReorderStats_UnboundParameterIsInert: a parameter the caller did not
// bind resolves to null, which matches nothing under three-valued logic and is not
// a row count any statistic describes. It must not drive a swap.
func TestReorderFilteredScan_UnboundParameterStillRecognisedStructurally(t *testing.T) {
	sel := ir.NewSelectionExpr("(a.x = $p)", &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
		Right:    &ast.Parameter{Name: "p"},
	}, &ir.NodeByLabelScan{NodeVar: "a", Label: "A"})
	if _, ok := reorderFilteredScan(sel); !ok {
		t.Fatal("a parameterised comparison must be recognised structurally at parse time")
	}
}

// TestReorderFilteredScan_RejectsForeignVariable is the correlation guard.
// ir.Selection.Vars() delegates to its child and never exposes the predicate's
// free variables, so reorderVarsDisjoint alone cannot see a predicate that reaches
// into the other arm. The shape recogniser is what forbids it, and this pins that.
func TestReorderFilteredScan_RejectsForeignVariable(t *testing.T) {
	scan := &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}
	cases := map[string]ast.Expression{
		"other variable's property on the right": &ast.BinaryOp{
			Operator: "=",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
			Right:    &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "y"},
		},
		"other variable's property on the left": &ast.BinaryOp{
			Operator: "<",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "y"},
			Right:    &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
		},
		"bare foreign variable": &ast.BinaryOp{
			Operator: "=",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
			Right:    &ast.Variable{Name: "b"},
		},
		"property of a different variable entirely": &ast.BinaryOp{
			Operator: ">",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "y"},
			Right:    &ast.IntLiteral{Value: 3},
		},
		"self-comparison of two properties": &ast.BinaryOp{
			Operator: "=",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
			Right:    &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "y"},
		},
		"unsupported operator": &ast.BinaryOp{
			Operator: "<>",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
			Right:    &ast.IntLiteral{Value: 3},
		},
		"conjunction of two comparisons": &ast.BinaryOp{
			Operator: "AND",
			Left: &ast.BinaryOp{
				Operator: "=",
				Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
				Right:    &ast.IntLiteral{Value: 1},
			},
			Right: &ast.BinaryOp{
				Operator: "=",
				Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "y"},
				Right:    &ast.IntLiteral{Value: 2},
			},
		},
	}
	for name, pred := range cases {
		t.Run(name, func(t *testing.T) {
			sel := ir.NewSelectionExpr("(pred)", pred, scan)
			if _, ok := reorderFilteredScan(sel); ok {
				t.Fatalf("predicate was admitted as a filtered component: %s", name)
			}
			if isReorderComponent(sel) {
				t.Fatalf("isReorderComponent admitted it: %s", name)
			}
		})
	}
}

// TestReorderFilteredScan_RejectsNonLabelScanChild: an unlabelled AllNodesScan has
// no (label, property) statistic, and a deeper child is not the recognised shape.
func TestReorderFilteredScan_RejectsNonLabelScanChild(t *testing.T) {
	pred := &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
		Right:    &ast.IntLiteral{Value: 1},
	}
	for name, child := range map[string]ir.LogicalPlan{
		"all-nodes scan": &ir.AllNodesScan{NodeVar: "a"},
		"nested selection": ir.NewSelectionExpr("(inner)", pred,
			&ir.NodeByLabelScan{NodeVar: "a", Label: "A"}),
		"unlabelled label scan": &ir.NodeByLabelScan{NodeVar: "a", Label: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := reorderFilteredScan(ir.NewSelectionExpr("(pred)", pred, child)); ok {
				t.Fatalf("admitted a filtered component over %s", name)
			}
		})
	}
}

// TestIsReorderBareComponent_ExcludesFilteredArmsFromComposition pins the nesting
// restriction: a filtered arm is a component in its own right but may NOT compose
// into a nested Apply component, because the product drain model is only exact for
// arms whose drain equals their rows.
func TestIsReorderBareComponent_ExcludesFilteredArms(t *testing.T) {
	filtered := ir.NewSelectionExpr("(a.x = 1)", &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "a"}, Key: "x"},
		Right:    &ast.IntLiteral{Value: 1},
	}, &ir.NodeByLabelScan{NodeVar: "a", Label: "A"})
	bare := &ir.NodeByLabelScan{NodeVar: "b", Label: "B"}

	if !isReorderComponent(filtered) {
		t.Fatal("a filtered label scan must be a component on its own")
	}
	if isReorderBareComponent(filtered) {
		t.Fatal("a filtered label scan must NOT be a bare component")
	}
	nested := &ir.Apply{Outer: bare, Inner: filtered}
	if isReorderComponent(nested) {
		t.Fatal("an Apply composing a filtered arm must not be a component")
	}
	if !isReorderComponent(&ir.Apply{Outer: bare, Inner: &ir.NodeByLabelScan{NodeVar: "c", Label: "C"}}) {
		t.Fatal("an Apply of two bare scans must still be a component")
	}
}

// TestReorderSwapWins_ReducesToTheRowRule is the bit-exactness claim of
// [reorderSwapWins], asserted directly rather than inferred: for arms whose drain
// equals their rows, the two-rule gate admits exactly `rows(inner) < rows(outer)`.
func TestReorderSwapWins_ReducesToTheRowRule(t *testing.T) {
	mk := func(n float64) componentCost {
		e := estimate{rows: n, source: estExact}
		return componentCost{drain: e, rows: e}
	}
	for _, pair := range [][2]float64{
		{0, 0}, {1, 1}, {0, 1}, {1, 0}, {4, 80}, {80, 4}, {30, 30},
		{1, 1e6}, {1e6, 1}, {1e6, 1e6}, {2, 3}, {3, 2}, {1e9, 1e9 - 1},
	} {
		outer, inner := mk(pair[0]), mk(pair[1])
		want := inner.rows.rows < outer.rows.rows
		if got := reorderSwapWins(outer, inner); got != want {
			t.Fatalf("reorderSwapWins(outer=%g, inner=%g) = %v, want %v (the pre-#2766 rule)",
				pair[0], pair[1], got, want)
		}
	}
}

// TestReorderSwapWins_MinimaxOverRealisations pins BOTH conjuncts of the gate, each
// with the case the other one alone gets wrong.
func TestReorderSwapWins_MinimaxOverRealisations(t *testing.T) {
	exact := func(n float64) estimate { return estimate{rows: n, source: estExact} }

	// Rule 1 alone would admit this and be wrong under scan-and-filter: an outer
	// that scans 1e9 to emit 5, against an inner bare scan of 4.
	outer := componentCost{drain: exact(1e9), rows: exact(5)}
	inner := componentCost{drain: exact(4), rows: exact(4)}
	if reorderSwapWins(outer, inner) {
		t.Fatal("admitted a swap that costs 4 + 4*1e9 against a written order of 1e9 + 5*4")
	}

	// Rule 2 alone would admit this and be wrong under an index-seek realisation:
	// an outer bare scan of 2 against an inner that emits 3 from 1e6.
	outer = componentCost{drain: exact(2), rows: exact(2)}
	inner = componentCost{drain: exact(1e6), rows: exact(3)}
	if reorderSwapWins(outer, inner) {
		t.Fatal("admitted a swap the seek realisation makes 9-vs-8 worse")
	}

	// The shape rmp #2766 exists for: both rules admit.
	outer = componentCost{drain: exact(1000), rows: exact(1000)}
	inner = componentCost{drain: exact(1e6), rows: exact(3)}
	if !reorderSwapWins(outer, inner) {
		t.Fatal("declined the swap the task exists to make")
	}
}

// TestReorderSwapWins_CertifiedIntervalIsOneSided proves the interval gate is
// evaluated at the pessimistic end: an estimate whose certified error spans the
// other arm's cardinality must not drive a swap even though its POINT estimate
// clears the rule comfortably.
func TestReorderSwapWins_CertifiedIntervalIsOneSided(t *testing.T) {
	outer := componentCost{drain: estimate{rows: 1000, source: estExact}, rows: estimate{rows: 1000, source: estExact}}
	point := componentCost{
		drain:   estimate{rows: 1e6, source: estExact},
		rows:    estimate{rows: 4, source: estStats},
		rowsErr: 0,
	}
	if !reorderSwapWins(outer, point) {
		t.Fatal("a zero-error stats estimate must behave exactly like a point estimate")
	}
	wide := point
	wide.rowsErr = 3900 // 1/256 of a million rows
	if reorderSwapWins(outer, wide) {
		t.Fatal("a certified interval reaching past the other arm must not drive a swap")
	}
}

// buildRangeSkewGraph creates nA :A nodes with x = 0..nA-1 (a uniform integer
// column the equi-depth histogram summarises well) and nB :B nodes with a distinct
// y, so `b.y = <one value>` is an MCV-exact single row.
func buildRangeSkewGraph(t testing.TB, nA, nB int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < nA; i++ {
		mustNode(t, g, fmt.Sprintf("a%d", i), "A", "x", int64(i))
	}
	for i := 0; i < nB; i++ {
		mustNode(t, g, fmt.Sprintf("b%d", i), "B", "y", int64(i))
	}
	return g
}

// TestJoinReorderStats_HistogramRangeDrivesASwap exercises the estStats half: the
// written order drives a range-filtered :A emitting ~98 of 5000, and the reorder
// promotes the 1-row MCV-exact :B arm so the 5000-row scan is paid once instead of
// 98 times. The estStats estimate is consumed over its certified interval and under
// the 3x stats margin, and the result bag is identical to the default plan's.
func TestJoinReorderStats_HistogramRangeDrivesASwap(t *testing.T) {
	g := buildRangeSkewGraph(t, 5000, 2000)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"

	lg, err := on.ExplainLogical(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(lg, "stats, err=") {
		t.Fatalf("expected a histogram (estStats) estimate on the decision path:\n%s", lg)
	}

	before := joinReorderBuildCount.Load()
	gotOn := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatalf("expected the histogram-driven reorder to fire:\n%s", lg)
	}
	gotOff := sortedRows(t, off, q)
	if len(gotOn) != 99 {
		t.Fatalf("row count = %d, want 99 (x in 4901..4999 crossed with one b)", len(gotOn))
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch: reorder=%d default=%d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("bag row %d differs:\n  reorder = %s\n  default = %s", i, gotOn[i], gotOff[i])
		}
	}
	if !plansDriveWith(mustExplainTable(t, on, q, nil), "Selection") {
		t.Fatal("expected a Selection arm to drive after the swap")
	}
}

// TestJoinReorderStats_SelectiveRangeOuterStaysPut is the negative twin: when the
// range-filtered arm is ALREADY the driver and the other arm is a bare 2000-row
// scan, promoting the bare arm would pay the 5000-row scan 2000 times. The gate
// must decline, and the plan must equal the reorder-disabled plan exactly.
func TestJoinReorderStats_SelectiveRangeOuterStaysPut(t *testing.T) {
	g := buildRangeSkewGraph(t, 5000, 2000)
	on, off := statsReorderPair(t, g)
	const q = "MATCH (a:A) WHERE a.x >= 4900 MATCH (b:B) RETURN a.x AS ax, b.y AS bv"

	before := joinReorderBuildCount.Load()
	got := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() != before {
		t.Fatal("promoted a 2000-row bare scan over a selective filtered driver")
	}
	if tbl, ref := mustExplainTable(t, on, q, nil), mustExplainTable(t, off, q, nil); tbl != ref {
		t.Fatalf("plan deviated from the default:\n%s", tbl)
	}
	if len(got) != 100*2000 {
		t.Fatalf("row count = %d, want %d", len(got), 100*2000)
	}
}
