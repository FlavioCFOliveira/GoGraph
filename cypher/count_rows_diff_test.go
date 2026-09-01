package cypher

// count_rows_diff_test.go — differential + correctness tests for the general
// count(*) row count (#2625).
//
// A group-by-less, non-DISTINCT count(*) has a CONSTANT aggregate argument (see
// aggArgItem: expr.BoolValue(true)), so the serial pipeline materialised one
// fresh single-column row per input row purely so CountAgg could null-check a
// value that is never null. Over 7 million relationships that was about 36% of
// the query. exec.CountRows counts the child's rows instead.
//
// The tests assert the path ENGAGES for count(*) over a non-scan child, DECLINES
// for every shape whose argument is not constant, that the answer is identical
// to the serial pipeline, and — the semantic that a wrong implementation would
// break — that count(*) still counts rows whose bindings are NULL.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildCountRowsGraph makes a small graph with typed relationships and one node
// deliberately left without any outgoing FRIEND edge, so OPTIONAL MATCH produces
// a genuine null-bearing row.
func buildCountRowsGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	const n = 12
	for i := 0; i < n; i++ {
		if err := g.AddNode(fmt.Sprintf("u%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	// Every node but the last gets two outgoing FRIEND edges.
	for i := 0; i < n-1; i++ {
		for j := 1; j <= 2; j++ {
			if err := g.AddEdgeLabeled(fmt.Sprintf("u%d", i), fmt.Sprintf("u%d", (i+j)%n), 1, "FRIEND"); err != nil {
				t.Fatalf("AddEdgeLabeled: %v", err)
			}
		}
	}
	return g
}

// scalarOf runs q and returns its single scalar cell, draining the result.
func scalarOf(t *testing.T, e *Engine, q string) string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		out = append(out, res.ValueAt(0).String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err %q: %v", q, err)
	}
	_ = res.Close()
	if len(out) != 1 {
		t.Fatalf("%q returned %d rows, want exactly 1: %v", q, len(out), out)
	}
	return out[0]
}

// TestCountRows_EngagesOverNonScanChild proves the path fires for a typed
// relationship count — the shape #2625 was filed against, whose child is an
// Expand and which therefore reaches none of the bare-scan leaf pushdowns.
func TestCountRows_EngagesOverNonScanChild(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))
	before := countRowsBuildCount.Load()
	got := scalarOf(t, e, `MATCH ()-[:FRIEND]->() RETURN count(*)`)
	if countRowsBuildCount.Load() == before {
		t.Error("CountRows did not engage for count(*) over an Expand")
	}
	if want := "22"; got != want {
		t.Errorf("count(*) = %s, want %s", got, want)
	}
}

// TestCountRows_DeclinesWhenArgumentIsNotConstant proves the gate is narrow: an
// argument that must be evaluated per row keeps the pre-projection, and a grouping
// key needs it too.
//
// `MATCH (a)-[:FRIEND]->() RETURN count(a)` USED TO BE the first row here, and it is
// now served by this operator: rmp #2657 normalises count(<bare pattern-bound var>)
// to count(*) before any recogniser runs, because a is bound by a non-optional
// ir.Expand and so cannot be null. That is a change of what reaches the gate, not a
// widening of the gate — CountRows still refuses every non-empty argument. The rows
// below are the ones the normalisation does NOT touch, so the gate's own narrowness
// is still under test:
//
//   - a PROPERTY argument: never a rewrite candidate, and genuinely per-row.
//   - DISTINCT: excluded by the rewrite, and dedups on the argument's value.
//   - an OPTIONAL binding: the rewrite's null-safety walk refuses it, and count(r)
//     must skip the null rows count(*) would include.
//   - a grouping key: needs the pre-projection regardless of the argument.
//
// The engaged direction for count(a) is asserted in
// TestCountRows_EngagesForNormalisedCountVar below.
func TestCountRows_DeclinesWhenArgumentIsNotConstant(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))
	for _, q := range []string{
		`MATCH (a)-[:FRIEND]->() RETURN count(a.name)`,
		`MATCH (a)-[:FRIEND]->() RETURN count(DISTINCT a)`,
		`MATCH (a) OPTIONAL MATCH (a)-[r:FRIEND]->() RETURN count(r)`,
		`MATCH (a)-[:FRIEND]->(b) RETURN a, count(*)`,
	} {
		before := countRowsBuildCount.Load()
		res, err := e.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", q, err)
		}
		for res.Next() {
		}
		_ = res.Close()
		if countRowsBuildCount.Load() != before {
			t.Errorf("CountRows engaged for %q, which needs its argument evaluated per row", q)
		}
	}
}

// TestCountRows_EngagesForNormalisedCountVar is the counterpart of the row rmp #2657
// moved out of the decline table: count(<bare pattern-bound var>) over an Expand now
// reaches this operator, and must still return the same integer count(*) does,
// because a non-optional Expand binds a non-null node in every row.
func TestCountRows_EngagesForNormalisedCountVar(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))
	const (
		varQ  = `MATCH (a)-[:FRIEND]->() RETURN count(a)`
		starQ = `MATCH (a)-[:FRIEND]->() RETURN count(*)`
	)
	before := countRowsBuildCount.Load()
	got := scalarOf(t, e, varQ)
	if countRowsBuildCount.Load() == before {
		t.Errorf("CountRows did not engage for %q; #2657 should have normalised it to "+
			"count(*)", varQ)
	}
	if want := scalarOf(t, e, starQ); got != want {
		t.Errorf("%q = %s but %q = %s; the normalisation changed the answer",
			varQ, got, starQ, want)
	}
	if want := "22"; got != want {
		t.Errorf("count(a) = %s, want %s", got, want)
	}
}

