package search

// Task 561: BFS on C_n cycle graph.
//
// Invariants verified for an undirected cycle C_n from any source:
//
//   - max(dist) == floor(n/2)
//   - For each k in [1, floor((n-1)/2)]: exactly 2 nodes at distance k.
//   - At distance floor(n/2): 1 node if n is odd, 2 nodes if n is even.
//   - Total visited count == n.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestBFS_CycleGraph_DistanceHistogram(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 4, 5, 16, 17} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.Cycle(n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			// Run from key 0; by symmetry this is representative.
			srcID, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("key 0 not found in mapper")
			}
			dist := make(map[int]int, n)
			BFS(c, srcID, func(node graph.NodeID, d int) bool {
				v, vok := a.Mapper().Resolve(node)
				if !vok {
					t.Errorf("resolve failed for NodeID %d", node)
					return false
				}
				dist[v] = d
				return true
			})

			if len(dist) != n {
				t.Fatalf("visited %d nodes, want %d (n=%d)", len(dist), n, n)
			}

			maxDist := 0
			for _, d := range dist {
				if d > maxDist {
					maxDist = d
				}
			}
			wantMax := n / 2
			if maxDist != wantMax {
				t.Fatalf("max dist = %d, want floor(%d/2) = %d", maxDist, n, wantMax)
			}

			// Build histogram: count[d] = number of nodes at distance d.
			count := make([]int, maxDist+1)
			for _, d := range dist {
				count[d]++
			}

			// Source has dist 0; exactly one node.
			if count[0] != 1 {
				t.Fatalf("count[0] = %d, want 1", count[0])
			}
			// For k in [1, maxDist-1]: exactly 2 nodes (one clockwise,
			// one counterclockwise from the source).
			for k := 1; k < maxDist; k++ {
				if count[k] != 2 {
					t.Fatalf("count[%d] = %d, want 2 (n=%d)", k, count[k], n)
				}
			}
			// At the diameter floor(n/2):
			//   n even → single antipodal node → 1 node.
			//   n odd  → two equidistant nodes  → 2 nodes.
			wantAtDiameter := 1
			if n%2 != 0 {
				wantAtDiameter = 2
			}
			if count[maxDist] != wantAtDiameter {
				t.Fatalf("count[%d] = %d, want %d (n=%d, %s)",
					maxDist, count[maxDist], wantAtDiameter, n,
					map[bool]string{true: "even", false: "odd"}[n%2 == 0])
			}
		})
	}
}
