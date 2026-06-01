package cypher_test

// scan_all_trivial_test.go — T584: MATCH (n) RETURN n on trivial shapes
// (empty graph, single node, single edge).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestScanAll_EmptyGraph verifies that MATCH (n) RETURN n on an empty graph
// yields zero rows.
func TestScanAll_EmptyGraph(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 0 {
		t.Errorf("empty graph: want 0 rows, got %d", count)
	}
}

// TestScanAll_SingleNode verifies that MATCH (n) RETURN n on a graph with one
// node yields exactly one row.
func TestScanAll_SingleNode(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("v0"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 1 {
		t.Errorf("single-node graph: want 1 row, got %d", count)
	}
}

// TestScanAll_SingleEdge verifies that MATCH (n) RETURN n on a graph with two
// nodes and one edge yields exactly two rows (one per node).
func TestScanAll_SingleEdge(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}
	if err := g.AddEdge("u", "v", 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 2 {
		t.Errorf("single-edge graph: want 2 rows (one per node), got %d", count)
	}
}
