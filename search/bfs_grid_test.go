package search

// Task 570: BFS on grid L_{m×n}.
//
// shapegen.Grid(m, n, false) builds an undirected 4-neighbour grid.
// Nodes are in row-major order: node at row r, column c has key r*n+c.
//
// From the top-left corner (key=0, position (0,0)), the BFS distance
// to node with key k == r*n+c equals the Manhattan distance r+c.
//
// Verified for (m,n) in {(4,4), (10,20), (64,64)}.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestBFS_Grid_ManhattanDistances(t *testing.T) {
	t.Parallel()
	cases := []struct{ m, n int }{
		{4, 4},
		{10, 20},
		{64, 64},
	}
	for _, tc := range cases {
		tc := tc
		name := itoa(tc.m) + "x" + itoa(tc.n)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.Grid(tc.m, tc.n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			total := tc.m * tc.n
			srcID, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("key 0 not found in mapper")
			}
			dist := make(map[int]int, total)
			BFS(c, srcID, func(node graph.NodeID, d int) bool {
				v, vok := a.Mapper().Resolve(node)
				if !vok {
					t.Errorf("resolve failed for NodeID %d", node)
					return false
				}
				dist[v] = d
				return true
			})
			if len(dist) != total {
				t.Fatalf("visited %d nodes, want %d", len(dist), total)
			}
			cols := tc.n
			for key, got := range dist {
				row := key / cols
				col := key % cols
				want := row + col
				if got != want {
					t.Fatalf("dist[(%d,%d)] = %d, want Manhattan %d+%d=%d",
						row, col, got, row, col, want)
				}
			}
		})
	}
}
