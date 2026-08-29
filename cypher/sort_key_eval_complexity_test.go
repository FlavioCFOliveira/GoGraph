package cypher

// sort_key_eval_complexity_test.go — #2652
//
// The COMPLEXITY oracle for the ORDER BY sort-key evaluator.
//
// Before #2652 both [exec.Sort] and [exec.Top] evaluated every sort key from
// INSIDE their comparators, so a compiled ORDER BY key evaluator ran O(n log n)
// times to order n rows — and each run allocated a fresh expr.RowContext map
// plus a freshly sorted schema walk. The fix materialises each key once per row
// (decorate-sort-undecorate) and compares only the precomputed values.
//
// No timing measurement can separate O(n) from O(n log n) at a size a unit test
// can afford: the ratio between 4n and 4n·(log 4n / log n) is about 1.2, well
// inside this project's measured noise floor. The oracle is therefore a COUNTER
// ([sortKeyEvalCount]), and the assertion is on its GROWTH RATE:
//
//	n -> 4n must multiply the count by exactly 4 (linear), not by ~4.8
//	(4·log2(4000)/log2(1000) = 4.80, the n log n growth).
//
// Both arms of the ratio are asserted, so the test fails whether the count
// regresses to n log n or collapses to something sub-linear (which would mean
// the keys stopped being evaluated at all).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// sortComplexityQuery is the #2652 reproduction. `p.salary` is NOT projected, so
// irSortKeys cannot resolve it by schema lookup (Case 1) and compiles an
// expression evaluator instead (Case 2) — the shape whose evaluation count this
// file measures.
//
// It carries NO pagination clause. It used to be spelled `SKIP 0 LIMIT 10`,
// chosen because any SKIP blocked ORDER BY+LIMIT fusion and so forced the full
// Sort; #2509 removed that blocker, and `SKIP 0 LIMIT 10` now fuses into
// Skip(0) over Top(10) — which is the whole point of that task. An unbounded
// ORDER BY is the shape that is a Sort by definition rather than by a planner
// limitation, so it is the durable spelling for this oracle.
// TestSortKeyEvalPlanIsSort is the guard that fails if that ever stops being
// true.
const sortComplexityQuery = `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary`

// sortComplexityGraph builds n :Person nodes carrying the two properties the
// reproduction needs.
//
// salary is deliberately NOT monotonic in i and NOT distinct: (i*2_654_435_761)
// mod 65_536 scatters the key across the sort space so the comparator does real
// work, and the modulus is smaller than n at the larger sample size so ties are
// exercised too. Every value is >= 100_000 + 0, i.e. far outside the Go
// runtime's staticuint64s window (< 256), so no measurement here can be
// flattered by a boxing-free integer.
func sortComplexityGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(key)); err != nil {
			t.Fatalf("SetNodeProperty firstName %s: %v", key, err)
		}
		salary := int64(100_000 + (i*2_654_435_761)%65_536)
		if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(salary)); err != nil {
			t.Fatalf("SetNodeProperty salary %s: %v", key, err)
		}
	}
	return g
}

// sortKeyEvalsFor runs the reproduction over an n-node graph and returns how
// many times the compiled sort-key evaluator was invoked.
//
// The graph is built through the lpg API rather than through CREATE queries, so
// no sort-key evaluation from the setup can leak into the measured delta.
func sortKeyEvalsFor(t *testing.T, n int) (evals uint64, rows int) {
	t.Helper()
	g := sortComplexityGraph(t, n)
	eng := NewEngine(g)

	before := sortKeyEvalCount.Load()
	res, err := eng.Run(context.Background(), sortComplexityQuery, nil)
	if err != nil {
		t.Fatalf("Run(n=%d): %v", n, err)
	}
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain(n=%d): %v", n, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(n=%d): %v", n, err)
	}
	return sortKeyEvalCount.Load() - before, rows
}

// TestSortKeyEvalPlanIsSort is the non-vacuity guard for the whole file: it
// fails if the reproduction query stops compiling to a Sort. Without it a
// planner change that fused this shape into Top (or dropped the sort entirely)
// would leave every assertion below measuring a different program while still
// passing.
func TestSortKeyEvalPlanIsSort(t *testing.T) {
	g := sortComplexityGraph(t, 64)
	eng := NewEngine(g)
	plan, err := eng.Explain(sortComplexityQuery, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "Sort") {
		t.Fatalf("reproduction no longer compiles to a Sort; plan:\n%s", plan)
	}
	if strings.Contains(plan, "Top") {
		t.Fatalf("reproduction fused into Top, so it no longer exercises Sort; plan:\n%s", plan)
	}
}

