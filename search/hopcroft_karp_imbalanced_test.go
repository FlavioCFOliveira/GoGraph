package search

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/flow"
)

// buildBipartiteCSR constructs a directed CSR for a bipartite graph
// where left vertices are keyed "L%d" (0..m-1) and right vertices
// "R%d" (0..n-1). Left nodes are pre-interned so they occupy the low
// NodeID range. edges is a list of (leftIdx, rightIdx) pairs.
func buildBipartiteCSR(m, n int, edges [][2]int) *csr.CSR[struct{}] {
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < m; i++ {
		if err := a.AddNode(fmt.Sprintf("L%05d", i)); err != nil {
			panic(fmt.Sprintf("AddNode L%05d: %v", i, err))
		}
	}
	for i := 0; i < n; i++ {
		if err := a.AddNode(fmt.Sprintf("R%05d", i)); err != nil {
			panic(fmt.Sprintf("AddNode R%05d: %v", i, err))
		}
	}
	for _, e := range edges {
		if err := a.AddEdge(fmt.Sprintf("L%05d", e[0]), fmt.Sprintf("R%05d", e[1]), struct{}{}); err != nil {
			panic(fmt.Sprintf("AddEdge L%05d->R%05d: %v", e[0], e[1], err))
		}
	}
	return csr.BuildFromAdjList(a)
}

// dinicBipartiteMaxFlow computes the maximum matching on a bipartite
// graph via Dinic's algorithm on the standard flow reduction:
//
//   - super-source (node 0) → each left vertex with cap 1
//   - each left vertex → each right vertex (per edge list) with cap 1
//   - each right vertex → super-sink (node m+n+1) with cap 1
func dinicBipartiteMaxFlow(m, n int, edges [][2]int) int {
	// nodes: 0 = super-source, 1..m = left, m+1..m+n = right, m+n+1 = super-sink
	total := m + n + 2
	src := 0
	sink := m + n + 1
	g := flow.NewNetwork(total)
	for i := 0; i < m; i++ {
		g.AddEdge(src, i+1, 1)
	}
	for _, e := range edges {
		g.AddEdge(e[0]+1, m+e[1]+1, 1)
	}
	for i := 0; i < n; i++ {
		g.AddEdge(m+i+1, sink, 1)
	}
	return flow.MaxFlow(g, src, sink)
}

// TestHopcroftKarp_Imbalanced_K30x50 asserts that HopcroftKarp on
// K_{30,50} finds a matching of size 30 (all left vertices saturated).
func TestHopcroftKarp_Imbalanced_K30x50(t *testing.T) {
	t.Parallel()
	const m, n = 30, 50
	edges := make([][2]int, 0, m*n)
	for l := 0; l < m; l++ {
		for r := 0; r < n; r++ {
			edges = append(edges, [2]int{l, r})
		}
	}
	c := buildBipartiteCSR(m, n, edges)
	match := HopcroftKarp(c, int(c.MaxNodeID()))
	if match.Size != m {
		t.Fatalf("K_{30,50} matching size = %d, want %d", match.Size, m)
	}
}

// TestHopcroftKarp_Imbalanced_RandomCrossCheck verifies that on a
// random bipartite graph with m=30, n=50, edge-probability 0.5,
// HopcroftKarp returns the same maximum matching size as Dinic.
func TestHopcroftKarp_Imbalanced_RandomCrossCheck(t *testing.T) {
	t.Parallel()
	const m, n = 30, 50
	r := rand.New(rand.NewPCG(314159, 265358)) //nolint:gosec // deterministic test RNG

	edges := make([][2]int, 0, m*n/2)
	for l := 0; l < m; l++ {
		for ri := 0; ri < n; ri++ {
			if r.Float64() < 0.5 {
				edges = append(edges, [2]int{l, ri})
			}
		}
	}

	c := buildBipartiteCSR(m, n, edges)
	hkSize := HopcroftKarp(c, int(c.MaxNodeID())).Size
	dinicSize := dinicBipartiteMaxFlow(m, n, edges)
	if hkSize != dinicSize {
		t.Fatalf("HopcroftKarp=%d, Dinic=%d (mismatch on random K_{30,50} p=0.5)", hkSize, dinicSize)
	}
}