// TestCountRows_CountsNullBearingRows is the semantic a wrong implementation
// breaks. count(*) counts ROWS; count(v) counts non-null bindings. An OPTIONAL
// MATCH that fails to match still produces a row, with a null binding, and
// count(*) must include it while count(r) must not.
func TestCountRows_CountsNullBearingRows(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))

	star := scalarOf(t, e, `MATCH (a) OPTIONAL MATCH (a)-[r:FRIEND]->() RETURN count(*)`)
	bound := scalarOf(t, e, `MATCH (a) OPTIONAL MATCH (a)-[r:FRIEND]->() RETURN count(r)`)

	// 11 nodes with 2 edges each = 22 matched rows, plus 1 unmatched row for the
	// node with no outgoing FRIEND edge.
	if want := "23"; star != want {
		t.Errorf("count(*) over OPTIONAL MATCH = %s, want %s (the null row counts)", star, want)
	}
	if want := "22"; bound != want {
		t.Errorf("count(r) over OPTIONAL MATCH = %s, want %s (the null binding does not)", bound, want)
	}
	if star == bound {
		t.Error("count(*) and count(r) agree, so this test proves nothing about null rows")
	}
}

// TestCountRows_MatchesSerialPipeline proves the answer is identical to what the
// pre-projection + EagerAggregation pipeline computes, across shapes, by
// comparing against the row count materialised the long way.
func TestCountRows_MatchesSerialPipeline(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))
	for _, tc := range []struct{ count, rows string }{
		{`MATCH ()-[:FRIEND]->() RETURN count(*)`, `MATCH (a)-[:FRIEND]->(b) RETURN a`},
		{`MATCH (a)-[:FRIEND]->(b) WHERE a <> b RETURN count(*)`, `MATCH (a)-[:FRIEND]->(b) WHERE a <> b RETURN a`},
		{`MATCH ()-[:ABSENT]->() RETURN count(*)`, `MATCH (a)-[:ABSENT]->(b) RETURN a`},
	} {
		res, err := e.Run(context.Background(), tc.rows, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", tc.rows, err)
		}
		var n int64
		for res.Next() {
			n++
		}
		_ = res.Close()
		if got, want := scalarOf(t, e, tc.count), fmt.Sprint(n); got != want {
			t.Errorf("%q = %s, but materialising the rows gives %s", tc.count, got, want)
		}
	}
}

// TestCountRows_PlanShowsItsChild guards the defect the PlanChildren
// completeness gate exists to catch: without the method the rendered plan
// truncates at CountRows and silently hides how the rows were produced.
func TestCountRows_PlanShowsItsChild(t *testing.T) {
	e := NewEngine(buildCountRowsGraph(t))
	plan, err := e.Explain(`MATCH ()-[:FRIEND]->() RETURN count(*)`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "CountRows") {
		t.Errorf("plan does not name CountRows:\n%s", plan)
	}
	if !strings.Contains(plan, "Expand") || !strings.Contains(plan, "AllNodesScan") {
		t.Errorf("plan truncates at CountRows and hides its child:\n%s", plan)
	}
}

// TestCountRows_AliasShadowingAPatternVariable is the regression for the defect
// the first implementation shipped with. Installing the post-aggregation schema
// through the UNGUARDED installCountAggSchema tagged the output column scalar
// even when its alias shadowed a pre-aggregation variable, and the Selection
// operators below CountRows — which hold closures over bopts.scalarCols — then
// read the bound node's column as a scalar and dropped every row.
//
// The query returned 0 instead of 1, and ONLY when the alias collided: the same
// query aliased AS c was correct throughout. Both forms are asserted so a
// regression cannot hide behind the non-colliding one.
func TestCountRows_AliasShadowingAPatternVariable(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := NewEngine(g)
	ctx := context.Background()
	for _, w := range []string{
		`CREATE (:P {name: 'A'})`,
		`CREATE (:P {name: 'B'})`,
		`MATCH (a:P {name: 'A'}), (b:P {name: 'B'}) CREATE (a)-[:LIKES]->(b)`,
	} {
		if _, err := e.RunInTx(ctx, w, nil); err != nil {
			t.Fatalf("setup %q: %v", w, err)
		}
	}

	const pattern = `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS `
	shadowed := scalarOf(t, e, pattern+"n") // alias collides with the bound variable
	distinct := scalarOf(t, e, pattern+"c") // alias collides with nothing

	if want := "1"; distinct != want {
		t.Errorf("count(*) AS c = %s, want %s", distinct, want)
	}
	if shadowed != distinct {
		t.Errorf("count(*) AS n = %s but AS c = %s; the alias must not change the answer",
			shadowed, distinct)
	}
}
