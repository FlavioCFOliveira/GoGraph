package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// buildUnitGridCSR constructs an undirected m×ncols grid with all edge
// weights equal to 1 (int64). Node at row i, column j has key i*ncols+j.
// Only right (j→j+1) and down (i→i+ncols) edges are inserted; the
// undirected mirror handles the reverse direction automatically.
func buildUnitGridCSR(tb testing.TB, m, ncols int) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for i := 0; i < m; i++ {
		for j := 0; j < ncols; j++ {
			node := i*ncols + j
			if j+1 < ncols {
				if err := a.AddEdge(node, node+1, 1); err != nil {
					tb.Fatalf("AddEdge horizontal(%d→%d): %v", node, node+1, err)
				}
			}
			if i+1 < m {
				if err := a.AddEdge(node, node+ncols, 1); err != nil {
					tb.Fatalf("AddEdge vertical(%d→%d): %v", node, node+ncols, err)
				}
			}
		}
	}
	return csr.BuildFromAdjList(a), a
}

// bfsGridDistances runs BFS from src and returns the distance map
// (nodeID → depth). It is the reference implementation against which
// Dijkstra is compared.
func bfsGridDistances(c *csr.CSR[int64], src graph.NodeID) map[graph.NodeID]int {
	dist := make(map[graph.NodeID]int)
	BFS(c, src, func(node graph.NodeID, depth int) bool {
		dist[node] = depth
		return true
	})
	return dist
}

// TestDijkstra_GridUnitWeights verifies that Dijkstra distances from
// the top-left corner (key=0) equal both the BFS distances and the
// closed-form Manhattan distance (row + col) for grid sizes (8,8) and
// (32,32).
//
// Manhattan distance from (0,0) to node with key k is (k/ncols) + (k%ncols).
func TestDijkstra_GridUnitWeights(t *testing.T) {
	t.Parallel()

	cases := []struct{ m, ncols int }{
		{8, 8},
		{32, 32},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(itoa(tc.m)+"x"+itoa(tc.ncols), func(t *testing.T) {
			t.Parallel()

			c, a := buildUnitGridCSR(t, tc.m, tc.ncols)
			src, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatal("node 0 not found in mapper")
			}

			dijk, err := Dijkstra(c, src)
			if err != nil {
				t.Fatalf("Dijkstra: %v", err)
			}

			bfsDist := bfsGridDistances(c, src)

			for i := 0; i < tc.m; i++ {
				for j := 0; j < tc.ncols; j++ {
					key := i*tc.ncols + j
					nodeID, _ := a.Mapper().Lookup(key)
					manhattan := i + j

					dijkDist, dijkOK := dijk.Distance(nodeID)
					if !dijkOK {
						t.Errorf("node (%d,%d) key=%d: Dijkstra reports unreachable", i, j, key)
						continue
					}
					if int64(manhattan) != dijkDist {
						t.Errorf("node (%d,%d) key=%d: Dijkstra dist=%d, want %d (Manhattan)", i, j, key, dijkDist, manhattan)
					}

					bfsD, bfsOK := bfsDist[nodeID]
					if !bfsOK {
						t.Errorf("node (%d,%d) key=%d: BFS reports unreachable", i, j, key)
						continue
					}
					if int64(bfsD) != dijkDist {
						t.Errorf("node (%d,%d) key=%d: Dijkstra dist=%d, BFS dist=%d", i, j, key, dijkDist, bfsD)
					}
				}
			}
		})
	}
}
