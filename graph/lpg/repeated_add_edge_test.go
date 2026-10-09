package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestGraph_RepeatedAddEdge_IsTwoRelationships pins the multigraph contract at
// the graph layer (rmp #3072): two AddEdge calls on the same pair create two
// relationships with distinct stable handles, each carrying its own weight.
func TestGraph_RepeatedAddEdge_IsTwoRelationships(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	if got := g.AdjList().Size(); got != 2 {
		t.Fatalf("Size = %d, want 2 relationships", got)
	}
	hs := g.AppendEdgeHandles("a", "b", nil)
	if len(hs) != 2 || hs[0] == 0 || hs[1] == 0 || hs[0] == hs[1] {
		t.Fatalf("handles = %v, want two distinct non-zero handles", hs)
	}
	var weights []int64
	for _, w := range g.AdjList().Neighbours("a") {
		weights = append(weights, w)
	}
	if len(weights) != 2 || weights[0]+weights[1] != 3 {
		t.Fatalf("weights = %v, want each relationship's own weight (1 and 2)", weights)
	}
}
