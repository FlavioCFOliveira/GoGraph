package cypher_test

// subquery_eval_test.go — integration tests for EXISTS { … } and COUNT { … }
// subquery expressions wired through the engine (task-396).
//
// The TCK-driven runner covers ExistentialSubquery{1,2,3} feature files at
// system level; these tests assert the per-engine expression-level behaviour
// in isolation:
//   - EXISTS on empty match returns false;
//   - EXISTS on non-empty match returns true;
//   - COUNT on empty match returns 0;
//   - COUNT on non-empty match returns the exact row count;
//   - subqueries embedded in larger expressions (AND, OR, comparisons)
//     evaluate correctly.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runSetup executes the CREATE setup and drains the result. Test helper.
func runSetup(t *testing.T, eng *cypher.Engine, setup string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), setup, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("setup iter: %v", err)
	}
	_ = res.Close()
}

// drainAll runs query and returns the full record list. Test helper.
func drainAll(t *testing.T, eng *cypher.Engine, query string) []map[string]any {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer func() { _ = res.Close() }()
	var rows []map[string]any
	for res.Next() {
		r := res.Record()
		// Copy the record into a stable map so subsequent Next calls do not
		// overwrite the values.
		cp := make(map[string]any, len(r))
		for k, v := range r {
			cp[k] = v
		}
		rows = append(rows, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	return rows
}

// TestExistsSubquery_True asserts EXISTS { (n)-->() } returns true for at
// least one outer node when the graph has an outgoing edge.
func TestExistsSubquery_True(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:A)-[:R]->(:B)`)

	rows := drainAll(t, eng, `MATCH (n) RETURN EXISTS { (n)-->() } AS has`)

	// We expect 2 rows (one per node): the :A node has an outgoing edge
	// (true) and the :B node does not (false).
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (%v)", len(rows), rows)
	}
	trueCount, falseCount := 0, 0
	for _, r := range rows {
		v, ok := r["has"]
		if !ok {
			t.Fatalf("row missing 'has': %v", r)
		}
		b, ok := v.(expr.BoolValue)
		if !ok {
			t.Fatalf("'has' is not BoolValue: %T = %v", v, v)
		}
		if bool(b) {
			trueCount++
		} else {
			falseCount++
		}
	}
	if trueCount != 1 || falseCount != 1 {
		t.Errorf("expected exactly 1 true and 1 false, got true=%d false=%d", trueCount, falseCount)
	}
}

// TestCountSubquery_NonEmpty asserts COUNT { (n)-->() } returns the exact
// number of outgoing edges for each outer node. Uses single-edge setup to
// avoid the multi-pattern CREATE bug that affects TCK ExistentialSubquery
// scenarios (tracked separately; not in scope for task-396).
func TestCountSubquery_NonEmpty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	// Single CREATE clause per edge avoids multi-pattern CREATE bug.
	runSetup(t, eng, `CREATE (:A)-[:R]->(:B)`)

	rows := drainAll(t, eng, `MATCH (n) RETURN COUNT { (n)-->() } AS c`)

	// 2 nodes: :A (1 outgoing), :B (0 outgoing).
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (%v)", len(rows), rows)
	}
	var oneCount, zeroCount int
	for _, r := range rows {
		v, ok := r["c"]
		if !ok {
			t.Fatalf("row missing 'c': %v", r)
		}
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			t.Fatalf("'c' is not IntegerValue: %T = %v", v, v)
		}
		switch int64(iv) {
		case 0:
			zeroCount++
		case 1:
			oneCount++
		default:
			t.Errorf("unexpected count %d (row %v)", int64(iv), r)
		}
	}
	if oneCount != 1 || zeroCount != 1 {
		t.Errorf("expected exactly one node with count=1 and one with count=0, got one=%d zero=%d", oneCount, zeroCount)
	}
}

// TestCountSubquery_Zero asserts COUNT { } returns 0 when the inner plan
// produces no rows for any outer row.
func TestCountSubquery_Zero(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Alone)`)

	rows := drainAll(t, eng, `MATCH (n) RETURN COUNT { (n)-->() } AS c`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	v := rows[0]["c"]
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("'c' is not IntegerValue: %T", v)
	}
	if int64(iv) != 0 {
		t.Errorf("expected COUNT=0 for isolated node, got %d", int64(iv))
	}
}

// TestExistsSubquery_InAndPredicate asserts EXISTS works when nested inside a
// larger boolean expression (AND).
func TestExistsSubquery_InAndPredicate(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:A {tag: 1})-[:R]->(:B {tag: 2})`)

	// Predicate: tag = 1 AND EXISTS { (n)-->() }
	// Only :A satisfies both; :B has tag=2 and no outgoing edges.
	rows := drainAll(t, eng, `MATCH (n) WHERE n.tag = 1 AND EXISTS { (n)-->() } RETURN n.tag AS t`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
	v := rows[0]["t"]
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("'t' is not IntegerValue: %T", v)
	}
	if int64(iv) != 1 {
		t.Errorf("expected tag=1, got %d", int64(iv))
	}
}

// TestCountSubquery_InComparison asserts COUNT works inside a comparison
// expression (e.g. COUNT { … } > 0 used as a WHERE predicate).
func TestCountSubquery_InComparison(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:A)-[:R]->(:B)`)

	// COUNT { (n)-->() } > 0 is true for :A only.
	rows := drainAll(t, eng, `MATCH (n) WHERE COUNT { (n)-->() } > 0 RETURN n`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
}

// TestExistsSubquery_NotExistsInOr asserts NOT EXISTS used inside OR is
// evaluated through the expression path (not the SemiApply short-circuit).
func TestExistsSubquery_NotExistsInOr(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:A {tag: 1})-[:R]->(:B {tag: 2})`)

	// Predicate: tag = 2 OR NOT EXISTS { (n)-->() }
	// :A: tag=1, has outgoing → false OR false = false → excluded.
	// :B: tag=2 → true OR ... = true → included.
	rows := drainAll(t, eng, `MATCH (n) WHERE n.tag = 2 OR NOT EXISTS { (n)-->() } RETURN n.tag AS t`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
	v := rows[0]["t"]
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("'t' is not IntegerValue: %T", v)
	}
	if int64(iv) != 2 {
		t.Errorf("expected tag=2, got %d", int64(iv))
	}
}

// TestExistsSubquery_TopLevelWhere keeps the original SemiApply optimisation
// path covered: EXISTS as the sole WHERE predicate must still produce the
// correct result.
func TestExistsSubquery_TopLevelWhere(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:A)-[:R]->(:B)`)

	rows := drainAll(t, eng, `MATCH (n) WHERE EXISTS { (n)-->() } RETURN n`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
}
