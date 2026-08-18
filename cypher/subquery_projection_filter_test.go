package cypher_test

// subquery_projection_filter_test.go — regression battery for rmp #2507:
// a filtered EXISTS { … } / COUNT { … } in PROJECTION position answered the
// wrong value.
//
// # The defect
//
// `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(:Person
// {name:'B'}) }` returned false on a graph where the plain MATCH of the same
// pattern returns one row. The same subquery in WHERE position was correct, and
// so was the same projection without the inner property filter — which is what
// made the class invisible: every unfiltered probe in the suite passed.
//
// # Why it happened
//
// [subqueryEvaluator.compileSubAST] built the inner plan with a nil parameter
// map and a nil *buildOpts, so four things the inner predicate needs were absent:
//
//  1. PARAMS. cypher/parser.StripLiterals hoists a string literal inside a MATCH
//     or WHERE onto an auto-parameter, so `{name:'B'}` reaches the planner as
//     `{name: $«auto_1»}`. With no parameter map the reference evaluated to NULL
//     and the comparison yielded NULL, which the Filter drops. A user-supplied
//     `$param` inside a subquery was broken by the same nil for the same reason,
//     independently of hoisting.
//  2. edgeVarMeta, so an inner relationship variable stayed a raw IntegerValue
//     and `r.since` read a property off an integer (→ NULL).
//  3. subEval, so a subquery NESTED inside a subquery had no evaluator and
//     silently answered false / 0.
//  4. patEval, so a pattern predicate inside a subquery's WHERE failed the query
//     outright with "no PatternEvaluator wired".
//
// The fix threads the enclosing query's parameters and a SCOPED CHILD buildOpts
// (buildOpts.forSubquery) into the inner build. The child carries only
// scope-independent services; every column-indexed and plan-keyed field starts
// empty so the inner build populates it against the INNER row layout.
//
// Every case below states the ground truth as a plain MATCH wherever one exists,
// so the expectation is anchored on the engine's own uncontested behaviour rather
// than on a hand-computed number.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newSubqueryFilterEngine returns a multigraph in-memory engine holding:
//
//	(A:Person {age:30}) -[:KNOWS {since:2020}]-> (B:Person {age:40})
//	(A:Person)          -[:KNOWS {since:1999}]-> (C:Person {age:50})
//	(B:Person)          -[:KNOWS]->              (D:Person {age:60})
//	(A:Person)          -[:WORKS_AT]->           (ACME:Company)
//
// A therefore KNOWS exactly two people, exactly one of whom is named 'B', and
// exactly one of whom (B) in turn KNOWS someone named 'D'. Those three facts are
// what separate a correct filtered answer from an unfiltered one.
func newSubqueryFilterEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Person {name:'A', age:30})-[:KNOWS {since:2020}]->(:Person {name:'B', age:40})`)
	runSetup(t, eng, `MATCH (a:Person {name:'A'}) CREATE (a)-[:KNOWS {since:1999}]->(:Person {name:'C', age:50})`)
	runSetup(t, eng, `MATCH (b:Person {name:'B'}) CREATE (b)-[:KNOWS]->(:Person {name:'D', age:60})`)
	runSetup(t, eng, `MATCH (a:Person {name:'A'}) CREATE (a)-[:WORKS_AT]->(:Company {name:'ACME'})`)
	return eng
}

// renderCol renders one projected value as "type(value)". The TYPE is part of the
// rendering on purpose: a bare "%v" cannot tell BoolValue(true) from
// StringValue("true"), so an assertion written against it could pass on a value of
// the wrong kind (the lesson of rmp #2457).
func renderCol(v any) string { return fmt.Sprintf("%T(%v)", v, v) }

// runCol runs q and returns the "c" column of every row, rendered by renderCol.
func runCol(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("Run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		out = append(out, renderCol(res.Record()["c"]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain(%s): %v", q, err)
	}
	return out
}

// assertCol asserts the rendered "c" column of q equals want.
func assertCol(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value, want ...string) {
	t.Helper()
	got := runCol(t, eng, q, params)
	if len(got) != len(want) {
		t.Fatalf("query %s\n  got  %d rows %v\n  want %d rows %v", q, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("query %s\n  row %d: got %s, want %s", q, i, got[i], want[i])
		}
	}
}

const (
	wantTrue  = "expr.BoolValue(true)"
	wantFalse = "expr.BoolValue(false)"
	wantZero  = "expr.IntegerValue(0)"
	wantOne   = "expr.IntegerValue(1)"
	wantTwo   = "expr.IntegerValue(2)"
)

// TestSubqueryGroundTruth_2507 pins the facts the rest of the file asserts
// against, using plain MATCH — the access path no part of #2507 touches. If this
// test fails the fixture changed and every expectation below is void.
func TestSubqueryGroundTruth_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(b:Person {name:'B'}) RETURN count(*) AS c`,
		nil, wantOne)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(b:Person) RETURN count(*) AS c`,
		nil, wantTwo)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[r:KNOWS]->(b:Person) WHERE r.since = 2020 RETURN count(*) AS c`,
		nil, wantOne)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:WORKS_AT]->(x:Company) RETURN count(*) AS c`,
		nil, wantOne)
	assertCol(t, eng,
		`MATCH (b:Person {name:'B'})-[:KNOWS]->(:Person {name:'D'}) RETURN count(*) AS c`,
		nil, wantOne)
}

// TestSubqueryProjectionFilter_2507 is the core regression: a filtered subquery in
// PROJECTION position must answer the same thing the equivalent MATCH counts.
//
// The unfiltered rows are CONTROLS. They were correct before the fix and must stay
// correct after it: they are the only evidence that the fix did not simply make
// every subquery report "found something".
func TestSubqueryProjectionFilter_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
		want   string
	}{
		// ── EXISTS in projection position ──────────────────────────────────
		{
			name:  "exists_inline_string_property",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } AS c`,
			want:  wantTrue,
		},
		{
			name:  "exists_inline_string_property_no_match",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } AS c`,
			want:  wantFalse,
		},
		{
			name:  "exists_unfiltered_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(:Person) } AS c`,
			want:  wantTrue,
		},
		{
			name:  "exists_where_string_property",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.name = 'B' } AS c`,
			want:  wantTrue,
		},

		// ── COUNT in projection position ───────────────────────────────────
		{
			name:  "count_inline_string_property",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } AS c`,
			want:  wantOne,
		},
		{
			name:  "count_inline_string_property_no_match",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } AS c`,
			want:  wantZero,
		},
		{
			name:  "count_unfiltered_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person) } AS c`,
			want:  wantTwo,
		},
		{
			name:  "count_where_string_property",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.name = 'B' } AS c`,
			want:  wantOne,
		},

		// ── numeric literals: NOT hoisted by StripLiterals, so these two were
		// already correct. They are controls for the params half of the fix.
		{
			name:  "count_inline_numeric_property_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person {age:40}) } AS c`,
			want:  wantOne,
		},
		{
			name:  "count_where_numeric_property_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.age = 40 } AS c`,
			want:  wantOne,
		},

		// ── a USER parameter inside the subquery. Broken by the same nil
		// parameter map, and independent of literal hoisting.
		{
			name:   "count_user_parameter_inline",
			query:  `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person {name: $nm}) } AS c`,
			params: map[string]expr.Value{"nm": expr.StringValue("B")},
			want:   wantOne,
		},
		{
			name:   "count_user_parameter_where",
			query:  `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.name = $nm } AS c`,
			params: map[string]expr.Value{"nm": expr.StringValue("B")},
			want:   wantOne,
		},
		{
			name:   "count_user_parameter_no_match",
			query:  `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person {name: $nm}) } AS c`,
			params: map[string]expr.Value{"nm": expr.StringValue("ZZZ")},
			want:   wantZero,
		},

		// ── a filter on a RELATIONSHIP variable's property. This half needs
		// edgeVarMeta, not params: `2020` is a number and is never hoisted.
		{
			name:  "count_relationship_property",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 2020 } AS c`,
			want:  wantOne,
		},
		{
			name:  "exists_relationship_property",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 1999 } AS c`,
			want:  wantTrue,
		},
		{
			name:  "exists_relationship_property_no_match",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 1234 } AS c`,
			want:  wantFalse,
		},
		{
			name:  "count_relationship_type_function",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[r]->() WHERE type(r) = 'WORKS_AT' } AS c`,
			want:  wantOne,
		},

		// ── a filter on a LABEL, inline and in the inner WHERE.
		{
			name:  "count_inline_label_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-->(x:Company) } AS c`,
			want:  wantOne,
		},
		{
			name:  "count_where_label_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-->(x) WHERE x:Company } AS c`,
			want:  wantOne,
		},
		{
			name:  "count_where_label_and_string_property",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-->(x) WHERE x:Company AND x.name = 'ACME' } AS c`,
			want:  wantOne,
		},

		// ── UNCORRELATED inner pattern: no outer variable appears inside, and
		// it was broken too, which is what proved the defect is not about
		// correlation.
		{
			name:  "count_uncorrelated_filtered",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (x:Person {name:'B'}) } AS c`,
			want:  wantOne,
		},
		{
			name:  "count_uncorrelated_unfiltered_CONTROL",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (x:Person) } AS c`,
			want:  "expr.IntegerValue(4)",
		},

		// ── an outer-bound variable used INSIDE the pattern (both endpoints
		// fixed), with and without an inner filter.
		{
			name:  "exists_both_endpoints_bound_CONTROL",
			query: `MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(b) } AS c`,
			want:  wantTrue,
		},
		{
			name:  "exists_outer_bound_variable_in_inner_filter",
			query: `MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.name > a.name } AS c`,
			want:  wantTrue,
		},
		{
			name:  "count_outer_bound_variable_in_inner_filter",
			query: `MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE x.age > a.age } AS c`,
			want:  wantTwo,
		},
	}

	for _, tc := range cases {
		// NOT t.Parallel(): the subtests share one Engine, and concurrent
		// executions of the same plan-cached query whose projection holds an
		// EXISTS { } / COUNT { } race on the cached AST — ir.TranslateSubquery runs
		// per execution and installs synthetic variable names into it
		// (cypher/ir/match.go:1463-1470). That is a pre-existing engine defect,
		// unrelated to #2507 and reproducible on shapes that predate it, so this
		// battery does not assert against it; each TOP-LEVEL test still runs in
		// parallel, each against its own Engine.
		t.Run(tc.name, func(t *testing.T) {
			assertCol(t, eng, tc.query, tc.params, tc.want)
		})
	}
}

// TestSubqueryWherePositionControls_2507 pins the WHERE-position EXISTS forms,
// which were already correct: an EXISTS predicate at the top of a WHERE is lifted
// to a SemiApply by the planner and never reaches the subquery evaluator, so it
// must be BYTE-identical before and after the fix.
//
// The non-matching filter is what makes this suite discriminating: without it,
// "row survives" would also be produced by a predicate that had stopped filtering
// at all.
//
// COUNT in WHERE position is NOT here — see
// [TestSubqueryWhereCountIsAlsoAffected_2507]. It is not lifted, so it shares the
// projection path's defect.
func TestSubqueryWherePositionControls_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } RETURN a.name AS c`,
		nil)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE NOT EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE EXISTS { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 2020 } RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE EXISTS { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 1234 } RETURN a.name AS c`,
		nil)
}

// TestSubqueryWhereCountIsAlsoAffected_2507 records a shape #2507's report did not
// list: a filtered COUNT { … } compared against a value in WHERE position.
//
// It looks like a WHERE-position form and so like a control, but only a bare
// EXISTS predicate is lifted to a SemiApply. `COUNT { … } = 1` is an arithmetic
// comparison, stays an expression, and is therefore answered by the same compiled
// inner plan the projection uses — so it was broken in exactly the same way and
// for exactly the same reason.
//
// The row that already answered correctly (`= 1` against a pattern that matches
// nothing) is kept: it is the one that would still pass if the evaluator had
// simply started returning a fixed non-zero count.
func TestSubqueryWhereCountIsAlsoAffected_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } = 1 RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } = 1 RETURN a.name AS c`,
		nil)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'ZZZ'}) } = 0 RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE COUNT { MATCH (a)-[r:KNOWS]->(:Person) WHERE r.since = 2020 } > 0 RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
}

