package search

// Task 643: A* with Manhattan heuristic on unit-weight grids equals Dijkstra.
//
// Builds float64-weight undirected 4-connected grids of sizes 8×8,
// 32×32, and 64×64 and verifies that the path cost returned by AStar
// matches the Dijkstra distance from the same source. The heuristic
// is the Manhattan distance to the goal, which is admissible on
// unit-weight grids.

import (
	"fmt"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestAStar_Grid_ManhattanHeuristic verifies that on unit-weight
// grids the A* cost with a Manhattan-distance heuristic equals the
// Dijkstra distance for the (0,0)→(m-1,n-1) query.
func TestAStar_Grid_ManhattanHeuristic(t *testing.T) {
	t.Parallel()
	cases := []struct{ rows, cols int }{
		{8, 8},
		{32, 32},
		{64, 64},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%dx%d", tc.rows, tc.cols), func(t *testing.T) {
			t.Parallel()
			c, a, srcID, dstID := buildUnitGrid(t, tc.rows, tc.cols)

			dstRow := tc.rows - 1
			dstCol := tc.cols - 1
			ncols := tc.cols
			h := func(id graph.NodeID) float64 {
				v, ok := a.Mapper().Resolve(id)
				if !ok {
					return 0
				}
				row := v / ncols
				col := v % ncols
				dr := dstRow - row
				if dr < 0 {
					dr = -dr
				}
				dc := dstCol - col
				if dc < 0 {
					dc = -dc
				}
				return float64(dr + dc)
			}

			_, costA, errA := AStar(c, srcID, dstID, h)
			if errA != nil {
				t.Fatalf("AStar: %v", errA)
			}

			dij, errD := Dijkstra(c, srcID)
			if errD != nil {
				t.Fatalf("Dijkstra: %v", errD)
			}
			costD, ok := dij.Distance(dstID)
			if !ok {
				t.Fatalf("Dijkstra: dst unreachable")
			}

			if math.Abs(costA-costD) > 1e-12 {
				t.Fatalf("AStar cost = %g, Dijkstra cost = %g (diff > 1e-12)", costA, costD)
			}
		})
	}
}

// buildUnitGrid builds an m×n undirected 4-connected grid with
// float64 edge weights of 1.0. Nodes are keyed by row*ncols+col.
//
//nolint:gocritic // unnamedResult: four-element return is clearer than named vars that shadow loop counters
func buildUnitGrid(tb testing.TB, m, n int) (*csr.CSR[float64], *adjlist.AdjList[int, float64], graph.NodeID, graph.NodeID) {
	tb.Helper()
	adj := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for r := 0; r < m; r++ {
		for col := 0; col < n; col++ {
			cur := r*n + col
			if col+1 < n {
				if err := adj.AddEdge(cur, r*n+col+1, 1.0); err != nil {
					tb.Fatalf("AddEdge h: %v", err)
				}
			}
			if r+1 < m {
				if err := adj.AddEdge(cur, (r+1)*n+col, 1.0); err != nil {
					tb.Fatalf("AddEdge v: %v", err)
				}
			}
		}
	}
	c := csr.BuildFromAdjList(adj)
	srcID, _ := adj.Mapper().Lookup(0)
	dstID, _ := adj.Mapper().Lookup((m-1)*n + (n - 1))
	return c, adj, srcID, dstID
}
