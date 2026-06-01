package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestLeiden_BridgeDoubleCluster builds two K30 cliques connected by a
// single bridge edge and asserts that Leiden correctly recovers the
// two-community structure with NMI == 1.0. It also verifies
// bit-for-bit determinism across two independent runs.
func TestLeiden_BridgeDoubleCluster(t *testing.T) {
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
	// Bridge: last node of clique A ↔ first node of clique B.
	if err := a.AddEdge(k-1, k, struct{}{}); err != nil {
		t.Fatalf("AddEdge bridge: %v", err)
	}

	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())

	if p.NumCommunities != 2 {
		t.Fatalf("Leiden BridgeDoubleCluster: got %d communities, want 2", p.NumCommunities)
	}

	// Build ground-truth labels: clique A → 0, clique B → 1.
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
	if nmiVal < 1.0-1e-9 {
		t.Errorf("Leiden BridgeDoubleCluster: NMI = %.4f, want 1.0", nmiVal)
	}

	// Verify all members of each clique land in the same community.
	id0, _ := a.Mapper().Lookup(0)
	idKm1, _ := a.Mapper().Lookup(k - 1)
	idK, _ := a.Mapper().Lookup(k)
	idLast, _ := a.Mapper().Lookup(2*k - 1)
	if p.Community[id0] != p.Community[idKm1] {
		t.Fatalf("clique A split: c(0)=%d c(%d)=%d", p.Community[id0], k-1, p.Community[idKm1])
	}
	if p.Community[idK] != p.Community[idLast] {
		t.Fatalf("clique B split: c(%d)=%d c(%d)=%d", k, p.Community[idK], 2*k-1, p.Community[idLast])
	}
	if p.Community[id0] == p.Community[idK] {
		t.Fatalf("Leiden merged clique A and B: c(0)=c(%d)=%d", k, p.Community[id0])
	}

	// Determinism: a second run on the same CSR must produce an
	// identical Community slice.
	p2 := Leiden(c, DefaultLeidenOptions())
	for i, v := range p.Community {
		if p2.Community[i] != v {
			t.Fatalf("non-deterministic: Community[%d] = %d vs %d on second run", i, v, p2.Community[i])
		}
	}
}
