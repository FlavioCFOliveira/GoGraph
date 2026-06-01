package cypher_test

// top_limit_sort_test.go — T688
//
// Integration tests for LIMIT applied after ORDER BY on a graph with 50 nodes.
// Verifies that the correct youngest and oldest nodes are returned.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newLargeAgedGraph creates a graph with n nodes aged 0 through n-1.
// Names are formatted as "p00", "p01", … so lexicographic order matches
// numeric order for n ≤ 100.
func newLargeAgedGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		q := fmt.Sprintf(`CREATE (x {name: 'p%02d', age: %d})`, i, i)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newLargeAgedGraph CREATE i=%d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newLargeAgedGraph drain i=%d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newLargeAgedGraph close i=%d: %v", i, err)
		}
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Top-5 youngest
// ─────────────────────────────────────────────────────────────────────────────

// TestTopLimitSort_Youngest5 verifies that ORDER BY n.age ASC LIMIT 5 on a
// 50-node graph returns exactly 5 rows with ages 0, 1, 2, 3, 4.
func TestTopLimitSort_Youngest5(t *testing.T) {
	g := newLargeAgedGraph(t, 50)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.age ORDER BY n.age ASC LIMIT 5`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var ages []int64
	for res.Next() {
		rec := res.Record()
		iv, ok := rec["n.age"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("n.age: expected IntegerValue, got %T", rec["n.age"])
		}
		ages = append(ages, int64(iv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	if len(ages) != 5 {
		t.Fatalf("expected 5 rows, got %d: %v", len(ages), ages)
	}
	for i, want := range []int64{0, 1, 2, 3, 4} {
		if ages[i] != want {
			t.Errorf("row[%d] age = %d, want %d (full: %v)", i, ages[i], want, ages)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Top-3 oldest
// ─────────────────────────────────────────────────────────────────────────────

// TestTopLimitSort_Oldest3 verifies that ORDER BY n.age DESC LIMIT 3 on the
// same 50-node graph returns exactly 3 rows with ages 49, 48, 47.
func TestTopLimitSort_Oldest3(t *testing.T) {
	g := newLargeAgedGraph(t, 50)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.age ORDER BY n.age DESC LIMIT 3`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var ages []int64
	for res.Next() {
		rec := res.Record()
		iv, ok := rec["n.age"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("n.age: expected IntegerValue, got %T", rec["n.age"])
		}
		ages = append(ages, int64(iv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	if len(ages) != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", len(ages), ages)
	}
	for i, want := range []int64{49, 48, 47} {
		if ages[i] != want {
			t.Errorf("row[%d] age = %d, want %d (full: %v)", i, ages[i], want, ages)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. LIMIT equals result size — no truncation
// ─────────────────────────────────────────────────────────────────────────────

// TestTopLimitSort_LimitEqualsSize verifies that LIMIT N on a graph with
// exactly N nodes returns all N rows.
func TestTopLimitSort_LimitEqualsSize(t *testing.T) {
	const n = 10
	g := newLargeAgedGraph(t, n)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		fmt.Sprintf(`MATCH (x) RETURN x.age ORDER BY x.age ASC LIMIT %d`, n), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	count := 0
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != n {
		t.Errorf("LIMIT %d on %d-node graph returned %d rows, want %d", n, n, count, n)
	}
}
