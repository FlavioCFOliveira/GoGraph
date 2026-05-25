package cypher_test

// scan_all_classic_test.go — T589: MATCH (n) RETURN n on classic shapes
// (path P5, cycle C4, star S4, complete K3, complete bipartite K2,3).

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// runScanAllCount runs MATCH (n) RETURN n against g and returns the row count.
func runScanAllCount(t *testing.T, g *lpg.Graph[string, float64]) int {
	t.Helper()
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
	return count
}

// TestScanAll_PathP5 verifies MATCH (n) RETURN n on path P5 (5 nodes, 4 edges)
// yields 5 rows.
func TestScanAll_PathP5(t *testing.T) {
	t.Parallel()

	// P5: v0 → v1 → v2 → v3 → v4
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range 5 {
		if err := g.AddNode(fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("AddNode v%d: %v", i, err)
		}
	}
	for i := range 4 {
		if err := g.AddEdge(fmt.Sprintf("v%d", i), fmt.Sprintf("v%d", i+1), 1.0); err != nil {
			t.Fatalf("AddEdge v%d→v%d: %v", i, i+1, err)
		}
	}

	if got := runScanAllCount(t, g); got != 5 {
		t.Errorf("P5: want 5 rows, got %d", got)
	}
}

// TestScanAll_CycleC4 verifies MATCH (n) RETURN n on cycle C4 (4 nodes, 4
// directed edges forming a cycle) yields 4 rows.
func TestScanAll_CycleC4(t *testing.T) {
	t.Parallel()

	// C4: v0 → v1 → v2 → v3 → v0
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range 4 {
		if err := g.AddNode(fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("AddNode v%d: %v", i, err)
		}
	}
	for i := range 4 {
		if err := g.AddEdge(fmt.Sprintf("v%d", i), fmt.Sprintf("v%d", (i+1)%4), 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}

	if got := runScanAllCount(t, g); got != 4 {
		t.Errorf("C4: want 4 rows, got %d", got)
	}
}

// TestScanAll_StarS4 verifies MATCH (n) RETURN n on star S4 (1 center + 3
// leaves) yields 4 rows.
func TestScanAll_StarS4(t *testing.T) {
	t.Parallel()

	// S4: center → leaf0, leaf1, leaf2
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("center"); err != nil {
		t.Fatalf("AddNode center: %v", err)
	}
	for i := range 3 {
		leaf := fmt.Sprintf("leaf%d", i)
		if err := g.AddNode(leaf); err != nil {
			t.Fatalf("AddNode %s: %v", leaf, err)
		}
		if err := g.AddEdge("center", leaf, 1.0); err != nil {
			t.Fatalf("AddEdge center→%s: %v", leaf, err)
		}
	}

	if got := runScanAllCount(t, g); got != 4 {
		t.Errorf("S4: want 4 rows (center + 3 leaves), got %d", got)
	}
}

// TestScanAll_CompleteK3 verifies MATCH (n) RETURN n on directed K3 (3 nodes,
// 3 directed edges a→b→c→a) yields 3 rows.
func TestScanAll_CompleteK3(t *testing.T) {
	t.Parallel()

	// K3 directed: a → b → c → a
	g := lpg.New[string, float64](adjlist.Config{})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	edges := [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}}
	for _, e := range edges {
		if err := g.AddEdge(e[0], e[1], 1.0); err != nil {
			t.Fatalf("AddEdge %s→%s: %v", e[0], e[1], err)
		}
	}

	if got := runScanAllCount(t, g); got != 3 {
		t.Errorf("K3: want 3 rows, got %d", got)
	}
}

// TestScanAll_CompleteBipartiteK2x3 verifies MATCH (n) RETURN n on K2,3
// (2 left + 3 right nodes, 6 directed edges) yields 5 rows.
func TestScanAll_CompleteBipartiteK2x3(t *testing.T) {
	t.Parallel()

	// K2,3: l0,l1 → r0,r1,r2
	g := lpg.New[string, float64](adjlist.Config{})
	left := []string{"l0", "l1"}
	right := []string{"r0", "r1", "r2"}
	for _, n := range append(left, right...) {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	for _, l := range left {
		for _, r := range right {
			if err := g.AddEdge(l, r, 1.0); err != nil {
				t.Fatalf("AddEdge %s→%s: %v", l, r, err)
			}
		}
	}

	if got := runScanAllCount(t, g); got != 5 {
		t.Errorf("K2,3: want 5 rows (2 left + 3 right), got %d", got)
	}
}