// TestSubqueryNeighbouringShapes_2507 covers the shapes adjacent to the RETURN
// projection: a WITH projection, an aggregate argument, a nested subquery, and a
// subquery inside a pattern comprehension.
//
// The nested and pattern-predicate rows are the two that the params half of the
// fix does NOT reach: they need the child buildOpts to carry subEval and patEval.
// Before the fix the nested case answered false and the pattern-predicate case
// failed the query with "no PatternEvaluator wired".
func TestSubqueryNeighbouringShapes_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	t.Run("with_projection", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) WITH a, COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } AS c RETURN c`,
			nil, wantOne)
	})
	t.Run("with_projection_then_filter", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) WITH a, COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } AS n WHERE n = 1 RETURN n AS c`,
			nil, wantOne)
	})
	t.Run("inside_aggregation", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person) RETURN sum(COUNT { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) }) AS c`,
			nil, wantOne)
	})
	t.Run("inside_aggregation_unfiltered_CONTROL", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person) RETURN sum(COUNT { MATCH (a)-[:KNOWS]->(:Person) }) AS c`,
			nil, "expr.IntegerValue(3)")
	})
	t.Run("inside_case_expression", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN CASE WHEN EXISTS { MATCH (a)-[:KNOWS]->(:Person {name:'B'}) } THEN 'yes' ELSE 'no' END AS c`,
			nil, `expr.StringValue("yes")`)
	})
	t.Run("nested_subquery", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE EXISTS { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } } AS c`,
			nil, wantTrue)
	})
	t.Run("nested_subquery_no_match", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE EXISTS { MATCH (x)-[:KNOWS]->(:Person {name:'ZZZ'}) } } AS c`,
			nil, wantFalse)
	})
	t.Run("nested_count_subquery", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE COUNT { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } = 1 } AS c`,
			nil, wantOne)
	})
	t.Run("pattern_predicate_inside_subquery", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE (x)-[:KNOWS]->(:Person) } AS c`,
			nil, wantTrue)
	})
	t.Run("pattern_predicate_inside_subquery_no_match", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:KNOWS]->(x:Person) WHERE (x)-[:WORKS_AT]->(:Company) } AS c`,
			nil, wantFalse)
	})
	t.Run("pattern_comprehension_over_subquery", func(t *testing.T) {
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN [ (a)-[:KNOWS]->(x:Person) WHERE EXISTS { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } | x.name ] AS c`,
			nil, `expr.ListValue(["B"])`)
	})
	t.Run("subquery_inside_pattern_comprehension_projection", func(t *testing.T) {
		// REDUCED to a sum rather than asserted element-wise: a pattern
		// comprehension enumerates the anchor's neighbours in adjacency order,
		// which this engine does not specify and which is genuinely unstable run to
		// run — asserting [1, 0] failed under -race purely because the two
		// neighbours came back the other way round. The sum is order-insensitive
		// and still discriminating: before the fix the subquery inside the
		// comprehension's PROJECTION answered 0 for every element, so the sum was 0.
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN reduce(s = 0, v IN [ (a)-[:KNOWS]->(x:Person) | COUNT { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } ] | s + v) AS c`,
			nil, wantOne)
		// The element count is asserted separately, so a comprehension that had
		// stopped producing elements at all could not pass the sum by accident.
		assertCol(t, eng,
			`MATCH (a:Person {name:'A'}) RETURN size([ (a)-[:KNOWS]->(x:Person) | COUNT { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } ]) AS c`,
			nil, wantTwo)
	})
}

