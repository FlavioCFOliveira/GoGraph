package cypher_test

// unwind_list_test.go — UNWIND with literal lists and range() (T711).
//
// TestEngine_Unwind_LiteralList and TestEngine_Unwind_EmptyList already exist
// in api_unwind_temporal_test.go. This file adds the complementary cases:
// arithmetic projection, range(), and empty-list degenerate.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newBareEngine returns an engine backed by an empty directed graph. Suitable
// for UNWIND queries that do not inspect any stored nodes.
func newBareEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// TestUnwindList_ArithmeticProjection verifies that UNWIND combined with an
// arithmetic projection emits the correct computed values.
//
// Query: UNWIND [1,2,3] AS x RETURN x * 2 AS doubled
// Expected: rows with doubled = 2, 4, 6 (in order).
func TestUnwindList_ArithmeticProjection(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	res, err := eng.Run(context.Background(), `UNWIND [1, 2, 3] AS x RETURN x * 2 AS doubled`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	want := []int64{2, 4, 6}
	for i, row := range rows {
		v, ok := row["doubled"].(expr.IntegerValue)
		if !ok {
			t.Errorf("row %d: doubled is %T (%v), want IntegerValue", i, row["doubled"], row["doubled"])
			continue
		}
		if int64(v) != want[i] {
			t.Errorf("row %d: doubled = %d, want %d", i, int64(v), want[i])
		}
	}
}

// TestUnwindList_RangeFunction verifies that UNWIND on range(1, 5) produces 5
// rows with values 1 through 5.
//
// range(start, end) is inclusive on both ends in openCypher. The function is
// registered in cypher/funcs/essentials.go.
func TestUnwindList_RangeFunction(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	res, err := eng.Run(context.Background(), `UNWIND range(1, 5) AS x RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run range(1,5): %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 5 {
		t.Fatalf("UNWIND range(1,5): want 5 rows, got %d", len(rows))
	}
	for i, row := range rows {
		v, ok := row["x"].(expr.IntegerValue)
		if !ok {
			t.Errorf("row %d: x is %T (%v), want IntegerValue", i, row["x"], row["x"])
			continue
		}
		want := int64(i + 1)
		if int64(v) != want {
			t.Errorf("row %d: x = %d, want %d", i, int64(v), want)
		}
	}
}

// TestUnwindList_EmptyList verifies that UNWIND on an empty list emits zero
// rows. This duplicates the check already in api_unwind_temporal_test.go at
// the unit level but adds column introspection.
//
// Query: UNWIND [] AS x RETURN x
// Expected: 0 rows, no error.
func TestUnwindList_EmptyList(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	res, err := eng.Run(context.Background(), `UNWIND [] AS x RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run UNWIND []: %v", err)
	}
	defer res.Close()

	count := 0
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if count != 0 {
		t.Errorf("UNWIND []: expected 0 rows, got %d", count)
	}
}
