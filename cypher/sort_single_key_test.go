package cypher_test

// sort_single_key_test.go — T677
//
// Integration tests for ORDER BY on a single key (string and integer), in
// both ASC and DESC directions. The existing ordering_test.go tests sort
// wiring on node values (n); these tests verify ordering on property values
// (n.name, n.age) and assert the exact sequence of returned values.

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newNamedNodeGraph creates a graph with nodes, each carrying a name property
// from the provided slice. Node IDs are auto-generated as "ssk0", "ssk1", ….
func newNamedNodeGraph(t *testing.T, names []string) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i, name := range names {
		q := fmt.Sprintf(`CREATE (n {name: '%s'})`, name)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newNamedNodeGraph CREATE i=%d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newNamedNodeGraph drain i=%d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newNamedNodeGraph close i=%d: %v", i, err)
		}
	}
	return g
}

// newAgedNodeGraph creates a graph with nodes carrying an age integer property.
// ages[i] is the age of the i-th node; names are auto-generated.
func newAgedNodeGraph(t *testing.T, ages []int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i, age := range ages {
		q := fmt.Sprintf(`CREATE (n {name: 'p%02d', age: %d})`, i, age)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newAgedNodeGraph CREATE i=%d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newAgedNodeGraph drain i=%d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newAgedNodeGraph close i=%d: %v", i, err)
		}
	}
	return g
}

// collectStringCol drains res and returns n.name values as a slice in
// iteration order.
func collectStringCol(t *testing.T, res *cypher.Result, col string) []string {
	t.Helper()
	defer res.Close()
	var out []string
	for res.Next() {
		rec := res.Record()
		sv, ok := rec[col].(expr.StringValue)
		if !ok {
			t.Errorf("%s: expected StringValue, got %T (%v)", col, rec[col], rec[col])
			continue
		}
		out = append(out, string(sv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("collectStringCol: %v", err)
	}
	return out
}

// collectIntCol drains res and returns integer column values in iteration order.
func collectIntCol(t *testing.T, res *cypher.Result, col string) []int64 {
	t.Helper()
	defer res.Close()
	var out []int64
	for res.Next() {
		rec := res.Record()
		iv, ok := rec[col].(expr.IntegerValue)
		if !ok {
			t.Errorf("%s: expected IntegerValue, got %T (%v)", col, rec[col], rec[col])
			continue
		}
		out = append(out, int64(iv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("collectIntCol: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. String property ORDER BY ASC
// ─────────────────────────────────────────────────────────────────────────────

// TestSortSingleKey_StringASC verifies that ORDER BY n.name ASC produces
// alphabetical order.
func TestSortSingleKey_StringASC(t *testing.T) {
	g := newNamedNodeGraph(t, []string{"Charlie", "Alice", "Eve", "Bob", "Diana"})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name ORDER BY n.name ASC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collectStringCol(t, res, "n.name")

	want := []string{"Alice", "Bob", "Charlie", "Diana", "Eve"}
	if len(got) != len(want) {
		t.Fatalf("row count %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. String property ORDER BY DESC
// ─────────────────────────────────────────────────────────────────────────────

// TestSortSingleKey_StringDESC verifies that ORDER BY n.name DESC produces
// reverse alphabetical order.
func TestSortSingleKey_StringDESC(t *testing.T) {
	g := newNamedNodeGraph(t, []string{"Charlie", "Alice", "Eve", "Bob", "Diana"})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name ORDER BY n.name DESC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collectStringCol(t, res, "n.name")

	want := []string{"Eve", "Diana", "Charlie", "Bob", "Alice"}
	if len(got) != len(want) {
		t.Fatalf("row count %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Integer property ORDER BY ASC
// ─────────────────────────────────────────────────────────────────────────────

// TestSortSingleKey_IntASC verifies that ORDER BY n.age ASC produces numeric
// ascending order.
func TestSortSingleKey_IntASC(t *testing.T) {
	g := newAgedNodeGraph(t, []int{30, 10, 50, 20, 40})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.age ORDER BY n.age ASC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collectIntCol(t, res, "n.age")

	want := []int64{10, 20, 30, 40, 50}
	if len(got) != len(want) {
		t.Fatalf("row count %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row[%d] = %d, want %d (full: %v)", i, got[i], w, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Integer property ORDER BY DESC
// ─────────────────────────────────────────────────────────────────────────────

// TestSortSingleKey_IntDESC verifies that ORDER BY n.age DESC produces numeric
// descending order.
func TestSortSingleKey_IntDESC(t *testing.T) {
	g := newAgedNodeGraph(t, []int{30, 10, 50, 20, 40})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.age ORDER BY n.age DESC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collectIntCol(t, res, "n.age")

	want := []int64{50, 40, 30, 20, 10}
	if len(got) != len(want) {
		t.Fatalf("row count %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row[%d] = %d, want %d (full: %v)", i, got[i], w, got)
		}
	}
}
