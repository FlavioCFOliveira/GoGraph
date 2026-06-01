package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestLeiden_RingOfCliques builds a ring of k=8 cliques of size c=10
// connected by single inter-clique edges and asserts that Leiden
// recovers all 8 communities with NMI == 1.0.
func TestLeiden_RingOfCliques(t *testing.T) {
	t.Parallel()
	const (
		numCliques = 8
		cliqueSize = 10
		totalNodes = numCliques * cliqueSize
	)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	// Intra-clique edges: complete graph within each clique.
	for ci := 0; ci < numCliques; ci++ {
		base := ci * cliqueSize
		for i := base; i < base+cliqueSize; i++ {
			for j := i + 1; j < base+cliqueSize; j++ {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					t.Fatalf("AddEdge intra(%d,%d): %v", i, j, err)
				}
			}
		}
	}

	// Inter-clique bridge edges: last node of clique i ↔ first node of
	// clique (i+1) % numCliques, forming a ring.
	for ci := 0; ci < numCliques; ci++ {
		last := ci*cliqueSize + (cliqueSize - 1)
		first := ((ci + 1) % numCliques) * cliqueSize
		if err := a.AddEdge(last, first, struct{}{}); err != nil {
			t.Fatalf("AddEdge inter(%d,%d): %v", last, first, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())

	if p.NumCommunities != numCliques {
		t.Fatalf("Leiden RingOfCliques: got %d communities, want %d", p.NumCommunities, numCliques)
	}

	// Build ground-truth: node n belongs to clique n/cliqueSize.
	n := int(c.MaxNodeID())
	gt := make([]int, n)
	for i := range gt {
		gt[i] = -1
	}
	for node := 0; node < totalNodes; node++ {
		id, ok := a.Mapper().Lookup(node)
		if !ok {
			t.Fatalf("Lookup(%d): not found", node)
		}
		gt[id] = node / cliqueSize
	}

	nmiVal := nmi(p.Community, gt, p.NumCommunities, numCliques)
	if nmiVal < 1.0-1e-9 {
		t.Errorf("Leiden RingOfCliques: NMI = %.4f, want 1.0", nmiVal)
	}

	// Verify that all nodes within each clique share the same community.
	for ci := 0; ci < numCliques; ci++ {
		base := ci * cliqueSize
		id0, _ := a.Mapper().Lookup(base)
		for node := base + 1; node < base+cliqueSize; node++ {
			id, _ := a.Mapper().Lookup(node)
			if p.Community[id] != p.Community[id0] {
				t.Fatalf("clique %d split: c(%d)=%d c(%d)=%d",
					ci, base, p.Community[id0], node, p.Community[id])
			}
		}
	}

	// Determinism.
	p2 := Leiden(c, DefaultLeidenOptions())
	for i, v := range p.Community {
		if p2.Community[i] != v {
			t.Fatalf("non-deterministic: Community[%d] = %d vs %d on second run", i, v, p2.Community[i])
		}
	}
}