// TestSortKeyEvalIsLinearInRows is acceptance criterion 1 of #2652.
func TestSortKeyEvalIsLinearInRows(t *testing.T) {
	const (
		small = 1000
		large = 4000
	)

	evalsSmall, rowsSmall := sortKeyEvalsFor(t, small)
	evalsLarge, rowsLarge := sortKeyEvalsFor(t, large)

	// Both arms must have shipped every row, or the sort never ran over the
	// full input and the counts describe nothing.
	if rowsSmall != small || rowsLarge != large {
		t.Fatalf("shipped rows: n=%d -> %d, n=%d -> %d; want the full input each",
			small, rowsSmall, large, rowsLarge)
	}
	if evalsSmall == 0 || evalsLarge == 0 {
		t.Fatalf("sort-key evaluator never ran (n=%d -> %d evals, n=%d -> %d evals): "+
			"the ORDER BY key resolved by schema lookup, so this test measures nothing",
			small, evalsSmall, large, evalsLarge)
	}

	ratio := float64(evalsLarge) / float64(evalsSmall)
	t.Logf("sort-key evaluations: n=%d -> %d, n=%d -> %d, ratio %.4f "+
		"(linear target 4.0000; n log n would be 4.8016)",
		small, evalsSmall, large, evalsLarge, ratio)

	// One key, one evaluation per row: the count IS the row count.
	if evalsSmall != uint64(small) {
		t.Errorf("n=%d: %d sort-key evaluations, want exactly %d (one per row)",
			small, evalsSmall, small)
	}
	if evalsLarge != uint64(large) {
		t.Errorf("n=%d: %d sort-key evaluations, want exactly %d (one per row)",
			large, evalsLarge, large)
	}

	// The growth-rate assertion, stated independently of the exact counts so it
	// still bites if the per-row constant changes. n log n growth over this
	// interval is 4.80; anything at or above 4.4 is that regression.
	if ratio >= 4.4 {
		t.Errorf("sort-key evaluations grow super-linearly: ratio %.4f >= 4.4 "+
			"(n=%d -> %d, n=%d -> %d); the comparator is evaluating keys again",
			ratio, small, evalsSmall, large, evalsLarge)
	}
	if ratio < 3.6 {
		t.Errorf("sort-key evaluations grow sub-linearly: ratio %.4f < 3.6 "+
			"(n=%d -> %d, n=%d -> %d); keys are no longer evaluated once per row",
			ratio, small, evalsSmall, large, evalsLarge)
	}
}

// topComplexityQuery is the same reproduction WITH a pagination clause, so
// ORDER BY + SKIP + LIMIT fuses into Skip over Top (#2509) and the twin
// call-site family in cypher/exec/top.go is exercised end to end. The SKIP is
// present deliberately: it is the clause that used to defeat the fusion, so this
// query is also the end-to-end witness that it no longer does.
const topComplexityQuery = `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`

// TestTopKeyEvalIsLinearInRows is TestSortKeyEvalIsLinearInRows for the Top
// operator. It is a separate test because Top's evaluation count depends on the
// limit as well as on n, so only the GROWTH in n is assertable as a ratio; the
// decorated path makes it exactly one evaluation per input row regardless of the
// limit, which is the stronger statement asserted here.
func TestTopKeyEvalIsLinearInRows(t *testing.T) {
	const (
		small = 1000
		large = 4000
	)

	run := func(n int) uint64 {
		t.Helper()
		g := sortComplexityGraph(t, n)
		eng := NewEngine(g)
		plan, err := eng.Explain(topComplexityQuery, nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if !strings.Contains(plan, "Top") {
			t.Fatalf("reproduction no longer compiles to a Top, so this test does not "+
				"exercise cypher/exec/top.go; plan:\n%s", plan)
		}
		before := sortKeyEvalCount.Load()
		res, err := eng.Run(context.Background(), topComplexityQuery, nil)
		if err != nil {
			t.Fatalf("Run(n=%d): %v", n, err)
		}
		rows := 0
		for res.Next() {
			rows++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain(n=%d): %v", n, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close(n=%d): %v", n, err)
		}
		if rows != 10 {
			t.Fatalf("n=%d shipped %d rows, want 10", n, rows)
		}
		return sortKeyEvalCount.Load() - before
	}

	evalsSmall := run(small)
	evalsLarge := run(large)
	ratio := float64(evalsLarge) / float64(evalsSmall)
	t.Logf("Top sort-key evaluations: n=%d -> %d, n=%d -> %d, ratio %.4f",
		small, evalsSmall, large, evalsLarge, ratio)

	if evalsSmall != uint64(small) {
		t.Errorf("n=%d: %d sort-key evaluations, want exactly %d (one per input row)",
			small, evalsSmall, small)
	}
	if evalsLarge != uint64(large) {
		t.Errorf("n=%d: %d sort-key evaluations, want exactly %d (one per input row)",
			large, evalsLarge, large)
	}
	if ratio >= 4.4 {
		t.Errorf("Top sort-key evaluations grow super-linearly: ratio %.4f >= 4.4", ratio)
	}
}