// TestSubqueryProjectionFilter_MultiRowCorrelation_2507 drives the subquery over
// SEVERAL outer rows, so a compiled-plan cache that survived the fix cannot
// answer the first row correctly and then reuse a stale binding for the rest.
//
// The compiled pipeline is built once per subquery occurrence and re-seeded per
// outer row; nothing in the fix changes that, and this is the assertion that says
// so.
func TestSubqueryProjectionFilter_MultiRowCorrelation_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	assertCol(t, eng,
		`MATCH (p:Person) RETURN COUNT { MATCH (p)-[:KNOWS]->(:Person {name:'B'}) } AS c ORDER BY p.name`,
		nil, wantOne, wantZero, wantZero, wantZero)
	assertCol(t, eng,
		`MATCH (p:Person) RETURN COUNT { MATCH (p)-[:KNOWS]->(:Person {name:'D'}) } AS c ORDER BY p.name`,
		nil, wantZero, wantOne, wantZero, wantZero)
	assertCol(t, eng,
		`MATCH (p:Person) RETURN EXISTS { MATCH (p)-[r:KNOWS]->(:Person) WHERE r.since = 2020 } AS c ORDER BY p.name`,
		nil, wantTrue, wantFalse, wantFalse, wantFalse)
}

// TestSubqueryProjectionFilter_RewriteFastPathsStillFire_2507 pins the two
// adjacency fast paths (#2232 degree rewrite, #2235 labelled single hop) that
// answer an unfiltered subquery WITHOUT building an inner plan. They are the code
// the fix must not disturb: a filtered pattern is refused by both recognisers and
// falls through to the compiled plan, which is the path #2507 repairs.
func TestSubqueryProjectionFilter_RewriteFastPathsStillFire_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	// Degree rewrite shape: unlabelled, unfiltered far node.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-->() } AS c`,
		nil, "expr.IntegerValue(3)")
	// Labelled single-hop shape: a label on the far node, still no property.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:Person) } AS c`,
		nil, wantTwo)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)-[:WORKS_AT]->(:Company) } AS c`,
		nil, wantTrue)
	// A label the graph does not intern: a real zero, not an unresolvable.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(:NoSuchLabel) } AS c`,
		nil, wantZero)
}

