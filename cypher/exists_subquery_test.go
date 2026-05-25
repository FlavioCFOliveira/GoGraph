package cypher_test

// exists_subquery_test.go — additive EXISTS { } subquery tests (T881).
//
// Sprint 69 / T723 covers the basic SemiApply pattern (semi_apply_exists_test.go)
// and expression-level EXISTS / COUNT wiring (subquery_eval_test.go). This file
// adds three scenarios that were not covered there:
//
//  1. EXISTS with a label-constrained inner pattern: EXISTS { (a)-[:R]->(b:Label) }
//  2. EXISTS with an inner WHERE predicate via the full-subquery form:
//     EXISTS { MATCH (a)-[r]->(b) WHERE b.prop > threshold RETURN b }
//  3. EXISTS as a RETURN expression (Boolean column alongside other projections).
//
// Known engine limitations:
//
//   - Correlated subqueries where the outer variable appears on the right-hand
//     side of the inner pattern — e.g. EXISTS { (src)-[]->(n) } when n is outer
//     — do not propagate the outer binding and always return false.  Tests use
//     the outer variable on the left-hand side only.
//
//   - The pattern-form WHERE clause — EXISTS { (a)-[:R]->(b) WHERE b.prop > x }
//     — does not apply the WHERE predicate to b's properties; it only evaluates
//     the structural pattern.  Property predicates in the inner subquery must be
//     expressed via the full-subquery form:
//     EXISTS { MATCH (a)-[:R]->(b) WHERE b.prop > x RETURN b }.
//
// Supported forms (verified):
//   - Pattern only:      EXISTS { (a)-[:R]->(b) }
//   - Pattern + label:   EXISTS { (a)-[:R]->(b:Label) }
//   - Full subquery:     EXISTS { MATCH (a)-[:R]->(b) RETURN b }
//   - Full + WHERE:      EXISTS { MATCH (a)-[:R]->(b) WHERE b.x > 0 RETURN b }
//   - RETURN expression: RETURN EXISTS { (n)-[]->(m) } AS hasOut

import (
	"context"
	"slices"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newExistsGraph returns an engine backed by:
//
//	alice  ──KNOWS──►  bob   (bob.age = 20)
//	alice  ──LIKES──►  dave  (dave.age = 35)
//	charlie             (isolated)
//
// All nodes carry a "name" property and an "age" property.
//
// The first CREATE uses an inline pattern to create alice→bob in one statement
// (multi-MATCH CREATE is not supported). Subsequent edges are added by matching
// alice and attaching new nodes via a separate CREATE.
func newExistsGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Person {name: 'alice', age: 30})-[:KNOWS]->(:Person {name: 'bob', age: 20})`)
	runSetup(t, eng, `MATCH (a:Person {name: 'alice'}) CREATE (a)-[:LIKES]->(:Person {name: 'dave', age: 35})`)
	runSetup(t, eng, `CREATE (:Person {name: 'charlie', age: 40})`)
	return eng
}

// collectColumn drains a Result and returns the string values of the named
// column as a sorted slice for deterministic comparison.
func collectColumn(t *testing.T, eng *cypher.Engine, query, col string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", query, err)
	}
	rows := collectRecords(t, res)
	vals := make([]string, 0, len(rows))
	for _, row := range rows {
		if sv, ok := row[col].(expr.StringValue); ok {
			vals = append(vals, string(sv))
		} else {
			t.Errorf("column %q: expected StringValue, got %T (%v)", col, row[col], row[col])
		}
	}
	slices.Sort(vals)
	return vals
}

// TestExists_ComplexInnerPattern verifies EXISTS on a label-constrained inner
// pattern: WHERE EXISTS { (a)-[:KNOWS]->(b:Person) }.
//
// Only alice has a KNOWS edge to a :Person node; bob, dave, and charlie have no
// outgoing KNOWS edges. Expected result: [alice].
func TestExists_ComplexInnerPattern(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (a:Person) WHERE EXISTS { (a)-[:KNOWS]->(b:Person) } RETURN a.name AS name`
	got := collectColumn(t, eng, q, "name")
	want := []string{"alice"}
	if !slices.Equal(got, want) {
		t.Errorf("EXISTS complex inner pattern: got %v, want %v", got, want)
	}
}

