package cypher_test

// foreach_test.go — coverage for the FOREACH updating clause (#2029).
//
// FOREACH (x IN list | <updating clauses>) binds x to each element of the list
// and runs the body clauses as side-effects, without changing the surrounding
// query's row cardinality. It supports CREATE, SET, MERGE, REMOVE, DELETE and
// nested FOREACH in the body, over a literal list, a bound list variable, or a
// list-valued expression.

import (
	"context"
	"testing"
)

// TestForeach_Create_LiteralList: FOREACH over a literal list creating nodes.
func TestForeach_Create_LiteralList(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `FOREACH (i IN [1, 2, 3] | CREATE (:N {v: i}))`)
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 3)
	assertCount(context.Background(), t, eng, `MATCH (n:N) WHERE n.v = 2 RETURN count(n) AS n`, 1)
}

// TestForeach_Set_OuterVar: FOREACH after a MATCH sets a property on the
// matched node using the loop variable.
func TestForeach_Set_OuterVar(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id: 1})`)
	drainRunInTx(t, eng, `MATCH (n:N) FOREACH (x IN ['V'] | SET n.tag = x)`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.tag AS v`); got != "V" {
		t.Fatalf("n.tag = %q, want \"V\"", got)
	}
}

// TestForeach_Merge_Idempotent: FOREACH MERGE creates each node once; re-running
// matches rather than duplicating.
func TestForeach_Merge_Idempotent(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	const q = `FOREACH (i IN [1, 2] | MERGE (:M {id: i}))`
	drainRunInTx(t, eng, q)
	drainRunInTx(t, eng, q)
	assertCount(context.Background(), t, eng, `MATCH (m:M) RETURN count(m) AS n`, 2)
}

// TestForeach_ListVariable: FOREACH over a list bound by a preceding WITH.
func TestForeach_ListVariable(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `WITH [10, 20] AS xs FOREACH (x IN xs | CREATE (:N {v: x}))`)
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 2)
	assertCount(context.Background(), t, eng, `MATCH (n:N) WHERE n.v = 20 RETURN count(n) AS n`, 1)
}

// TestForeach_CardinalityPreserved: FOREACH does not multiply the outer rows.
func TestForeach_CardinalityPreserved(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id: 1}), (:N {id: 2})`)
	// Two :N rows; the FOREACH body runs 3 times per row but the query still
	// returns exactly two rows (one per matched node).
	res, err := eng.RunInTxAny(context.Background(),
		`MATCH (n:N) FOREACH (x IN [1, 2, 3] | SET n.touched = true) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if e := res.Err(); e != nil {
		t.Fatalf("result error: %v", e)
	}
	res.Close()
	if rows != 2 {
		t.Fatalf("FOREACH changed cardinality: got %d rows, want 2", rows)
	}
}

// TestForeach_Nested: a FOREACH inside a FOREACH.
func TestForeach_Nested(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `FOREACH (i IN [1, 2] | FOREACH (j IN [1, 2, 3] | CREATE (:N {a: i, b: j})))`)
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 6)
}

// TestForeach_Delete: FOREACH DELETE removes nodes gathered into a list.
func TestForeach_Delete(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:X), (:X), (:X)`)
	drainRunInTx(t, eng, `MATCH (n:X) WITH collect(n) AS ns FOREACH (x IN ns | DELETE x)`)
	assertCount(context.Background(), t, eng, `MATCH (n:X) RETURN count(n) AS n`, 0)
}

// TestForeach_EmptyList: an empty list runs the body zero times, no error.
func TestForeach_EmptyList(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	if err := runDrainErr(t, eng, `FOREACH (x IN [] | CREATE (:N {v: x}))`); err != nil {
		t.Fatalf("empty-list FOREACH must be a no-op, not an error: %v", err)
	}
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 0)
}

// TestForeach_Remove: FOREACH REMOVE clears a property.
func TestForeach_Remove(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id: 1, tag: 'x'})`)
	drainRunInTx(t, eng, `MATCH (n:N) FOREACH (x IN [1] | REMOVE n.tag)`)
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.tag AS v`) {
		t.Fatal("n.tag should be removed by FOREACH REMOVE")
	}
}
