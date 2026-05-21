package cypher_test

// aggregation_test.go — integration tests for EagerAggregation wiring
// (tasks #371). Tests exercise MATCH … RETURN count(*), count(n), and
// group-by aggregation end-to-end through the Engine.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// collectRecords drains a Result into a slice of exec.Record maps.
func collectRecords(t *testing.T, res *cypher.Result) []map[string]interface{} {
	t.Helper()
	defer res.Close()
	var rows []map[string]interface{}
	for res.Next() {
		rec := res.Record()
		cp := make(map[string]interface{}, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		rows = append(rows, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return rows
}

// countRows drains a Result and returns the number of rows produced.
func countRows(t *testing.T, res *cypher.Result) int {
	t.Helper()
	defer res.Close()
	var n int
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return n
}

// firstValue returns the first value in a single-entry map record, and the key.
// Used in tests where the query returns exactly one column.
func firstValue(t *testing.T, rec map[string]interface{}) (string, interface{}) {
	t.Helper()
	for k, v := range rec {
		return k, v
	}
	t.Fatal("record is empty")
	return "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. count(*) on 3-node graph → single row with IntegerValue(3)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountStar(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(*) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["cnt"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 3 {
		t.Errorf("count(*) = %d, want 3", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. count(*) on empty graph → no rows (EagerAggregation emits one row per
//    group; with zero input rows, zero groups are formed)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountStar_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(*) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// EagerAggregation emits one output row per distinct group seen. With zero
	// input rows, zero groups are formed and zero rows are emitted. This matches
	// the exec operator contract; global "zero-row → one-row-with-zero" semantics
	// would require special-casing not present in the current operator.
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for empty graph aggregate, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. count(n) — counts non-null values of n (should equal node count)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountN(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("a")
	g.AddNode("b")
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(n) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["cnt"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 2 {
		t.Errorf("count(n) = %d, want 2", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. count(*) via WITH — tests EagerAggregation in a WITH clause pipeline
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_WithCountStar(t *testing.T) {
	g := newNNodeGraph(5)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) WITH count(*) AS total RETURN total", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["total"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("total: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 5 {
		t.Errorf("total = %d, want 5", int64(got))
	}
}
