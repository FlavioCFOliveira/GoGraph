package cypher_test

// ordering_test.go — integration tests for Sort, Top, Limit, Skip, and Distinct
// wiring in buildOperator (tasks #372).
//
// All tests run end-to-end through Engine.Run so they exercise the full
// translate→build→execute pipeline.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newNNodeGraph creates a graph with n nodes, each with a distinct string label.
// Using a constant label (e.g. "n") for all calls would deduplicate to one node
// because lpg.Graph[string,float64] treats the label as the node key.
func newNNodeGraph(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		if err := g.AddNode(fmt.Sprintf("node%d", i)); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
	}
	return g
}

// runQueryCount executes query on g and returns the number of rows produced.
func runQueryCount(t *testing.T, g *lpg.Graph[string, float64], query string) int {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	return countRows(t, res)
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. LIMIT — basic row-count truncation
// ─────────────────────────────────────────────────────────────────────────────

func TestLimit_Basic(t *testing.T) {
	g := newNNodeGraph(t, 5)

	got := runQueryCount(t, g, "MATCH (n) RETURN n LIMIT 2")
	if got != 2 {
		t.Errorf("LIMIT 2 returned %d rows, want 2", got)
	}
}

func TestLimit_Zero(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got := runQueryCount(t, g, "MATCH (n) RETURN n LIMIT 0")
	if got != 0 {
		t.Errorf("LIMIT 0 returned %d rows, want 0", got)
	}
}

func TestLimit_LargerThanResult(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got := runQueryCount(t, g, "MATCH (n) RETURN n LIMIT 100")
	if got != 2 {
		t.Errorf("LIMIT 100 on 2-node graph returned %d rows, want 2", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. SKIP — row offset
// ─────────────────────────────────────────────────────────────────────────────

func TestSkip_Basic(t *testing.T) {
	g := newNNodeGraph(t, 5)

	got := runQueryCount(t, g, "MATCH (n) RETURN n SKIP 3")
	if got != 2 {
		t.Errorf("SKIP 3 on 5-node graph returned %d rows, want 2", got)
	}
}

func TestSkip_AllRows(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got := runQueryCount(t, g, "MATCH (n) RETURN n SKIP 10")
	if got != 0 {
		t.Errorf("SKIP 10 on 2-node graph returned %d rows, want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. SKIP + LIMIT combination
// ─────────────────────────────────────────────────────────────────────────────

func TestSkipLimit_Combined(t *testing.T) {
	g := newNNodeGraph(t, 10)

	got := runQueryCount(t, g, "MATCH (n) RETURN n SKIP 3 LIMIT 4")
	if got != 4 {
		t.Errorf("SKIP 3 LIMIT 4 on 10-node graph returned %d rows, want 4", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. DISTINCT — deduplication
// ─────────────────────────────────────────────────────────────────────────────

func TestDistinct_Basic(t *testing.T) {
	// Build a graph whose node IDs are distinct (DISTINCT on node values should
	// preserve all rows, since node IDs are unique per-graph).
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got := runQueryCount(t, g, "MATCH (n) RETURN DISTINCT n")
	if got != 3 {
		t.Errorf("DISTINCT on 3-node graph returned %d rows, want 3", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. ORDER BY — Sort wiring produces expected row count (sort correctness is
//    tested at the exec layer; here we verify the IR→exec wiring doesn't error)
// ─────────────────────────────────────────────────────────────────────────────

func TestSort_OrderBy(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got := runQueryCount(t, g, "MATCH (n) RETURN n ORDER BY n")
	if got != 3 {
		t.Errorf("ORDER BY returned %d rows, want 3", got)
	}
}

func TestSort_OrderByDesc(t *testing.T) {
	g := newNNodeGraph(t, 4)

	got := runQueryCount(t, g, "MATCH (n) RETURN n ORDER BY n DESC")
	if got != 4 {
		t.Errorf("ORDER BY DESC returned %d rows, want 4", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. ORDER BY + LIMIT (Top operator)
// ─────────────────────────────────────────────────────────────────────────────

func TestTop_OrderByLimit(t *testing.T) {
	g := newNNodeGraph(t, 10)

	got := runQueryCount(t, g, "MATCH (n) RETURN n ORDER BY n LIMIT 3")
	if got != 3 {
		t.Errorf("ORDER BY … LIMIT 3 on 10-node graph returned %d rows, want 3", got)
	}
}