// TestOrderByResultsIdenticalAcrossSeam is acceptance criterion 3 at the QUERY
// level, and simultaneously the proof that the interleaved single-binary A/B the
// profiler needs actually works: the same Engine, the same graph, and the same
// query produce byte-identical result rows — tie order included — on both arms
// of [sortseam.SetKeyDecorationDisabled].
//
// The fixture is built so that ties dominate: salaryTieModulus is far smaller
// than the node count, so most rows share a sort key and only the tie order
// distinguishes a correct rewrite from an incorrect one. The full result is
// compared, not a LIMITed window, because a LIMIT would hide any transposition
// past its boundary.
//
// It is NOT t.Parallel: it flips a process-global control.
func TestOrderByResultsIdenticalAcrossSeam(t *testing.T) {
	const n = 800
	const salaryTieModulus = 11

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		// firstName is unique, so any reordering of tied rows is visible.
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(fmt.Sprintf("p%05d", i))); err != nil {
			t.Fatalf("SetNodeProperty firstName: %v", err)
		}
		// salary ties heavily and is >= 256; department adds a second key so the
		// multi-key tie-break path is exercised too.
		if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(int64(1000+i%salaryTieModulus))); err != nil {
			t.Fatalf("SetNodeProperty salary: %v", err)
		}
		if err := g.SetNodeProperty(key, "department", lpg.Int64Value(int64(500+i%3))); err != nil {
			t.Fatalf("SetNodeProperty department: %v", err)
		}
	}
	eng := NewEngine(g)

	queries := []string{
		// Sort, single evaluator-backed key, ASC.
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0`,
		// Sort, single evaluator-backed key, DESC.
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary DESC SKIP 0`,
		// Sort, two evaluator-backed keys with mixed direction.
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary ASC, p.department DESC SKIP 0`,
		// Sort, one projected key (ColIdx) and one evaluator-backed key.
		`MATCH (p:Person) RETURN p.department, p.firstName ORDER BY p.department, p.salary SKIP 0`,
		// Top: ORDER BY + LIMIT fused, limit well below the input size.
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 25`,
		// Top: multi-key, mixed direction.
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary DESC, p.department ASC LIMIT 40`,
		// Top: limit at the input size, i.e. every row admitted.
		fmt.Sprintf(`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT %d`, n),
	}

	collect := func(q string) [][]expr.Value {
		t.Helper()
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run(%q): %v", q, err)
		}
		var out [][]expr.Value
		ncols := len(res.Columns())
		for res.Next() {
			cp := make([]expr.Value, ncols)
			for c := 0; c < ncols; c++ {
				cp[c] = res.ValueAt(c)
			}
			out = append(out, cp)
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain(%q): %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close(%q): %v", q, err)
		}
		return out
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			restoreLegacy := sortseam.SetKeyDecorationDisabled(true)
			legacyEvalsBefore := sortKeyEvalCount.Load()
			legacy := collect(q)
			legacyEvals := sortKeyEvalCount.Load() - legacyEvalsBefore
			restoreLegacy()

			restoreDec := sortseam.SetKeyDecorationDisabled(false)
			decEvalsBefore := sortKeyEvalCount.Load()
			decorated := collect(q)
			decEvals := sortKeyEvalCount.Load() - decEvalsBefore
			restoreDec()

			// NON-VACUITY: the seam must have moved the execution. Without this
			// the whole comparison could be the decorated path against itself.
			if legacyEvals <= decEvals {
				t.Fatalf("legacy arm evaluated %d sort keys, decorated arm %d: the seam "+
					"did not select the legacy path, so this subtest compared the "+
					"decorated path against itself", legacyEvals, decEvals)
			}
			t.Logf("sort-key evaluations legacy=%d decorated=%d (%.1fx) over %d rows",
				legacyEvals, decEvals, float64(legacyEvals)/float64(decEvals), len(decorated))

			if len(decorated) != len(legacy) {
				t.Fatalf("decorated returned %d rows, legacy %d", len(decorated), len(legacy))
			}
			if len(decorated) == 0 {
				t.Fatal("query returned no rows; the comparison is vacuous")
			}
			for i := range legacy {
				if len(decorated[i]) != len(legacy[i]) {
					t.Fatalf("row %d: decorated width %d, legacy width %d",
						i, len(decorated[i]), len(legacy[i]))
				}
				for c := range legacy[i] {
					if !expr.Equivalent(decorated[i][c], legacy[i][c]) {
						t.Fatalf("row %d col %d: decorated %v, legacy %v (first divergence of %d rows)",
							i, c, decorated[i][c], legacy[i][c], len(legacy))
					}
				}
			}
		})
	}
}

// TestSortKeyEvalLegacyArmIsSuperLinear pins the OTHER half of the complexity
// claim: that the path #2652 replaced really did grow as n log n.
//
// Without it, TestSortKeyEvalIsLinearInRows asserts only that the current code is
// linear, and a reader has to take the "it used to be n log n" half on trust from
// a log file captured during one session. Driving the legacy arm through the seam
// makes both halves reproducible in one binary, and it is a second, independent
// check that the seam actually selects a different execution — if it silently
// stopped working, this test fails rather than the differential tests quietly
// becoming vacuous.
//
// It is NOT t.Parallel: it flips a process-global control.
func TestSortKeyEvalLegacyArmIsSuperLinear(t *testing.T) {
	const (
		small = 1000
		large = 4000
	)
	// 4 * log2(4000)/log2(1000) = 4.8016 is the n log n growth over this
	// interval. The threshold sits above the linear 4.0 and below that, so the
	// test distinguishes the two rather than merely accepting both.
	const superLinearFloor = 4.4

	restore := sortseam.SetKeyDecorationDisabled(true)
	defer restore()

	evalsSmall, rowsSmall := sortKeyEvalsFor(t, small)
	evalsLarge, rowsLarge := sortKeyEvalsFor(t, large)
	if rowsSmall != small || rowsLarge != large {
		t.Fatalf("shipped rows: n=%d -> %d, n=%d -> %d; want the full input each",
			small, rowsSmall, large, rowsLarge)
	}

	ratio := float64(evalsLarge) / float64(evalsSmall)
	t.Logf("LEGACY arm sort-key evaluations: n=%d -> %d (%.1fx the row count), "+
		"n=%d -> %d (%.1fx), ratio %.4f",
		small, evalsSmall, float64(evalsSmall)/float64(small),
		large, evalsLarge, float64(evalsLarge)/float64(large), ratio)

	if evalsSmall <= uint64(small) || evalsLarge <= uint64(large) {
		t.Fatalf("legacy arm evaluated the key %d times at n=%d and %d times at n=%d, "+
			"i.e. no more than once per row: the seam did not select the legacy path, "+
			"so every differential test in this package is comparing the decorated "+
			"path against itself", evalsSmall, small, evalsLarge, large)
	}
	if ratio < superLinearFloor {
		t.Errorf("legacy arm ratio %.4f < %.2f: it is no longer the super-linear path "+
			"#2652 replaced, so it is not a valid control arm", ratio, superLinearFloor)
	}
}

// TestOrderByColIdxResultsIdenticalAcrossSeam is TestOrderByResultsIdenticalAcrossSeam
// for the shape that test CANNOT cover: an ORDER BY every one of whose keys is a
// PROJECTED column, so irSortKeys resolves all of them by schema lookup (Case 1)
// and compiles no evaluator at all.
//
// # Why it needs its own test
//
// TestOrderByResultsIdenticalAcrossSeam proves the seam moved the execution by
// requiring the legacy arm to evaluate strictly more sort keys than the decorated
// arm. On this shape BOTH arms evaluate exactly ZERO, so that guard fires and the
// query cannot be added to that table. The shape is nonetheless the most common
// ORDER BY there is — `RETURN p.salary ... ORDER BY p.salary` — and it goes
// through decorate-sort-undecorate exactly like the evaluator-backed one: the
// permutation, the cycle-following undecorate and the tie order are all the same
// code.
//
// # The non-vacuity guard, inverted
//
// Instead of "the counter moved", the guard here is "the counter did NOT move".
// That is what proves the subtest is exercising Case 1: if a projection change
// ever made one of these keys resolve to an evaluator, the queries would silently
// become duplicates of the other test's and this one would stop covering the
// ColIdx path. Asserting moved == 0 fails loudly instead.
//
// Tie order is the property under test, so salary ties heavily (mod 11 over 800
// rows) and firstName is unique and projected, which makes any transposition of
// tied rows visible in the compared output.
//
// It is NOT t.Parallel: it flips a process-global control.
func TestOrderByColIdxResultsIdenticalAcrossSeam(t *testing.T) {
	const n = 800
	const salaryTieModulus = 11

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(fmt.Sprintf("p%05d", i))); err != nil {
			t.Fatalf("SetNodeProperty firstName: %v", err)
		}
		if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(int64(1000+i%salaryTieModulus))); err != nil {
			t.Fatalf("SetNodeProperty salary: %v", err)
		}
		if err := g.SetNodeProperty(key, "department", lpg.Int64Value(int64(500+i%3))); err != nil {
			t.Fatalf("SetNodeProperty department: %v", err)
		}
	}
	eng := NewEngine(g)

	queries := []string{
		// Sort, one projected key, heavy ties.
		`MATCH (p:Person) RETURN p.salary, p.firstName ORDER BY p.salary SKIP 0`,
		`MATCH (p:Person) RETURN p.salary, p.firstName ORDER BY p.salary DESC SKIP 0`,
		// Sort, two projected keys, mixed direction — exercises the multi-key
		// tie-break inside keysLess with no evaluator anywhere.
		`MATCH (p:Person) RETURN p.salary, p.department, p.firstName ORDER BY p.salary ASC, p.department DESC SKIP 0`,
		// Top: limit well below the input, so the heap replaces repeatedly.
		`MATCH (p:Person) RETURN p.salary, p.firstName ORDER BY p.salary LIMIT 40`,
		// Top: multi-key, mixed direction.
		`MATCH (p:Person) RETURN p.salary, p.department, p.firstName ORDER BY p.salary DESC, p.department ASC LIMIT 40`,
		// Top: limit at the input size, i.e. every row admitted.
		fmt.Sprintf(`MATCH (p:Person) RETURN p.salary, p.firstName ORDER BY p.salary LIMIT %d`, n),
	}

	collect := func(q string) [][]expr.Value {
		t.Helper()
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run(%q): %v", q, err)
		}
		var out [][]expr.Value
		ncols := len(res.Columns())
		for res.Next() {
			cp := make([]expr.Value, ncols)
			for c := 0; c < ncols; c++ {
				cp[c] = res.ValueAt(c)
			}
			out = append(out, cp)
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain(%q): %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close(%q): %v", q, err)
		}
		return out
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			before := sortKeyEvalCount.Load()

			restoreLegacy := sortseam.SetKeyDecorationDisabled(true)
			legacy := collect(q)
			restoreLegacy()

			restoreDec := sortseam.SetKeyDecorationDisabled(false)
			decorated := collect(q)
			restoreDec()

			// NON-VACUITY, inverted: this file's other tests require the counter
			// to MOVE. Here it must not move at all, or the query stopped being
			// the ColIdx shape and this subtest silently duplicates coverage the
			// evaluator tests already have.
			if moved := sortKeyEvalCount.Load() - before; moved != 0 {
				t.Fatalf("the sort-key evaluator ran %d times: this query no longer "+
					"resolves every ORDER BY key by schema lookup, so it no longer covers "+
					"the ColIdx path that TestOrderByResultsIdenticalAcrossSeam cannot reach",
					moved)
			}
			if len(decorated) == 0 {
				t.Fatal("query returned no rows; the comparison is vacuous")
			}
			if len(decorated) != len(legacy) {
				t.Fatalf("decorated returned %d rows, legacy %d", len(decorated), len(legacy))
			}
			for i := range legacy {
				if len(decorated[i]) != len(legacy[i]) {
					t.Fatalf("row %d: decorated width %d, legacy width %d",
						i, len(decorated[i]), len(legacy[i]))
				}
				for c := range legacy[i] {
					if !expr.Equivalent(decorated[i][c], legacy[i][c]) {
						t.Fatalf("row %d col %d: decorated %v, legacy %v (first divergence of %d rows)",
							i, c, decorated[i][c], legacy[i][c], len(legacy))
					}
				}
			}
			t.Logf("%d rows, tie order identical, 0 sort-key evaluations (pure ColIdx)", len(decorated))
		})
	}
}
