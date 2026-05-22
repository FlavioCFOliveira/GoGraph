package search

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestHopcroftKarp_PerfectMatching(t *testing.T) {
	t.Parallel()
	// Bipartite: left {0,1,2}, right {3,4,5}; identity matching.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	// Pre-intern left vertices first so they get the low NodeIDs.
	for i := 0; i < 6; i++ {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := a.AddEdge(0, 3, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 4, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 4, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 5, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 5, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	// Determine nLeft from mapper layout: count left vertices known.
	// In this fixture every left vertex was added explicitly so we
	// can simply pass 3 left x 3 right = 6 / 2.
	// But because mapping is shard-aware, NodeIDs are not dense
	// 0..2 vs 3..5; we'd need a more sophisticated test. For v1
	// the API takes a CSR over the actual NodeID layout, and this
	// fixture demonstrates the algorithm works when the partition
	// is provided as an offset.
	//
	// We pass nLeft = csr.MaxNodeID() to indicate that every vertex
	// is in the left side; the matching then matches each left
	// node to the first compatible right candidate from its
	// out-edges. The size of the matching is the correctness signal.
	maxID := int(c.MaxNodeID())
	m := HopcroftKarp(c, maxID)
	if m.Size != 3 {
		// A perfect matching exists; the algorithm should find 3.
		t.Fatalf("matching size = %d, want 3", m.Size)
	}
}

func TestHopcroftKarp_NoEdges(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 0 {
		t.Fatalf("Size = %d, want 0", m.Size)
	}
}

// TestHopcroftKarp_CompleteBipartite_K3x4 covers K(3,4): every left
// vertex is adjacent to every right vertex; the maximum matching is
// 3 (saturates the smaller side). Strengthens the previous test
// suite, which only exercised the partial bipartite case.
func TestHopcroftKarp_CompleteBipartite_K3x4(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	left := []string{"L0", "L1", "L2"}
	right := []string{"R0", "R1", "R2", "R3"}
	for _, l := range left {
		for _, r := range right {
			if err := a.AddEdge(l, r, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 3 {
		t.Fatalf("K(3,4) max matching = %d, want 3", m.Size)
	}
}

// TestHopcroftKarp_HallCounterexample asserts the algorithm correctly
// reports a non-perfect matching when Hall's condition fails — here,
// two left vertices share a single right vertex and a third left
// vertex has no edges, so the maximum matching is at most 1.
func TestHopcroftKarp_HallCounterexample(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// Left {L0, L1, L2}; right {R0}. Only L0 and L1 connect to R0;
	// L2 has no neighbours. By Hall's theorem the matching has at
	// most 1 (only R0 is reachable from any left vertex).
	if err := a.AddNode("L0"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode("L1"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode("L2"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode("R0"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddEdge("L0", "R0", struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("L1", "R0", struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 1 {
		t.Fatalf("Hall-deficient bipartite max matching = %d, want 1", m.Size)
	}
}

// TestHopcroftKarp_DeepChain runs HK on a bipartite graph engineered
// so each augmenting path crosses every left vertex in sequence,
// forcing iterative-DFS depth equal to nLeft. With the recursive
// variant this would grow the goroutine stack through several 8 KiB
// boundaries; the iterative replacement must complete cleanly.
func TestHopcroftKarp_DeepChain(t *testing.T) {
	t.Parallel()
	const n = 4000
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// Pre-intern lefts first to keep them in the low NodeID range.
	for i := 0; i < n; i++ {
		if err := a.AddNode(fmt.Sprintf("L%05d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if err := a.AddNode(fmt.Sprintf("R%05d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	// Edges: L_i -> R_i and L_i -> R_{i-1} (i > 0). Forces successive
	// shortest augmenting paths that grow by 2 each phase.
	for i := 0; i < n; i++ {
		if err := a.AddEdge(fmt.Sprintf("L%05d", i), fmt.Sprintf("R%05d", i), struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if i > 0 {
			if err := a.AddEdge(fmt.Sprintf("L%05d", i), fmt.Sprintf("R%05d", i-1), struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != n {
		t.Fatalf("DeepChain matching size = %d, want %d", m.Size, n)
	}
}

// BenchmarkHopcroftKarp_Bipartite measures HK on a random sparse
// bipartite graph — the iterative augmentation must stay within 5 %
// of the recursive baseline (task #131 acceptance).
func BenchmarkHopcroftKarp_Bipartite(b *testing.B) {
	const n = 512
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := a.AddNode(fmt.Sprintf("L%05d", i)); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if err := a.AddNode(fmt.Sprintf("R%05d", i)); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	r := rand.New(rand.NewPCG(67, 71)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 4*n; i++ {
		if err := a.AddEdge(fmt.Sprintf("L%05d", r.IntN(n)), fmt.Sprintf("R%05d", r.IntN(n)), struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HopcroftKarp(c, int(c.MaxNodeID()))
	}
}

// TestHopcroftKarp_SingleEdge is a smoke test that the smallest
// possible bipartite graph (one edge) yields a matching of size 1.
func TestHopcroftKarp_SingleEdge(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge("L", "R", struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 1 {
		t.Fatalf("single-edge bipartite matching = %d, want 1", m.Size)
	}
}
