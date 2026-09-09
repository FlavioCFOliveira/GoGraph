package cypher

// orderby_key_hoist_test.go — rmp #2662
//
// The gates for projecting a non-projected ORDER BY key into its own hidden
// column instead of passing the whole entity through the projection.
//
// # The defect
//
// `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary` projected a hidden `p`
// (#1805's ORDER-BY passthrough) so the Sort above could evaluate `p.salary`
// against it. The projection's node-variable fast path turns that column into a
// full [expr.NodeValue] — labels AND the entire property bag — once per row.
// Measured on the 120 000-node bench/audit352 fixture at MemProfileRate=1, that
// was 840 001 allocated objects, 7.00 per row, on a query that ships ten.
//
// # What is asserted here
//
// The optimisation is worthless if it changes a single result, so the identity
// gates come first and the counter gate second:
//
//   - [TestOrderByKeyHoistResultsIdentical] — every row, every column, in tie
//     order, byte-identical between the hoisting plan and the pre-#2662 plan, on
//     the SAME query in the SAME binary. Differential, not golden.
//   - [TestOrderByKeyHoistColumnsAreInvisible] — the hidden column appears in no
//     result column list and in no Explain column signature, on either arm.
//   - [TestOrderByKeyHoistRemovesKeyEvaluation] — the reproduction evaluates the
//     sort key ZERO times with the hoist on and once per row with it off. This is
//     the test that fails on the old behaviour.
//
// # The arm seam and the plan cache
//
// [sortseam.SetKeyHoistDisabled] is read at TRANSLATE time, and a translated plan
// is cached per Engine, so an arm cannot be selected by flipping the control
// around a call on a shared engine. [hoistArmEngine] gives each arm its own
// engine, built and warmed with the control already set.

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

// hoistReproduction is the query rmp #2662 was opened on, verbatim.
const hoistReproduction = `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`

// hoistGraph builds n :Person nodes in a chain of :KNOWS edges.
//
// salary ties heavily and is far outside the Go runtime's staticuint64s window,
// so the tie-break path is exercised and no arm can be flattered by a boxing-free
// integer. department gives a second key. Every fifth node is left WITHOUT a
// salary so the NULL-ordering contract is exercised on both arms rather than
// assumed.
func hoistGraph(t testing.TB, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(key)); err != nil {
			t.Fatalf("SetNodeProperty firstName: %v", err)
		}
		if i%5 != 0 {
			salary := int64(100_000 + (i*7919)%97)
			if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(salary)); err != nil {
				t.Fatalf("SetNodeProperty salary: %v", err)
			}
		}
		if err := g.SetNodeProperty(key, "department", lpg.Int64Value(int64(500+i%3))); err != nil {
			t.Fatalf("SetNodeProperty department: %v", err)
		}
	}
	for i := 0; i+1 < n; i++ {
		src, dst := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1)
		if err := g.AddEdge(src, dst, 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(src, dst, "KNOWS")
	}
	return g
}

// hoistArmEngine returns an Engine whose plan cache was populated with the arm
// selected by disabled. The control is restored before returning; the engine
// keeps the plans it translated under it.
//
// Callers must not share an engine between arms: the cached ir.LogicalPlan IS
// the arm.
func hoistArmEngine(t *testing.T, g *lpg.Graph[string, float64], disabled bool, warm []string) *Engine {
	t.Helper()
	restore := sortseam.SetKeyHoistDisabled(disabled)
	defer restore()
	eng := NewEngine(g)
	for _, q := range warm {
		if _, err := eng.Explain(q, nil); err != nil {
			t.Fatalf("warm Explain(%q) disabled=%v: %v", q, disabled, err)
		}
	}
	return eng
}

