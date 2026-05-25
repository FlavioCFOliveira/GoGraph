package search

import (
	"testing"
)

// TestHopcroftKarp_ForcedPath_StaircaseGraph runs HopcroftKarp on a
// "staircase" bipartite graph designed so that each BFS phase finds
// exactly one augmenting path of increasing odd length. The graph has
// m=n=20 with left vertex i connected only to right vertex i and
// right vertex i-1 (for i>0). This forces cascading augmentation
// across previously matched edges.
//
// Correctness is verified by:
//  1. Exact matching size equals 20 (all left vertices saturated).
//  2. Dinic cross-check agrees.
func TestHopcroftKarp_ForcedPath_StaircaseGraph(t *testing.T) {
	t.Parallel()
	const n = 20
	edges := make([][2]int, 0, 2*n-1)
	for i := 0; i < n; i++ {
		edges = append(edges, [2]int{i, i})
		if i > 0 {
			edges = append(edges, [2]int{i, i - 1})
		}
	}

	c := buildBipartiteCSR(n, n, edges)
	match := HopcroftKarp(c, int(c.MaxNodeID()))
	if match.Size != n {
		t.Fatalf("staircase(n=%d) matching size = %d, want %d", n, match.Size, n)
	}

	dinicSize := dinicBipartiteMaxFlow(n, n, edges)
	if match.Size != dinicSize {
		t.Fatalf("HopcroftKarp=%d, Dinic=%d (staircase cross-check failed)", match.Size, dinicSize)
	}
}

// TestHopcroftKarp_ForcedPath_ChainedAugmentation builds a graph
// where augmenting paths must cross multiple previously matched edges
// to find free right vertices. Left i → Right i and Left i → Right i+1
// (where i+1 < n). Each phase is forced to extend augmenting path
// length.
func TestHopcroftKarp_ForcedPath_ChainedAugmentation(t *testing.T) {
	t.Parallel()
	const n = 20
	edges := make([][2]int, 0, 2*n-1)
	for i := 0; i < n; i++ {
		edges = append(edges, [2]int{i, i})
		if i+1 < n {
			edges = append(edges, [2]int{i, i + 1})
		}
	}

	c := buildBipartiteCSR(n, n, edges)
	match := HopcroftKarp(c, int(c.MaxNodeID()))
	dinicSize := dinicBipartiteMaxFlow(n, n, edges)
	if match.Size != dinicSize {
		t.Fatalf("HopcroftKarp=%d, Dinic=%d (chained augmentation cross-check failed)", match.Size, dinicSize)
	}
}
