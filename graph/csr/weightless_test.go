package csr

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestCSR_Weightless_NilWeightsForNonEmptyW is the runtime-gate test: a
// weightless adjacency whose weight type W is NOT zero-size (int64) must still
// yield a CSR with a nil weights slice. This proves the build gate is
// `hasWeights[W]() && !adj.Weightless()` (an AND of the compile-time and the
// runtime condition), not the compile-time check alone — the two can disagree
// for a deliberately weightless graph over a non-empty W.
func TestCSR_Weightless_NilWeightsForNonEmptyW(t *testing.T) {
	t.Parallel()

	// hasWeights[int64]() is true (int64 is not struct{}), so without the
	// runtime gate the CSR would allocate a zero-filled weights array.
	if !hasWeights[int64]() {
		t.Fatal("precondition: hasWeights[int64]() should be true")
	}

	weightless := adjlist.New[string, int64](adjlist.Config{Directed: true, Weightless: true})
	if err := weightless.AddEdge("a", "b", 100); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := weightless.AddEdge("a", "c", 200); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	c := BuildFromAdjList(weightless)
	if c.WeightsSlice() != nil {
		t.Fatalf("weightless CSR WeightsSlice() = %v, want nil", c.WeightsSlice())
	}
	if c.Size() != 2 {
		t.Fatalf("weightless CSR Size = %d, want 2", c.Size())
	}

	// NeighboursByID must render the zero weight for every edge, and the edge
	// set must be intact (topology is weight-independent).
	idA, _ := weightless.Mapper().Lookup("a")
	seen := 0
	for _, w := range c.NeighboursByID(idA) {
		if w != 0 {
			t.Fatalf("weightless NeighboursByID weight = %d, want 0", w)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("weightless NeighboursByID yielded %d edges, want 2", seen)
	}
}

// TestCSR_Weighted_Unchanged is the regression guard: a NON-weightless graph
// over the same W=int64 builds a CSR whose weights array is populated exactly
// as before, with the values aligned to the edges. The weightless feature must
// not perturb the weighted path.
func TestCSR_Weighted_Unchanged(t *testing.T) {
	t.Parallel()
	weighted := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := weighted.AddEdge("a", "b", 100); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := weighted.AddEdge("a", "c", 200); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	c := BuildFromAdjList(weighted)
	ws := c.WeightsSlice()
	if ws == nil || len(ws) != 2 {
		t.Fatalf("weighted CSR WeightsSlice len=%d (nil=%t), want 2 non-nil", len(ws), ws == nil)
	}

	// Collect (neighbour -> weight) and assert the weights survived intact.
	idA, _ := weighted.Mapper().Lookup("a")
	idB, _ := weighted.Mapper().Lookup("b")
	idC, _ := weighted.Mapper().Lookup("c")
	got := map[graph.NodeID]int64{}
	for n, w := range c.NeighboursByID(idA) {
		got[n] = w
	}
	if got[idB] != 100 {
		t.Fatalf("weighted: edge a->b weight = %d, want 100", got[idB])
	}
	if got[idC] != 200 {
		t.Fatalf("weighted: edge a->c weight = %d, want 200", got[idC])
	}
}
