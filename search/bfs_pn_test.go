package search

// Task 559: BFS on P_n path graph.
//
// Verifies that BFS from node 0 on a directed path graph P_n assigns
// dist[i] == i for every node i in [0, n). Uses shapegen.Path so the
// graph structure is the canonical catalogue fixture.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestBFS_PathGraph_DistanceEqualsIndex(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 16, 1024} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.Path(n, true).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
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
				t.Fatalf("visited %d nodes, want %d", len(dist), n)
			}
			for i := 0; i < n; i++ {
				if dist[i] != i {
					t.Errorf("dist[%d] = %d, want %d", i, dist[i], i)
				}
			}
		})
	}
}
