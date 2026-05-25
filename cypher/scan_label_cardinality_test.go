package cypher_test

// scan_label_cardinality_test.go — T597: MATCH (n:Label) RETURN count(*) on
// label-cardinality extremes: 200-node set and a single-node set.

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// buildSingleLabelGraph creates a graph with n nodes all labeled label.
func buildSingleLabelGraph(t *testing.T, n int, label string) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range n {
		key := fmt.Sprintf("node%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
	}
	return g
}

// countStarLabel runs MATCH (n:label) RETURN count(*) and returns the integer
// result.
func countStarLabel(t *testing.T, g *lpg.Graph[string, float64], label string) int64 {
	t.Helper()
	eng := cypher.NewEngine(g)
	q := fmt.Sprintf("MATCH (n:%s) RETURN count(*) AS cnt", label)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	if !res.Next() {
		if err := res.Err(); err != nil {
			t.Fatalf("Err before first row: %v", err)
		}
		t.Fatal("expected one row from count(*), got none")
	}
	rec := res.Record()
	raw := rec["cnt"]
	iv, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("count(*): expected IntegerValue, got %T (%v)", raw, raw)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	return int64(iv)
}

// TestScanLabel_LargeCardinality verifies that MATCH (n:LargeSet) RETURN
// count(*) correctly returns 200 when 200 nodes carry that label.
func TestScanLabel_LargeCardinality(t *testing.T) {
	t.Parallel()

	g := buildSingleLabelGraph(t, 200, "LargeSet")
	if got := countStarLabel(t, g, "LargeSet"); got != 200 {
		t.Errorf("LargeSet count: want 200, got %d", got)
	}
}

// TestScanLabel_RareSingleton verifies that MATCH (n:Rare) RETURN count(*)
// returns 1 when exactly one node carries that label.
func TestScanLabel_RareSingleton(t *testing.T) {
	t.Parallel()

	// Graph has 200 LargeSet nodes plus exactly one Rare node.
	g := buildSingleLabelGraph(t, 200, "LargeSet")
	if err := g.AddNode("rare0"); err != nil {
		t.Fatalf("AddNode rare0: %v", err)
	}
	if err := g.SetNodeLabel("rare0", "Rare"); err != nil {
		t.Fatalf("SetNodeLabel rare0: %v", err)
	}

	if got := countStarLabel(t, g, "Rare"); got != 1 {
		t.Errorf("Rare count: want 1, got %d", got)
	}
}
