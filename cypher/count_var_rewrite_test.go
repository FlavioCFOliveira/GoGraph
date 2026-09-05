package cypher

// count_var_rewrite_test.go — rmp #2657: count(<pattern-bound var>) → count(*).
//
// The tests are split by what they defend:
//
//   - TestCountVarRewrite_SamePlanAsCountStar — the rewrite FIRES on the shapes it
//     is for, proven by the strongest available oracle: the physical plan for
//     `count(v) AS c` is byte-identical to the plan for `count(*) AS c`.
//   - TestCountVarRewrite_NullBindingsDifferential — the rewrite DOES NOT fire where
//     the two spellings must disagree. This is the test the task requires to fail
//     against a build that rewrites unconditionally, and every row asserts the exact
//     integer both spellings produce, not merely that they differ.
//   - TestCountVarRewrite_DistinctNeverRewritten — count(DISTINCT v) is excluded.
//   - TestCountVarRewrite_AllowlistIsClosed — the guard's admitted set is closed by
//     construction: a default-reject switch, and the admitted cases are pinned so a
//     later widening has to be deliberate.
//
// Every test establishes, from the query's own translated IR, that the shape under
// test WAS a count(<bare var>) candidate, so none of them can pass because the
// rewrite never looked at it. That check is deliberately NOT taken from the
// process-global countVarToCountStarCount: the sibling tests here run t.Parallel()
// and share it, so a "delta of zero" assertion around one query reads their
// increments too and fails at random. Measured during development: three of seven
// differential rows failed that way, reporting up to 4 spurious rewrites for a query
// that was correctly refused.