// TestExists_WithInnerWhere verifies EXISTS with a property predicate in the
// inner WHERE clause. The full-subquery form is required because the
// pattern-form WHERE does not evaluate property predicates (see file comment).
//
// Query: EXISTS { MATCH (a)-[:LIKES]->(b:Person) WHERE b.age > 30 RETURN b }
// alice LIKES dave (age=35 > 30) — alice is included.
// alice KNOWS bob  (age=20, but KNOWS is not LIKES) — irrelevant to this test.
// bob, dave, charlie have no outgoing LIKES edges.
// Expected: [alice].
func TestExists_WithInnerWhere(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:LIKES]->(b:Person) WHERE b.age > 30 RETURN b } RETURN a.name AS name`
	got := collectColumn(t, eng, q, "name")
	want := []string{"alice"}
	if !slices.Equal(got, want) {
		t.Errorf("EXISTS with inner WHERE: got %v, want %v", got, want)
	}
}

// TestExists_InnerWhere_NoMatch verifies that EXISTS with an inner WHERE that
// no path satisfies returns an empty result set.
//
// Query: EXISTS { MATCH (a)-[:KNOWS]->(b:Person) WHERE b.age > 25 RETURN b }
// alice KNOWS bob (age=20), which does not satisfy age > 25. No other node
// has a KNOWS edge. The full-subquery form is used to ensure the WHERE is
// evaluated (see file comment on pattern-form limitation).
// Expected: [] (no rows).
func TestExists_InnerWhere_NoMatch(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b:Person) WHERE b.age > 25 RETURN b } RETURN a.name AS name`
	got := collectColumn(t, eng, q, "name")
	if len(got) != 0 {
		t.Errorf("EXISTS inner WHERE no-match: got %v, want []", got)
	}
}

// TestExists_ReturnExpression verifies that EXISTS { (n)-[]->(m) } can appear
// as a RETURN projection, producing a Boolean column alongside other columns.
//
// Graph has alice (has outgoing edges), bob (no outgoing), dave (no outgoing),
// charlie (no outgoing). Expected: exactly one row with hasOut=true (alice) and
// three rows with hasOut=false.
func TestExists_ReturnExpression(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (n:Person) RETURN n.name AS name, EXISTS { (n)-[]->(m) } AS hasOut`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 4 {
		t.Fatalf("RETURN EXISTS: got %d rows, want 4", len(rows))
	}

	trueCount, falseCount := 0, 0
	for _, row := range rows {
		b, ok := row["hasOut"].(expr.BoolValue)
		if !ok {
			t.Errorf("hasOut column: expected BoolValue, got %T (%v)", row["hasOut"], row["hasOut"])
			continue
		}
		if bool(b) {
			trueCount++
		} else {
			falseCount++
		}
	}
	if trueCount != 1 {
		t.Errorf("RETURN EXISTS: expected 1 true row, got %d", trueCount)
	}
	if falseCount != 3 {
		t.Errorf("RETURN EXISTS: expected 3 false rows, got %d", falseCount)
	}
}

// TestExists_OnEmptyGraph verifies that EXISTS on an empty graph produces zero
// rows regardless of the subquery form used.
func TestExists_OnEmptyGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	queries := []string{
		`MATCH (n) WHERE EXISTS { (n)-[]->(m) } RETURN n`,
		`MATCH (n) WHERE EXISTS { MATCH (n)-[]->(m) RETURN m } RETURN n`,
		`MATCH (n) RETURN EXISTS { (n)-[]->(m) } AS hasOut`,
	}
	for _, q := range queries {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", q, err)
		}
		rows := collectRecords(t, res)
		if len(rows) != 0 {
			t.Errorf("EXISTS on empty graph %q: got %d rows, want 0", q, len(rows))
		}
	}
}
