package search

import (
	"testing"
)

// TestHopcroftKarp_Knn_Dense asserts that HopcroftKarp on K_{100,100}
// (the complete bipartite graph with 100 vertices on each side)
// finds a perfect matching of size 100.
//
// Additional invariants checked:
//   - Every left vertex has a distinct right partner.
//   - No two left vertices share a right partner.
func TestHopcroftKarp_Knn_Dense(t *testing.T) {
	t.Parallel()
	const n = 100
	edges := make([][2]int, 0, n*n)
	for l := 0; l < n; l++ {
		for r := 0; r < n; r++ {
			edges = append(edges, [2]int{l, r})
		}
	}

	c := buildBipartiteCSR(n, n, edges)
	match := HopcroftKarp(c, int(c.MaxNodeID()))

	if match.Size != n {
		t.Fatalf("K_{100,100} matching size = %d, want %d", match.Size, n)
	}

	// Verify: every left vertex that is matched has a unique right partner.
	// MatchL has length nLeft == MaxNodeID; only entries for actual left
	// vertices (NodeIDs 0..n*2-1 from pre-interning) are meaningful.
	// We cannot map back from NodeID to "left vs right" without the
	// mapper, so we verify uniqueness over the MatchL slice which covers
	// nLeft = MaxNodeID entries. The algorithm sets unmatched entries to
	// ^NodeID(0); matched entries to the partner NodeID.
	seen := make(map[int64]bool, n)
	matched := 0
	for _, v := range match.MatchL {
		if int64(v) < 0 { // unmatched sentinel (^NodeID(0) wraps negative in int64)
			continue
		}
		matched++
		if seen[int64(v)] {
			t.Fatalf("right vertex %d matched to two left vertices", v)
		}
		seen[int64(v)] = true
	}
	if matched != n {
		t.Fatalf("matched left vertices = %d, want %d", matched, n)
	}
}
