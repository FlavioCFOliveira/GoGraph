package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestDFS_BalancedBinaryTree verifies that iterative DFS visits every node
// of a perfect binary tree of depth d exactly once.
//
// BalancedBinary(d) produces 2^(d+1)-1 nodes (BFS order, root=0, children
// of i are 2i+1 and 2i+2). The test only checks visit count and uniqueness;
// it does not pin a specific traversal order because the iterative DFS visits
// neighbours in their CSR edge-list order, which may differ from recursive
// pre-order depending on insertion order in the adjlist.
func TestDFS_BalancedBinaryTree(t *testing.T) {
	t.Parallel()

	for _, d := range []int{3, 8, 16} {
		d := d
		wantNodes := (1 << (d + 1)) - 1
		t.Run("depth="+itoa(d), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.BalancedBinary(d).Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("BalancedBinary(%d): %v", d, err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)

			src, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatal("node 0 not found in mapper")
			}

			seen := make(map[graph.NodeID]int, wantNodes) // nodeID -> visit count
			DFS(c, src, func(node graph.NodeID, _ int) bool {
				seen[node]++
				return true
			})

			if len(seen) != wantNodes {
				t.Errorf("DFS visited %d distinct nodes, want %d", len(seen), wantNodes)
			}
			for id, cnt := range seen {
				if cnt != 1 {
					t.Errorf("node %d visited %d times, want 1", id, cnt)
				}
			}
		})
	}
}
