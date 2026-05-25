package cypher_test

// distinct_test.go — T694
//
// Integration tests for DISTINCT on duplicate property values.
//
// The existing ordering_test.go (TestDistinct_Basic) tests DISTINCT on node
// values (RETURN DISTINCT n) where all nodes are unique by identity. These
// tests differ: they apply DISTINCT to a property that has duplicate values
// across multiple nodes and verify the deduplicated set.
//
// Graph: 3 nodes with color="red", 2 with color="blue", 1 with color="green".

import (
	"context"
	"sort"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newColorGraph creates a graph with duplicate color property values:
// 3 × "red", 2 × "blue", 1 × "green".
func newColorGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	colors := []string{"red", "red", "red", "blue", "blue", "green"}
	for i, c := range colors {
		res, err := eng.RunInTxAny(ctx,
			"CREATE (n {color: '"+c+"'})", nil)
		if err != nil {
			t.Fatalf("newColorGraph CREATE i=%d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newColorGraph drain i=%d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newColorGraph close i=%d: %v", i, err)
		}
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. DISTINCT on a property with duplicates — unordered
// ─────────────────────────────────────────────────────────────────────────────

// TestDistinct_PropertyDedup verifies that RETURN DISTINCT n.color on a graph
// with repeated color values produces exactly 3 distinct strings — one per
// distinct color — regardless of result order.
func TestDistinct_PropertyDedup(t *testing.T) {
	g := newColorGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN DISTINCT n.color`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 distinct colors, got %d: %v", len(rows), rows)
	}

	seen := map[string]bool{}
	for _, row := range rows {
		sv, ok := row["n.color"].(expr.StringValue)
		if !ok {
			t.Errorf("n.color: expected StringValue, got %T (%v)", row["n.color"], row["n.color"])
			continue
		}
		seen[string(sv)] = true
	}
	for _, want := range []string{"red", "blue", "green"} {
		if !seen[want] {
			t.Errorf("color %q missing from DISTINCT result; got %v", want, seen)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. DISTINCT + ORDER BY — deterministic alphabetical order
// ─────────────────────────────────────────────────────────────────────────────

// TestDistinct_PropertyOrdered verifies that RETURN DISTINCT n.color ORDER BY
// n.color ASC produces the three distinct colors in alphabetical order.
func TestDistinct_PropertyOrdered(t *testing.T) {
	g := newColorGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN DISTINCT n.color ORDER BY n.color ASC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectStringCol(t, res, "n.color")

	want := []string{"blue", "green", "red"}
	if len(got) != len(want) {
		t.Fatalf("expected %d rows, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. DISTINCT on mixed duplicates — result is a proper subset
// ─────────────────────────────────────────────────────────────────────────────

// TestDistinct_RowCountReduction verifies that DISTINCT reduces 6 input rows
// to 3 output rows — i.e., the result cardinality is strictly less than the
// input cardinality when duplicates exist.
func TestDistinct_RowCountReduction(t *testing.T) {
	g := newColorGraph(t)
	eng := cypher.NewEngine(g)

	// Without DISTINCT: 6 rows.
	allRes, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.color`, nil)
	if err != nil {
		t.Fatalf("Run (all): %v", err)
	}
	allRows := collectRecords(t, allRes)
	if len(allRows) != 6 {
		t.Fatalf("expected 6 total rows, got %d", len(allRows))
	}

	// With DISTINCT: 3 rows.
	distRes, err := eng.Run(context.Background(),
		`MATCH (n) RETURN DISTINCT n.color`, nil)
	if err != nil {
		t.Fatalf("Run (distinct): %v", err)
	}
	distRows := collectRecords(t, distRes)
	if len(distRows) != 3 {
		t.Fatalf("expected 3 distinct rows, got %d: %v", len(distRows), distRows)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. DISTINCT when all values are unique — same count as input
// ─────────────────────────────────────────────────────────────────────────────

// TestDistinct_NoEffect verifies that RETURN DISTINCT n.name on a graph where
// every name is unique returns the same row count as without DISTINCT.
func TestDistinct_NoEffect(t *testing.T) {
	// Use a fresh graph with 4 nodes carrying distinct names.
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		res, err := eng.RunInTxAny(ctx,
			"CREATE (n {name: '"+name+"'})", nil)
		if err != nil {
			t.Fatalf("CREATE %s: %v", name, err)
		}
		for res.Next() {
		}
		res.Close() //nolint:errcheck // test teardown
	}

	res, err := eng.Run(ctx,
		`MATCH (n) RETURN DISTINCT n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (all unique), got %d: %v", len(rows), rows)
	}

	// Verify all four names are present.
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if sv, ok := row["n.name"].(expr.StringValue); ok {
			names = append(names, string(sv))
		}
	}
	sort.Strings(names)
	want := []string{"alpha", "beta", "delta", "gamma"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("names[%d] = %q, want %q (full: %v)", i, names[i], w, names)
		}
	}
}