// hoistCollect drains q and returns its column names and every row, in emission
// order. Tie order is part of the result, so nothing here sorts or normalises.
func hoistCollect(t *testing.T, eng *Engine, q string) (cols []string, rows [][]expr.Value) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols = append(cols, res.Columns()...)
	for res.Next() {
		cp := make([]expr.Value, len(cols))
		for c := range cols {
			cp[c] = res.ValueAt(c)
		}
		rows = append(rows, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	return cols, rows
}

// hoistCorpus is the differential corpus. It deliberately mixes shapes the hoist
// TAKES with shapes it must DECLINE, because a guard that silently stopped
// declining would be the way this change breaks a result.
//
// hoists records the expectation, and [TestOrderByKeyHoistResultsIdentical]
// asserts it per case against the two arms' plans — so a case that stopped
// exercising the seam fails instead of passing vacuously.
type hoistCase struct {
	name   string
	query  string
	hoists bool
}

var hoistCorpus = []hoistCase{
	// ── Shapes the hoist takes ────────────────────────────────────────────────
	{"reproduction", hoistReproduction, true},
	{"sort_unbounded", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary`, true},
	{"sort_desc", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary DESC`, true},
	{"top_limit", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 7`, true},
	{"two_keys_mixed", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary ASC, p.department DESC`, true},
	{"one_projected_one_hoisted", `MATCH (p:Person) RETURN p.department, p.firstName ORDER BY p.department, p.salary`, true},
	{"aliased_output", `MATCH (p:Person) RETURN p.firstName AS fn ORDER BY p.salary`, true},
	{"repeated_key", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary, p.salary DESC`, true},
	{"far_node_key", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName ORDER BY b.salary, a.firstName`, true},
	{"rel_property_key", `MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.firstName ORDER BY r.since, a.firstName`, true},
	{"missing_property_key", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary, p.firstName`, true},

	// ── Shapes the hoist must decline ─────────────────────────────────────────
	// The key is already a projected column: irSortKeys resolves it by schema
	// lookup today, and a second column under the same name would re-point it.
	{"key_is_projected", `MATCH (p:Person) RETURN p.salary, p.firstName ORDER BY p.salary`, false},
	// The receiver is one of the projection's own output names, so the key reads
	// the ALIAS, not the pre-projection entity.
	{"receiver_shadowed_by_alias", `MATCH (p:Person) RETURN p.firstName AS p ORDER BY p.salary`, false},
	// The whole entity is projected, so it costs nothing to keep reading it.
	{"whole_entity_projected", `MATCH (p:Person) RETURN p ORDER BY p.salary`, false},
	// Not a property access on a bare variable.
	{"function_key", `MATCH (p:Person) RETURN p.firstName ORDER BY coalesce(p.salary, 0), p.firstName`, false},
	{"arithmetic_key", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary + 0, p.firstName`, false},
	{"bare_alias_key", `MATCH (p:Person) RETURN p.firstName AS fn ORDER BY fn`, false},
	// The WITH call site, which [appendOrderByPassthrough] never hoists. The
	// shape is otherwise a textbook candidate; it is excluded because `RETURN *`
	// / `WITH *` expand through collectAllVars, which walks the whole child
	// subtree and so reports `p` even though the WITH dropped it from scope. That
	// spurious column takes its VALUE from the ORDER BY entity passthrough, so
	// removing the passthrough turns the node into NULL — a result change. The
	// wildcard cases below are the ones that caught it.
	{"with_then_return", `MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary RETURN fn`, false},
	{"with_limit_then_return", `MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary LIMIT 6 RETURN fn`, false},
	{"wildcard_after_with", `MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary RETURN *`, false},
	{"wildcard_chain", `MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary WITH * RETURN *`, false},
	// The wildcard AT a hoisting RETURN: `p` is among the items, so the receiver
	// is shadowed and the hoist refuses.
	{"wildcard_at_return", `MATCH (p:Person) RETURN * ORDER BY p.salary`, false},
	// UNION across a hoisting branch: the branch itself hoists, but each branch
	// narrows through its own ProduceResults, so the arity both sides agree on
	// must be unchanged.
	{"union_all_hoisting_branch", `MATCH (p:Person) RETURN p.firstName AS c ORDER BY p.salary UNION ALL MATCH (q:Person) RETURN q.firstName AS c`, true},
	{"union_distinct_hoisting_branch", `MATCH (p:Person) RETURN p.firstName AS c ORDER BY p.salary UNION MATCH (q:Person) RETURN q.firstName AS c`, true},
	// DISTINCT dedups the WHOLE row, so a hidden column would change cardinality.
	{"distinct", `MATCH (p:Person) RETURN DISTINCT p.department ORDER BY p.department`, false},
	// The aggregating path has its own ORDER-BY rewrite and never reaches the
	// passthrough at all.
	{"aggregation", `MATCH (p:Person) RETURN p.department AS d, count(*) AS c ORDER BY d`, false},
}

// hoistErrorCorpus holds the shapes whose ORDER BY key EVALUATION FAILS. They are
// hoisted like any other `var.prop` key, and they are listed apart only because
// they are the reason buildIRProjection gives every Hidden item exec.sortKeyValue's
// error-to-NULL contract: a property read on a non-entity is an InvalidArgumentType
// TypeError, which the sort operator swallows to NULL and a projection column would
// otherwise raise as a query failure.
var hoistErrorCorpus = []hoistCase{
	{"non_entity_receiver_string", `UNWIND ['b', 'a', 'c'] AS s RETURN 1 AS x ORDER BY s.foo`, true},
	{"non_entity_receiver_int", `UNWIND [3, 1, 2] AS s RETURN 1 AS x ORDER BY s.foo`, true},
	{"non_entity_receiver_list", `UNWIND [[1], [2]] AS s RETURN 1 AS x ORDER BY s.foo`, true},
}

// hoistWarmSet is every corpus query, for [hoistArmEngine].
func hoistWarmSet() []string {
	out := make([]string, 0, len(hoistCorpus)+len(hoistErrorCorpus))
	for _, c := range hoistCorpus {
		out = append(out, c.query)
	}
	for _, c := range hoistErrorCorpus {
		out = append(out, c.query)
	}
	return out
}

// TestOrderByKeyHoistResultsIdentical is the correctness gate: the hoisting plan
// and the pre-#2662 plan must return the same columns and the same rows in the
// same order — tie order included — for every shape in the corpus.
//
// It is NOT t.Parallel: hoistArmEngine writes a process-global control.
func TestOrderByKeyHoistResultsIdentical(t *testing.T) {
	const n = 240
	g := hoistGraph(t, n)
	warm := hoistWarmSet()
	off := hoistArmEngine(t, g, true, warm) // pre-#2662: entity passthrough
	on := hoistArmEngine(t, g, false, warm) // #2662: hoisted key column

	for _, c := range append(append([]hoistCase{}, hoistCorpus...), hoistErrorCorpus...) {
		c := c
		t.Run(c.name, func(t *testing.T) {
			evalsOff := sortKeyEvalCount.Load()
			colsOff, rowsOff := hoistCollect(t, off, c.query)
			evalsOff = sortKeyEvalCount.Load() - evalsOff

			evalsOn := sortKeyEvalCount.Load()
			colsOn, rowsOn := hoistCollect(t, on, c.query)
			evalsOn = sortKeyEvalCount.Load() - evalsOn

			// NON-VACUITY, on the ONE observable that actually separates the arms.
			// The rendered plan does NOT: the hoist changes a projection's item
			// list, and Explain renders operator names, not items — the two arms of
			// every case in this corpus print the same tree. What does separate them
			// is [sortKeyEvalCount]: a hoisted key resolves by schema lookup and is
			// never evaluated, so the pre-#2662 arm must evaluate strictly more.
			// A "declines" case must show the two arms doing the SAME work, or the
			// guard that is supposed to be refusing has stopped refusing.
			t.Logf("sort-key evaluator invocations: pre-#2662 %d, hoisted %d (%d rows)",
				evalsOff, evalsOn, len(rowsOn))
			if c.hoists && evalsOff <= evalsOn {
				t.Fatalf("declared hoisting, but the pre-#2662 arm evaluated %d sort keys "+
					"and the hoisted arm %d: the seam did not select a different execution, "+
					"so this case compares one program against itself", evalsOff, evalsOn)
			}
			if !c.hoists && evalsOff != evalsOn {
				t.Fatalf("declared NOT hoisting, but the arms evaluated %d and %d sort keys: "+
					"a guard in hoistableSortKeyItem stopped refusing this shape", evalsOff, evalsOn)
			}

			if len(rowsOn) == 0 {
				t.Fatal("query returned no rows; the comparison is vacuous")
			}
			if strings.Join(colsOn, "\x00") != strings.Join(colsOff, "\x00") {
				t.Fatalf("columns changed: hoisted %v, pre-#2662 %v", colsOn, colsOff)
			}
			if len(rowsOn) != len(rowsOff) {
				t.Fatalf("row count changed: hoisted %d, pre-#2662 %d", len(rowsOn), len(rowsOff))
			}
			for i := range rowsOff {
				for c2 := range rowsOff[i] {
					if !expr.Equivalent(rowsOn[i][c2], rowsOff[i][c2]) {
						t.Fatalf("row %d col %d: hoisted %v, pre-#2662 %v "+
							"(first divergence of %d rows; tie order is part of the result)",
							i, c2, rowsOn[i][c2], rowsOff[i][c2], len(rowsOff))
					}
				}
			}
		})
	}
}

// TestOrderByKeyHoistColumnsAreInvisible pins the second half of the contract:
// the hidden key column reaches no caller-visible surface.
//
// The Explain arm matters as much as the result arm. cypher.Engine.Explain
// renders the physical plan, and exec.Project declares its columns from the
// projection's item aliases — so a hidden item that escaped the ProduceResults
// filter would still be invisible in Columns() while showing up in a plan a user
// reads. Both are checked.
func TestOrderByKeyHoistColumnsAreInvisible(t *testing.T) {
	g := hoistGraph(t, 40)
	eng := NewEngine(g)

	cases := []struct {
		query string
		want  []string
	}{
		{`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary`, []string{"p.firstName"}},
		{hoistReproduction, []string{"p.firstName"}},
		{`MATCH (p:Person) RETURN p.firstName AS fn ORDER BY p.salary`, []string{"fn"}},
		{`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary DESC, p.department`, []string{"p.firstName"}},
		{`MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary RETURN fn`, []string{"fn"}},
		// RETURN * / WITH * after a hoisting WITH: the hidden key column must not
		// be re-projected as a real output column by the wildcard.
		//
		// The `p` column is NOT the hidden item and NOT a consequence of #2662. It
		// is a pre-existing defect: collectAllVars expands the wildcard by walking
		// the WHOLE child subtree, so it reaches `p` at the scan that binds it even
		// though the WITH dropped it from scope, and the same two columns come back
		// on a WITH with no ORDER BY at all. It is asserted verbatim here so that
		// this test measures what #2662 changed and nothing else;
		// TestOrderByKeyHoistResultsIdentical proves the two arms agree on it.
		{`MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary RETURN *`, []string{"fn", "p"}},
		{`MATCH (p:Person) WITH p.firstName AS fn ORDER BY p.salary WITH * RETURN *`, []string{"fn", "p"}},
		{`MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName ORDER BY b.salary`, []string{"a.firstName"}},
	}

	for _, c := range cases {
		c := c
		t.Run(c.query, func(t *testing.T) {
			cols, rows := hoistCollect(t, eng, c.query)
			if len(rows) == 0 {
				t.Fatal("query returned no rows; the assertion is vacuous")
			}
			if strings.Join(cols, "\x00") != strings.Join(c.want, "\x00") {
				t.Fatalf("result columns %v, want %v", cols, c.want)
			}
			for i, r := range rows {
				if len(r) != len(c.want) {
					t.Fatalf("row %d has width %d, want %d: a hidden column reached the result",
						i, len(r), len(c.want))
				}
			}
			// The rendered physical plan must not name the hidden column either.
			plan, err := eng.Explain(c.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if strings.Contains(plan, "p.salary") || strings.Contains(plan, "b.salary") {
				t.Fatalf("the hidden ORDER BY key column is named in the rendered plan:\n%s", plan)
			}
		})
	}
}

// TestOrderByKeyHoistRemovesKeyEvaluation is the regression gate proper: it is
// the assertion that FAILS on the pre-#2662 code and passes on the fixed code.
//
// With the hoist on, the sort key is a projected column and irSortKeys resolves
// it by schema lookup (case 1), so the compiled evaluator — [sortKeyEvalCount]'s
// subject — runs ZERO times. With the hoist off it runs once per input row, which
// is what makes the zero meaningful rather than merely small.
//
// It is NOT t.Parallel: hoistArmEngine writes a process-global control.
func TestOrderByKeyHoistRemovesKeyEvaluation(t *testing.T) {
	const n = 500
	g := hoistGraph(t, n)
	warm := []string{hoistReproduction}

	evals := func(eng *Engine) (uint64, int) {
		before := sortKeyEvalCount.Load()
		_, rows := hoistCollect(t, eng, hoistReproduction)
		return sortKeyEvalCount.Load() - before, len(rows)
	}

	off := hoistArmEngine(t, g, true, warm)
	on := hoistArmEngine(t, g, false, warm)

	evalsOff, rowsOff := evals(off)
	evalsOn, rowsOn := evals(on)

	t.Logf("sort-key evaluator invocations over %d scanned rows: pre-#2662 %d, hoisted %d",
		n, evalsOff, evalsOn)

	if rowsOff != 10 || rowsOn != 10 {
		t.Fatalf("shipped rows: pre-#2662 %d, hoisted %d; want 10 each", rowsOff, rowsOn)
	}
	// The control arm. Without it a zero on the hoisted arm could mean the
	// counter, not the optimisation, stopped working.
	if evalsOff != uint64(n) {
		t.Fatalf("pre-#2662 arm evaluated the key %d times over %d rows, want exactly %d "+
			"(once per row, since #2652): the control arm is not the path this task removed",
			evalsOff, n, n)
	}
	if evalsOn != 0 {
		t.Errorf("hoisted arm evaluated the sort key %d times, want 0: the key did not "+
			"resolve to a projected column, so the projection is still carrying the entity",
			evalsOn)
	}
}