// TestSubqueryAnonVarCollision_2507 guards a defect found while building the
// battery above, with a root cause of its own: the anonymous-variable name space
// was not shared between an outer scope and a subquery's inner scope.
//
// ir.TranslateSubquery started a fresh translator, whose anonymous counter
// restarts at zero, so the inner pattern minted `__anon_0` for its own anonymous
// relationship — the name the OUTER pattern's anonymous relationship already
// occupied in the seed row. The inner build then bound its column over the seeded
// one and the subquery counted nothing.
//
// Every query here uses a NUMERIC filter and reads no relationship variable, so
// none of it can be answered by the parameter or edgeVarMeta halves of the #2507
// fix: this pair isolates the collision and nothing else. The control differs by
// ONE character of query text — the outer relationship is named — which is what
// made the cause identifiable.
func TestSubqueryAnonVarCollision_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	// Ground truth: A KNOWS B and C; only B in turn KNOWS someone aged 60.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(x:Person) OPTIONAL MATCH (x)-[:KNOWS]->(y:Person) WHERE y.age = 60 RETURN count(y) AS c`,
		nil, wantOne)

	// Outer relationship ANONYMOUS: the shape that collided.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(x:Person) RETURN COUNT { MATCH (x)-[:KNOWS]->(y:Person) WHERE y.age = 60 } AS c ORDER BY x.name`,
		nil, wantOne, wantZero)
	// Outer relationship NAMED: the control, correct before the fix and after it.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[rr:KNOWS]->(x:Person) RETURN COUNT { MATCH (x)-[:KNOWS]->(y:Person) WHERE y.age = 60 } AS c ORDER BY x.name`,
		nil, wantOne, wantZero)

	// Anonymous on BOTH sides and at two depths, so a fix that merely renamed one
	// level would still fail here.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(x:Person) RETURN EXISTS { MATCH (x)-[:KNOWS]->(:Person {age:60}) } AS c ORDER BY x.name`,
		nil, wantTrue, wantFalse)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:KNOWS]->(x:Person) WHERE COUNT { MATCH (x)-[:KNOWS]->(:Person {age:60}) } = 1 } AS c`,
		nil, wantOne)
}

// TestPatternPredicateHoistedLiteral_2507 guards the SECOND site of #2507's
// parameter defect, in a different feature: a bare pattern predicate.
//
// patternEvaluator.checkNodePattern matches an inline property map itself rather
// than through a planned Filter, and evaluated it with a nil parameter map. Since
// parser.StripLiterals hoists a string literal inside a WHERE — and a pattern
// predicate is a WHERE — `WHERE (a)-[:KNOWS]->(:Person {name:'B'})` reached the
// matcher with the value already replaced by a parameter reference, which
// evaluated to NULL and rejected every row.
//
// The numeric rows are the controls: a number is never hoisted, so they were
// correct before the fix and must stay correct. The mismatching rows are what stop
// this passing on a matcher that had simply stopped filtering.
func TestPatternPredicateHoistedLiteral_2507(t *testing.T) {
	t.Parallel()
	eng := newSubqueryFilterEngine(t)

	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {name:'B'}) RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {name:'ZZZ'}) RETURN a.name AS c`,
		nil)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE NOT (a)-[:KNOWS]->(:Person {name:'ZZZ'}) RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {name: $nm}) RETURN a.name AS c`,
		map[string]expr.Value{"nm": expr.StringValue("B")}, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {name: $nm}) RETURN a.name AS c`,
		map[string]expr.Value{"nm": expr.StringValue("ZZZ")})

	// Numeric controls: never hoisted, therefore never broken.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {age:40}) RETURN a.name AS c`,
		nil, `expr.StringValue("A")`)
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) WHERE (a)-[:KNOWS]->(:Person {age:999}) RETURN a.name AS c`,
		nil)

	// A pattern comprehension whose WHERE holds a subquery: the second half of the
	// pattern-evaluator fix, where the SubqueryEvaluator was hard-coded nil.
	assertCol(t, eng,
		`MATCH (a:Person {name:'A'}) RETURN [ (a)-[:KNOWS]->(x:Person) WHERE EXISTS { MATCH (x)-[:KNOWS]->(:Person {name:'D'}) } | x.name ] AS c`,
		nil, `expr.ListValue(["B"])`)
}
