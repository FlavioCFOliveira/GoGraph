package search

// Task 564: BFS on K_n complete graph.
//
// In an undirected K_n, every non-source vertex is exactly 1 hop away.
// BFS from any source must visit all n vertices, with dist[v]==1 for
// every v != source and dist[source]==0.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

func TestBFS_CompleteGraph_AllAtDepthOne(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 16, 256} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.Complete(n, false).Build(defaultCfg())
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
			if dist[0] != 0 {
				t.Fatalf("source dist = %d, want 0", dist[0])
			}
			for v := 1; v < n; v++ {
				if dist[v] != 1 {
					t.Fatalf("node %d dist = %d, want 1", v, dist[v])
				}
			}
		})
	}
}
