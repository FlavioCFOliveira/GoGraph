package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestLabelPropagation_Bridge runs label propagation on two K30
// cliques connected by a single bridge and asserts that the result
// contains exactly 2 communities with NMI >= 0.95 against the
// two-clique ground truth.
//
// Label propagation is deterministic in this implementation (tie-break
// by smaller label, fixed iteration order). A single run suffices.
func TestLabelPropagation_Bridge(t *testing.T) {
	t.Parallel()
	const k = 30

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Clique A: nodes 0 .. k-1.
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge A(%d,%d): %v", i, j, err)
			}
		}
	}
	// Clique B: nodes k .. 2k-1.
	for i := k; i < 2*k; i++ {
		for j := i + 1; j < 2*k; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge B(%d,%d): %v", i, j, err)
			}
		}
	}
	// Bridge.
	if err := a.AddEdge(k-1, k, struct{}{}); err != nil {
		t.Fatalf("AddEdge bridge: %v", err)
	}

	c := csr.BuildFromAdjList(a)
	p := LabelPropagation(c, DefaultLabelPropagationOptions())

	if p.NumCommunities != 2 {
		t.Fatalf("LabelPropagation Bridge: got %d communities, want 2", p.NumCommunities)
	}

	// Ground truth: clique A → 0, clique B → 1.
	n := int(c.MaxNodeID())
	gt := make([]int, n)
	for i := range gt {
		gt[i] = -1
	}
	for node := 0; node < 2*k; node++ {
		id, ok := a.Mapper().Lookup(node)
		if !ok {
			t.Fatalf("Lookup(%d): not found", node)
		}
		if node < k {
			gt[id] = 0
		} else {
			gt[id] = 1
		}
	}

	nmiVal := nmi(p.Community, gt, p.NumCommunities, 2)
	if nmiVal < 0.95 {
		t.Errorf("LabelPropagation Bridge: NMI = %.4f, want >= 0.95", nmiVal)
	}
}