import (
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countVarGraph is the shared fixture:
//
//	a0..a9   :A   with v = 0..9
//	b0..b4   :B
//	:R edges a0→b0, a1→b1, a2→b2, a3→b3, a4→b4, a5→b0   (6 edges, 5 distinct targets)
//
// The asymmetries are all load-bearing: a6..a9 have NO outgoing :R, so an
// OPTIONAL MATCH over them produces null bindings; a5 shares b0 with a0, so
// count(DISTINCT b) differs from count(b); and no node carries a `missing`
// property, so a projection of one binds null in every row.
func countVarGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range 10 {
		k := fmt.Sprintf("a%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "A"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	for i := range 5 {
		k := fmt.Sprintf("b%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "B"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
	}
	edges := [][2]string{
		{"a0", "b0"}, {"a1", "b1"}, {"a2", "b2"},
		{"a3", "b3"}, {"a4", "b4"}, {"a5", "b0"},
	}
	for _, e := range edges {
		if err := g.AddEdgeLabeled(e[0], e[1], 1, "R"); err != nil {
			t.Fatalf("AddEdgeLabeled %v: %v", e, err)
		}
	}
	return g
}

// countVarScalar runs q and returns its single scalar column as a string, asserting
// exactly one row came back.
func countVarScalar(t *testing.T, eng *Engine, q string) string {
	t.Helper()
	rows := runRows(t, eng, q)
	if len(rows) != 1 {
		t.Fatalf("%q returned %d rows, want exactly 1: %v", q, len(rows), rows)
	}
	return rows[0]
}

// countVarAggregatesOf translates q to logical IR and returns every
// [ir.EagerAggregation] in the plan, outermost first.
//
// It exists because the rewrite's verdict must be asserted in BOTH directions, and
// the process-global counter can only carry one of them (see
// countVarToCountStarCount). Calling [rewriteCountVarToCountStar] on the translated
// IR is local to this test: a sibling test running in parallel cannot move the
// answer, and the returned pointer says exactly what happened — the same pointer
// means "refused", a different one means "rewritten".
func countVarAggregatesOf(t *testing.T, q string) []*ir.EagerAggregation {
	t.Helper()
	astNode, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	plan, err := ir.FromAST(astNode)
	if err != nil {
		t.Fatalf("FromAST %q: %v", q, err)
	}
	var out []*ir.EagerAggregation
	var walk func(ir.LogicalPlan)
	walk = func(n ir.LogicalPlan) {
		if n == nil {
			return
		}
		if agg, ok := n.(*ir.EagerAggregation); ok {
			out = append(out, agg)
		}
		for _, c := range n.Children() {
			walk(c)
		}
	}
	walk(plan)
	if len(out) == 0 {
		t.Fatalf("%q translated to a plan with no EagerAggregation, so there is nothing "+
			"for the rewrite to act on", q)
	}
	return out
}

// countVarRewriteVerdict reports, for the plan of q, how many count(<bare variable>)
// candidates its aggregations carry and how many of those the guard would rewrite.
// Both numbers are derived locally from the translated IR, so neither is affected by
// tests running in parallel.
func countVarRewriteVerdict(t *testing.T, q string) (candidates, rewritten int) {
	t.Helper()
	for _, agg := range countVarAggregatesOf(t, q) {
		for i := range agg.Aggregates {
			if countVarCandidate(&agg.Aggregates[i]) != "" {
				candidates++
			}
		}
		out := rewriteCountVarToCountStar(agg)
		if out == agg {
			continue
		}
		for i := range out.Aggregates {
			if out.Aggregates[i].Argument == "" && agg.Aggregates[i].Argument != "" {
				rewritten++
			}
		}
	}
	return candidates, rewritten
}

// TestCountVarRewrite_SamePlanAsCountStar asserts the rewrite fires by comparing the
// PHYSICAL PLAN of the two spellings. Both are aliased AS c so the aggregate's output
// column name is identical and the two plans are directly comparable strings — a
// rewrite that fires makes them the same program, and nothing weaker than that would
// establish "reaches the same pushdown".
//
// The first row is the acceptance criterion as written, and it is worth recording
// that it ALREADY HELD before this task: tryBuildLabelCountScan admits an argument
// equal to the scan's own NodeVar, so `MATCH (n:A) RETURN count(n)` was answered from
// the label count store already. It is kept as a regression pin, marked as such.
// The rows that the rewrite actually changes are the Selection- and Expand-child
// ones, which no leaf pushdown can serve.
func TestCountVarRewrite_SamePlanAsCountStar(t *testing.T) {
	t.Parallel()
	eng := NewEngine(countVarGraph(t))
	cases := []struct {
		name string
		varQ string
		strQ string
		// wantOp is the operator the shared plan must contain.
		wantOp string
		// preexisting marks a row that already reached wantOp before #2657.
		preexisting bool
		// want is the integer both spellings must return.
		want string
	}{
		{
			name:        "bare_label_scan",
			varQ:        `MATCH (n:A) RETURN count(n) AS c`,
			strQ:        `MATCH (n:A) RETURN count(*) AS c`,
			wantOp:      "LabelCountScan",
			preexisting: true,
			want:        "10",
		},
		{
			name:   "selection_child",
			varQ:   `MATCH (n:A) WHERE n.v >= 0 RETURN count(n) AS c`,
			strQ:   `MATCH (n:A) WHERE n.v >= 0 RETURN count(*) AS c`,
			wantOp: "CountRows",
			want:   "10",
		},
		{
			name:   "expand_child_target_node",
			varQ:   `MATCH (a:A)-[:R]->(b:B) RETURN count(b) AS c`,
			strQ:   `MATCH (a:A)-[:R]->(b:B) RETURN count(*) AS c`,
			wantOp: "CountRows",
			want:   "6",
		},
		{
			name:   "expand_child_relationship",
			varQ:   `MATCH (a:A)-[r:R]->(b:B) RETURN count(r) AS c`,
			strQ:   `MATCH (a:A)-[r:R]->(b:B) RETURN count(*) AS c`,
			wantOp: "CountRows",
			want:   "6",
		},
		{
			name:   "expand_child_source_node",
			varQ:   `MATCH (a:A)-[:R]->(b:B) RETURN count(a) AS c`,
			strQ:   `MATCH (a:A)-[:R]->(b:B) RETURN count(*) AS c`,
			wantOp: "CountRows",
			want:   "6",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Local oracle: the rewrite really fired on this query's own IR.
			cands, rewritten := countVarRewriteVerdict(t, tc.varQ)
			if cands != 1 {
				t.Fatalf("%q carries %d count(<bare var>) candidates, want exactly 1",
					tc.varQ, cands)
			}
			if rewritten != 1 {
				t.Fatalf("the guard refused %q (%d of %d candidates rewritten), so it never "+
					"became count(*)", tc.varQ, rewritten, cands)
			}
			// Process-global counter: only ever asserted to have MOVED, because sibling
			// tests run in parallel and share it.
			before := countVarToCountStarCount.Load()
			varPlan := explainOK(t, eng, tc.varQ)
			if countVarToCountStarCount.Load() == before {
				t.Fatalf("building %q moved no rewrite counter at all:\n%s", tc.varQ, varPlan)
			}
			strPlan := explainOK(t, eng, tc.strQ)
			if varPlan != strPlan {
				t.Fatalf("%q and %q do not compile to the same physical plan:\n"+
					"count(v):\n%s\ncount(*):\n%s", tc.varQ, tc.strQ, varPlan, strPlan)
			}
			if !strings.Contains(varPlan, tc.wantOp) {
				t.Fatalf("the shared plan for %q does not contain %s:\n%s",
					tc.varQ, tc.wantOp, varPlan)
			}
			if got := countVarScalar(t, eng, tc.varQ); got != tc.want {
				t.Fatalf("%q = %s, want %s", tc.varQ, got, tc.want)
			}
			if got := countVarScalar(t, eng, tc.strQ); got != tc.want {
				t.Fatalf("%q = %s, want %s", tc.strQ, got, tc.want)
			}
		})
	}
}

// countVarDifferentialCases are the shapes where count(v) and count(*) MUST disagree,
// each one a distinct reason for the guard to refuse. Every row is a real difference
// in the answer, so every row goes RED against a build that rewrites unconditionally.
// A row whose two spellings agreed would be a decoration, not a differential, and
// would not defend anything.
var countVarDifferentialCases = []struct {
	name string
	// why names the guard clause that must refuse this shape.
	why     string
	varQ    string
	strQ    string
	wantVar string
	wantStr string
}{
	{
		name:    "optional_expand_target_null",
		why:     "ir.OptionalExpand / ir.OptionalApply binds ToVar null for a4..a9",
		varQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) RETURN count(b) AS c`,
		strQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) RETURN count(*) AS c`,
		wantVar: "6",
		wantStr: "10",
	},
	{
		name:    "optional_expand_relationship_null",
		why:     "the same operator binds RelVar null, not only ToVar",
		varQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[r:R]->(b:B) RETURN count(r) AS c`,
		strQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[r:R]->(b:B) RETURN count(*) AS c`,
		wantVar: "6",
		wantStr: "10",
	},
	{
		name:    "optional_expand_grouped",
		why:     "a grouping key does not make the optional binding non-null",
		varQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) RETURN count(b) AS c ORDER BY c`,
		strQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) RETURN count(*) AS c ORDER BY c`,
		wantVar: "6",
		wantStr: "10",
	},
	{
		name:    "projection_binds_absent_property",
		why:     "ir.Projection can bind the variable to any expression, including null",
		varQ:    `MATCH (a:A) WITH a.missing AS m RETURN count(m) AS c`,
		strQ:    `MATCH (a:A) WITH a.missing AS m RETURN count(*) AS c`,
		wantVar: "0",
		wantStr: "10",
	},
	{
		name:    "projection_launders_optional_binding",
		why:     "a WITH between the optional expansion and the aggregate hides nothing",
		varQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) WITH b AS t RETURN count(t) AS c`,
		strQ:    `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b:B) WITH b AS t RETURN count(*) AS c`,
		wantVar: "6",
		wantStr: "10",
	},
	{
		name:    "unwind_binds_null_element",
		why:     "ir.Unwind binds its element variable to whatever the list holds",
		varQ:    `UNWIND [1, null, 3] AS x RETURN count(x) AS c`,
		strQ:    `UNWIND [1, null, 3] AS x RETURN count(*) AS c`,
		wantVar: "2",
		wantStr: "3",
	},
	{
		name:    "unwind_over_pattern",
		why:     "the same, with a pattern below so the subtree is not a bare Unwind",
		varQ:    `MATCH (a:A) WHERE a.v = 0 UNWIND [1, null] AS x RETURN count(x) AS c`,
		strQ:    `MATCH (a:A) WHERE a.v = 0 UNWIND [1, null] AS x RETURN count(*) AS c`,
		wantVar: "1",
		wantStr: "2",
	},
}

// TestCountVarRewrite_NullBindingsDifferential is the test the rewrite's safety rests
// on: for each shape the guard must refuse, count(v) and count(*) are driven side by
// side and both integers pinned. Against a build whose guard always returns true,
// every row here fails, because in every row the rewrite would change the answer.
//
// Non-vacuity: each row asserts the query DID carry a count(<bare var>) candidate,
// so "not rewritten" cannot be satisfied by a shape the rewrite never looked at. That
// assertion, and the refusal itself, are read from the query's own translated IR
// rather than from the process-global counter, which sibling parallel tests share.
func TestCountVarRewrite_NullBindingsDifferential(t *testing.T) {
	t.Parallel()
	eng := NewEngine(countVarGraph(t))
	for _, tc := range countVarDifferentialCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Non-vacuity FIRST: this shape must actually be a count(<bare var>) the
			// rewrite looked at, or the rest proves nothing about the guard.
			cands, rewritten := countVarRewriteVerdict(t, tc.varQ)
			if cands == 0 {
				t.Fatalf("%q carries no count(<bare var>) candidate at all (%s), so this row "+
					"cannot defend the guard", tc.varQ, tc.why)
			}
			// The ANSWER is the contract, so it is checked before the diagnosis: a wrong
			// rewrite must be caught by the integer the query returns, not only by an
			// assertion about the planner's internals.
			gotVar := countVarScalar(t, eng, tc.varQ)
			if gotVar != tc.wantVar {
				t.Fatalf("%q = %s, want %s — %s", tc.varQ, gotVar, tc.wantVar, tc.why)
			}
			gotStr := countVarScalar(t, eng, tc.strQ)
			if gotStr != tc.wantStr {
				t.Fatalf("%q = %s, want %s", tc.strQ, gotStr, tc.wantStr)
			}
			if gotVar == gotStr {
				t.Fatalf("count(v)=%s and count(*)=%s agree for %q, so this row is not a "+
					"differential and would survive an unconditional rewrite",
					gotVar, gotStr, tc.name)
			}
			// The diagnosis, last: WHY the answer is right.
			if rewritten != 0 {
				t.Fatalf("%q: the guard rewrote %d of %d candidates to count(*) — %s",
					tc.varQ, rewritten, cands, tc.why)
			}
		})
	}
}

// TestCountVarRewrite_DistinctNeverRewritten pins the DISTINCT exclusion. count(v)
// and count(DISTINCT v) differ on this fixture (a5 and a0 share b0), so erasing the
// argument of the DISTINCT form would change the answer from 5 to 6.
func TestCountVarRewrite_DistinctNeverRewritten(t *testing.T) {
	t.Parallel()
	eng := NewEngine(countVarGraph(t))
	const (
		distinctQ = `MATCH (a:A)-[:R]->(b:B) RETURN count(DISTINCT b) AS c`
		plainQ    = `MATCH (a:A)-[:R]->(b:B) RETURN count(b) AS c`
	)
	// Positive control: the non-DISTINCT spelling of the SAME aggregate over the SAME
	// pattern IS rewritten, so what the assertion below isolates is DISTINCT and not
	// the shape.
	if cands, rewritten := countVarRewriteVerdict(t, plainQ); cands != 1 || rewritten != 1 {
		t.Fatalf("the non-DISTINCT control %q was not rewritten (%d of %d candidates), so "+
			"this test is not isolating DISTINCT", plainQ, rewritten, cands)
	}
	if got := countVarScalar(t, eng, plainQ); got != "6" {
		t.Fatalf("%q = %s, want 6", plainQ, got)
	}

	// DISTINCT is refused by countVarCandidate, BEFORE the null-safety walk, so it is
	// not even a candidate — which is the stronger statement.
	cands, rewritten := countVarRewriteVerdict(t, distinctQ)
	if cands != 0 {
		t.Fatalf("count(DISTINCT b) counted as %d rewrite candidate(s); DISTINCT must be "+
			"excluded by countVarCandidate before the walk ever runs", cands)
	}
	if rewritten != 0 {
		t.Fatalf("count(DISTINCT b) was rewritten %d time(s); DISTINCT deduplicates on the "+
			"argument's VALUE, so erasing the argument changes the answer", rewritten)
	}
	if got := countVarScalar(t, eng, distinctQ); got != "5" {
		t.Fatalf("%q = %s, want 5 (a0 and a5 share b0)", distinctQ, got)
	}
}

// countVarAdmittedCases is the set of ir plan types countVarWalk admits. It is
// duplicated here on purpose: [TestCountVarRewrite_AllowlistIsClosed] compares it
// against the type switch in the source, so widening the guard cannot happen without
// this list — and the review of it — changing too.
var countVarAdmittedCases = []string{
	// Explicit rejections, named in the switch so each is individually testable.
	"ir.Argument",
	"ir.OptionalApply",
	"ir.OptionalExpand",
	// Non-nullable pattern binders.
	"ir.AllNodesScan",
	"ir.Expand",
	"ir.NodeByIndexRangeScan",
	"ir.NodeByIndexSeek",
	"ir.NodeByLabelScan",
	// Transparent, never a binder of the counted variable.
	"ir.Selection",
	"ir.VarLengthExpand",
}

// TestCountVarRewrite_AllowlistIsClosed reads the guard's own source and asserts two
// structural properties that no behavioural test can establish:
//
//  1. the type switch ends in a `default: return false`, so every ir plan type not
//     named in it — including any added later — declines the rewrite; and
//  2. the set of types it names is exactly countVarAdmittedCases.
//
// Inspection is an unreliable instrument for "can this widen silently?", and a
// behavioural test can only cover the shapes someone thought to write. Parsing the
// switch closes the set by construction instead of by review.
func TestCountVarRewrite_AllowlistIsClosed(t *testing.T) {
	t.Parallel()
	const src = "count_var_rewrite.go"
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, src, nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var sw *goast.TypeSwitchStmt
	goast.Inspect(f, func(n goast.Node) bool {
		fd, ok := n.(*goast.FuncDecl)
		if !ok || fd.Name == nil || fd.Name.Name != "countVarWalk" {
			return true
		}
		goast.Inspect(fd, func(m goast.Node) bool {
			if ts, ok := m.(*goast.TypeSwitchStmt); ok && sw == nil {
				sw = ts
			}
			return true
		})
		return false
	})
	if sw == nil {
		t.Fatalf("no type switch found in countVarWalk in %s; the guard's shape changed and "+
			"this test no longer checks anything", src)
	}

	var named []string
	sawDefault := false
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*goast.CaseClause)
		if !ok {
			continue
		}
		if cc.List == nil {
			sawDefault = true
			if len(cc.Body) != 1 {
				t.Fatalf("the default clause has %d statements; it must be exactly "+
					"`return false` so an unrecognised plan type cannot fall through",
					len(cc.Body))
			}
			ret, isRet := cc.Body[0].(*goast.ReturnStmt)
			if !isRet || len(ret.Results) != 1 {
				t.Fatalf("the default clause does not return a single value: %T", cc.Body[0])
			}
			lit, isIdent := ret.Results[0].(*goast.Ident)
			if !isIdent || lit.Name != "false" {
				t.Fatalf("the default clause returns %v, not false: an unrecognised plan "+
					"type would ADMIT the rewrite", ret.Results[0])
			}
			continue
		}
		for _, e := range cc.List {
			star, isStar := e.(*goast.StarExpr)
			if !isStar {
				t.Fatalf("case type %T is not a pointer to an ir type", e)
			}
			sel, isSel := star.X.(*goast.SelectorExpr)
			if !isSel {
				t.Fatalf("case type %T is not a qualified ir type", star.X)
			}
			pkg, isIdent := sel.X.(*goast.Ident)
			if !isIdent {
				t.Fatalf("case type qualifier %T is not an identifier", sel.X)
			}
			named = append(named, pkg.Name+"."+sel.Sel.Name)
		}
	}
	if !sawDefault {
		t.Fatalf("countVarWalk's type switch has NO default clause, so an unrecognised ir " +
			"plan type takes whatever the fallthrough does instead of declining")
	}
	sort.Strings(named)
	want := make([]string, len(countVarAdmittedCases))
	copy(want, countVarAdmittedCases)
	sort.Strings(want)
	if strings.Join(named, ",") != strings.Join(want, ",") {
		t.Fatalf("countVarWalk names a different set of ir types than "+
			"countVarAdmittedCases records:\n in source: %v\n recorded:  %v\n"+
			"Widening the guard is a semantic change and must update both.", named, want)
	}
}

// TestCountVarRewrite_GuardRefusesNonAdmittedSubtrees drives countVarWalk directly
// over constructed IR, so the refusals do not depend on which physical plan the
// translator happens to produce for a given query text. It is the direct-unit
// counterpart of the differential test above.
func TestCountVarRewrite_GuardRefusesNonAdmittedSubtrees(t *testing.T) {
	t.Parallel()
	scan := func() ir.LogicalPlan { return &ir.AllNodesScan{NodeVar: "n"} }
	cases := []struct {
		name  string
		child ir.LogicalPlan
		v     string
		want  bool
	}{
		{"bare_scan", scan(), "n", true},
		{"selection_over_scan",
			&ir.Selection{Predicate: "true", Child: scan()}, "n", true},
		{"expand_target",
			&ir.Expand{FromVar: "n", RelVar: "r", ToVar: "m", Child: scan()}, "m", true},
		{"expand_relationship",
			&ir.Expand{FromVar: "n", RelVar: "r", ToVar: "m", Child: scan()}, "r", true},
		{"expand_preserves_source",
			&ir.Expand{FromVar: "n", RelVar: "r", ToVar: "m", Child: scan()}, "n", true},
		{"varlen_passthrough_source",
			&ir.VarLengthExpand{FromVar: "n", RelVar: "rs", ToVar: "m", Child: scan()}, "n", true},

		{"unbound_variable", scan(), "zzz", false},
		{"optional_expand_target",
			&ir.OptionalExpand{FromVar: "n", RelVar: "r", ToVar: "m", Child: scan()}, "m", false},
		{"optional_expand_poisons_source",
			&ir.OptionalExpand{FromVar: "n", RelVar: "r", ToVar: "m", Child: scan()}, "n", false},
		{"argument_leaf", &ir.Argument{Variables: []string{"n"}}, "n", false},
		{"optional_apply",
			&ir.OptionalApply{Outer: scan(), Inner: &ir.Argument{Variables: []string{"n"}}}, "n", false},
		{"projection_rebinds",
			&ir.Projection{Items: []ir.ProjectionItem{{Name: "n"}}, Child: scan()}, "n", false},
		{"unwind_binds",
			&ir.Unwind{ElementVar: "x", Child: scan()}, "x", false},
		{"unwind_anywhere_in_subtree",
			&ir.Selection{Predicate: "true", Child: &ir.Unwind{ElementVar: "x", Child: scan()}}, "n", false},
		{"varlen_as_binder",
			&ir.VarLengthExpand{FromVar: "n", RelVar: "rs", ToVar: "m", Child: scan()}, "m", false},
		{"distinct", &ir.Distinct{Child: scan()}, "n", false},
		{"limit", &ir.Limit{Count: 1, Child: scan()}, "n", false},
		{"apply", &ir.Apply{Outer: scan(), Inner: scan()}, "n", false},
		{"union", &ir.Union{Left: scan(), Right: scan()}, "n", false},
		{"nil_child", nil, "n", false},
		{"empty_var", scan(), "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := countVarIsNonNullPatternBound(tc.child, tc.v); got != tc.want {
				t.Fatalf("countVarIsNonNullPatternBound(%s, %q) = %v, want %v",
					tc.name, tc.v, got, tc.want)
			}
		})
	}
}
