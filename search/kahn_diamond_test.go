package search

// Task 854: Kahn topological sort on a diamond DAG.
//
// Diamond: 4 vertices A=0, B=1, C=2, D=3.
// Edges: 0->1, 0->2, 1->3, 2->3.
//
// Acceptance criteria:
//  1. TopologicalSort returns no error.
//  2. len(order) == 4.
//  3. order[0] == NodeID of key 0 (source A is first).
//  4. order[3] == NodeID of key 3 (sink D is last).
//  5. {order[1], order[2]} == {NodeID of 1, NodeID of 2} (B and C
//     occupy the two middle positions in either order).
//  6. Determinism: a second run returns the same permutation.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestTopologicalSort_DiamondShape(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}} {
		if err := a.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge %d->%d: %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)

	order, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("len(order) = %d, want 4", len(order))
	}

	mapper := a.Mapper()
	id := func(key int) uint64 {
		nid, ok := mapper.Lookup(key)
		if !ok {
			t.Fatalf("key %d not in mapper", key)
		}
		return uint64(nid)
	}
	idA, idB, idC, idD := id(0), id(1), id(2), id(3)

	// AC 3: source A must appear first.
	if uint64(order[0]) != idA {
		t.Errorf("order[0] = %d, want idA=%d", order[0], idA)
	}
	// AC 4: sink D must appear last.
	if uint64(order[3]) != idD {
		t.Errorf("order[3] = %d, want idD=%d", order[3], idD)
	}
	// AC 5: B and C occupy the middle two positions (in either order).
	mid := map[uint64]bool{uint64(order[1]): true, uint64(order[2]): true}
	if !mid[idB] || !mid[idC] {
		t.Errorf("middle positions = {%d, %d}, want {idB=%d, idC=%d}",
			order[1], order[2], idB, idC)
	}

	// AC 6: determinism.
	order2, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("TopologicalSort (2nd run): %v", err)
	}
	if len(order2) != 4 {
		t.Fatalf("determinism: len %d", len(order2))
	}
	for i := range order {
		if order[i] != order2[i] {
			t.Errorf("determinism: position %d: %d vs %d", i, order[i], order2[i])
		}
	}
}
