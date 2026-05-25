package search

import (
	"testing"

	"pgregory.net/rapid"
)

// bipartiteShape holds the parameters needed to build and cross-check
// a bipartite graph.
type bipartiteShape struct {
	m, n  int
	edges [][2]int
}

// crossCheckHKvsDinic runs HopcroftKarp and Dinic on the same bipartite
// graph and asserts the matching sizes agree.
func crossCheckHKvsDinic(t testing.TB, shape bipartiteShape) {
	t.Helper()
	c := buildBipartiteCSR(shape.m, shape.n, shape.edges)
	hkSize := HopcroftKarp(c, int(c.MaxNodeID())).Size
	dinicSize := dinicBipartiteMaxFlow(shape.m, shape.n, shape.edges)
	if hkSize != dinicSize {
		t.Errorf("HopcroftKarp=%d, Dinic=%d (m=%d n=%d edges=%d)",
			hkSize, dinicSize, shape.m, shape.n, len(shape.edges))
	}
}

// TestCrossCheck_Bipartite_Imbalanced cross-checks HK against Dinic
// on a sparse imbalanced bipartite graph with m=20, n=30.
func TestCrossCheck_Bipartite_Imbalanced(t *testing.T) {
	t.Parallel()
	const m, n = 20, 30
	// Fixed edge set: L_i connects to R_i and R_{i+1} (where available).
	edges := make([][2]int, 0, 2*m)
	for i := 0; i < m; i++ {
		edges = append(edges, [2]int{i, i})
		if i+1 < n {
			edges = append(edges, [2]int{i, i + 1})
		}
	}
	crossCheckHKvsDinic(t, bipartiteShape{m: m, n: n, edges: edges})
}

// TestCrossCheck_Bipartite_K50x50 cross-checks HK against Dinic on
// K_{50,50}.
func TestCrossCheck_Bipartite_K50x50(t *testing.T) {
	t.Parallel()
	const n = 50
	edges := make([][2]int, 0, n*n)
	for l := 0; l < n; l++ {
		for r := 0; r < n; r++ {
			edges = append(edges, [2]int{l, r})
		}
	}
	crossCheckHKvsDinic(t, bipartiteShape{m: n, n: n, edges: edges})
}

// TestCrossCheck_Bipartite_RandomP03 cross-checks HK against Dinic on
// a random bipartite graph with m=n=30 and edge probability 0.3.
func TestCrossCheck_Bipartite_RandomP03(t *testing.T) {
	t.Parallel()
	// Pre-computed edge list from a fixed PRNG seed.
	const m, n = 30, 30
	// Use a simple LCG to avoid importing rand again — seed chosen so
	// ~30% of 900 edges are generated.
	edges := make([][2]int, 0, int(float64(m*n)*0.3+1))
	v := uint64(0xc0ffee42)
	for l := 0; l < m; l++ {
		for r := 0; r < n; r++ {
			v = v*6364136223846793005 + 1442695040888963407
			if v>>62 == 0 { // ~25% probability
				edges = append(edges, [2]int{l, r})
			}
		}
	}
	crossCheckHKvsDinic(t, bipartiteShape{m: m, n: n, edges: edges})
}

// TestCrossCheck_Bipartite_Rapid uses property-based testing to
// cross-check HK against Dinic on randomly generated bipartite graphs.
// Graph sizes are kept small (m,n ≤ 15) so Dinic stays fast.
func TestCrossCheck_Bipartite_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		m := rapid.IntRange(1, 15).Draw(rt, "m")
		n := rapid.IntRange(1, 15).Draw(rt, "n")
		edgeCount := rapid.IntRange(0, m*n).Draw(rt, "edgeCount")

		// Build a deduplicated edge set.
		type pair struct{ l, r int }
		edgeSet := make(map[pair]struct{}, edgeCount)
		for range edgeCount {
			l := rapid.IntRange(0, m-1).Draw(rt, "l")
			r := rapid.IntRange(0, n-1).Draw(rt, "r")
			edgeSet[pair{l, r}] = struct{}{}
		}
		edges := make([][2]int, 0, len(edgeSet))
		for e := range edgeSet {
			edges = append(edges, [2]int{e.l, e.r})
		}

		c := buildBipartiteCSR(m, n, edges)
		hkSize := HopcroftKarp(c, int(c.MaxNodeID())).Size
		dinicSize := dinicBipartiteMaxFlow(m, n, edges)
		if hkSize != dinicSize {
			rt.Errorf("HopcroftKarp=%d, Dinic=%d (m=%d n=%d edges=%d)",
				hkSize, dinicSize, m, n, len(edges))
		}
	})
}
