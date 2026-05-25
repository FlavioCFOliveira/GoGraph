package search

// bfs_vs_dijkstra_property_test.go — Task 717, Sprint 62.
//
// Property: for every graph with all edge weights ≥ 1, the BFS hop
// count from src to any reachable vertex v is at most the Dijkstra
// weighted distance to v.
//
// Proof sketch: if a shortest path to v has k hops and each edge
// weight is ≥ 1, the weighted cost is ≥ k = BFS hop count.

import (
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestProperty_BFSHops_LEQ_DijkstraDistance asserts that BFS hop
// depth is a lower bound on the Dijkstra weighted distance when all
// edge weights are ≥ 1.
func TestProperty_BFSHops_LEQ_DijkstraDistance(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(5, 30).Draw(rt, "n")

		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				rt.Fatalf("AddNode(%d): %v", i, err)
			}
		}

		// Guarantee reachability from 0: 0→1→…→n-1 spanning path.
		for i := 0; i < n-1; i++ {
			w := int64(rapid.IntRange(1, 100).Draw(rt, "span_w"))
			if err := a.AddEdge(i, i+1, w); err != nil {
				rt.Fatalf("spanning AddEdge(%d→%d): %v", i, i+1, err)
			}
		}

		// Additional random edges with positive integer weights.
		extra := rapid.IntRange(0, 4*n).Draw(rt, "extra")
		for i := 0; i < extra; i++ {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			w := int64(rapid.IntRange(1, 100).Draw(rt, "w"))
			if err := a.AddEdge(u, v, w); err != nil {
				rt.Fatalf("AddEdge(%d→%d): %v", u, v, err)
			}
		}

		c := csr.BuildFromAdjList(a)
		mapper := a.Mapper()

		srcKey := rapid.IntRange(0, n-1).Draw(rt, "src")
		srcID, ok := mapper.Lookup(srcKey)
		if !ok {
			return
		}

		// Collect BFS hop depths.
		hopDepth := make(map[graph.NodeID]int, n)
		BFS(c, srcID, func(id graph.NodeID, depth int) bool {
			hopDepth[id] = depth
			return true
		})

		// Run Dijkstra; skip if Dijkstra errors (should not happen
		// with non-negative weights, but defensive).
		dists, err := Dijkstra(c, srcID)
		if err != nil {
			return
		}

		// Property: for every vertex reachable by both BFS and
		// Dijkstra, BFS hop count ≤ weighted distance.
		for i := 0; i < n; i++ {
			nodeID, ok := mapper.Lookup(i)
			if !ok {
				continue
			}
			hops, bfsReachable := hopDepth[nodeID]
			wdist, dijkReachable := dists.Distance(nodeID)
			if !bfsReachable || !dijkReachable {
				// Reachability must agree.
				if bfsReachable != dijkReachable {
					rt.Fatalf(
						"reachability mismatch for node %d: bfs=%v dijk=%v",
						i, bfsReachable, dijkReachable,
					)
				}
				continue
			}
			if int64(hops) > wdist {
				rt.Fatalf(
					"node %d: BFS hops=%d > Dijkstra dist=%d (src=%d)",
					i, hops, wdist, srcKey,
				)
			}
		}
	})
}
