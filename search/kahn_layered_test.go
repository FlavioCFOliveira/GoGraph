package search

// Task 849: Kahn topological sort on a layered DAG.
//
// Shapegen fixture: Layered(4, 5, 80, 42) — 4 layers, 5 nodes per
// layer, 80 % inter-layer edge density, seed 42. Layer l owns node
// keys [l*5 .. l*5+4].
//
// Acceptance criteria:
//  1. TopologicalSort returns no error.
//  2. len(order) == 20 (4 layers × 5 nodes).
//  3. Every inter-layer edge u->v (u in layer l, v in layer l+1) is
//     respected: pos[u] < pos[v] in the output.
//  4. Every intra-layer node pair in adjacent layers appears in the
//     correct relative order in the output.
//  5. The output is deterministic: two independent runs on the same
//     CSR yield the same permutation.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

func TestTopologicalSort_Layered(t *testing.T) {
	t.Parallel()

	const (
		L       = 4
		w       = 5
		density = 80
		seed    = 42
	)

	g, err := shapegen.Layered(L, w, density, seed).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Layered.Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)

	order, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}

	total := L * w // 20 nodes
	if len(order) != total {
		t.Fatalf("len(order) = %d, want %d", len(order), total)
	}

	// Build position map: NodeID (as uint64) -> index in order.
	pos := make(map[uint64]int, total)
	for i, id := range order {
		pos[uint64(id)] = i
	}

	mapper := a.Mapper()

	// AC 3 & 4: every inter-layer edge u->v must satisfy pos[u] < pos[v].
	for l := 0; l < L-1; l++ {
		for uKey := l * w; uKey < (l+1)*w; uKey++ {
			uID, ok := mapper.Lookup(uKey)
			if !ok {
				t.Fatalf("key %d not in mapper", uKey)
			}
			for vKey := (l + 1) * w; vKey < (l+2)*w; vKey++ {
				vID, ok := mapper.Lookup(vKey)
				if !ok {
					t.Fatalf("key %d not in mapper", vKey)
				}
				if a.HasEdge(uKey, vKey) {
					pu, pv := pos[uint64(uID)], pos[uint64(vID)]
					if pu >= pv {
						t.Errorf("inter-layer edge key%d->key%d not respected: pos=%d >= pos=%d",
							uKey, vKey, pu, pv)
					}
				}
			}
		}
	}

	// AC 5: determinism — second call on the same CSR must yield the
	// same permutation.
	order2, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("TopologicalSort (2nd run): %v", err)
	}
	if len(order2) != len(order) {
		t.Fatalf("determinism: len %d vs %d", len(order), len(order2))
	}
	for i := range order {
		if order[i] != order2[i] {
			t.Errorf("determinism: position %d: %d vs %d", i, order[i], order2[i])
		}
	}
}
