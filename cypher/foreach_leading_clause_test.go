package cypher_test

// foreach_leading_clause_test.go — regression tests for the production-
// readiness audit finding [CY1] (rmp #2034).
//
// When FOREACH is the LEADING clause of a query its logical-plan Outer is nil.
// Foreach.Vars() dereferenced Outer unconditionally, so any following clause
// (CREATE, RETURN, WITH, another FOREACH) triggered a nil-pointer panic
// (recovered into an error) that wrongly rejected valid openCypher — two
// consecutive updating clauses are legal (SingleQuery = Clause+), and the
// no-recoverable-panic mandate forbids the crash. A standalone leading FOREACH
// already executed correctly; only the variable collection panicked.
//
// Each test must FAIL on the pre-fix code (panic-derived error) and PASS after
// (both the FOREACH body and the following clause run with correct results).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// singleRowAny runs a mixed write+read query via RunAny and returns its single
// result row (fatals unless exactly one row is produced).
func singleRowAny(t *testing.T, eng *cypher.Engine, q string) map[string]interface{} {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunAny(%q): %v", q, err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("RunAny(%q): got %d rows, want 1", q, len(rows))
	}
	return rows[0]
}

// TestForeach_Leading_ThenCreate: FOREACH (...) CREATE (...) — the following
// CREATE must run once after the loop body.
func TestForeach_Leading_ThenCreate(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `FOREACH (x IN [1, 2] | CREATE (:N {v: x})) CREATE (:X)`)

	n := singleRow(t, eng, `MATCH (:N) RETURN count(*) AS c`)
	if c, ok := n["c"].(expr.IntegerValue); !ok || int64(c) != 2 {
		t.Fatalf("count(:N) = %v, want 2", n["c"])
	}
	x := singleRow(t, eng, `MATCH (:X) RETURN count(*) AS c`)
	if c, ok := x["c"].(expr.IntegerValue); !ok || int64(c) != 1 {
		t.Fatalf("count(:X) = %v, want 1", x["c"])
	}
}

// TestForeach_Leading_ThenReturn: FOREACH (...) RETURN 1 — the following RETURN
// must yield exactly one row.
func TestForeach_Leading_ThenReturn(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	row := singleRowAny(t, eng, `FOREACH (x IN [1] | CREATE (:N)) RETURN 1 AS one`)
	if v, ok := row["one"].(expr.IntegerValue); !ok || int64(v) != 1 {
		t.Fatalf("one = %v, want 1", row["one"])
	}
}

// TestForeach_Leading_ThenForeach: FOREACH (...) FOREACH (...) — both loops run.
func TestForeach_Leading_ThenForeach(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `FOREACH (x IN [1] | CREATE (:N)) FOREACH (y IN [1, 2] | CREATE (:M))`)

	n := singleRow(t, eng, `MATCH (:N) RETURN count(*) AS c`)
	if c, ok := n["c"].(expr.IntegerValue); !ok || int64(c) != 1 {
		t.Fatalf("count(:N) = %v, want 1", n["c"])
	}
	m := singleRow(t, eng, `MATCH (:M) RETURN count(*) AS c`)
	if c, ok := m["c"].(expr.IntegerValue); !ok || int64(c) != 2 {
		t.Fatalf("count(:M) = %v, want 2", m["c"])
	}
}

// TestForeach_Leading_ThenWithReturn: FOREACH (...) WITH 1 AS y RETURN y.
func TestForeach_Leading_ThenWithReturn(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	row := singleRowAny(t, eng, `FOREACH (x IN [1] | CREATE (:N)) WITH 1 AS y RETURN y`)
	if v, ok := row["y"].(expr.IntegerValue); !ok || int64(v) != 1 {
		t.Fatalf("y = %v, want 1", row["y"])
	}
}
