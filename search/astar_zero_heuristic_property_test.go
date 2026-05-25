package search

// astar_zero_heuristic_property_test.go — Task 732, Sprint 62.
//
// Property: A* with h=0 (zero heuristic) is equivalent to Dijkstra.
// For every randomly generated graph and every reachable dst, the
// path cost returned by AStar must match the Dijkstra distance to
// within floating-point epsilon.

import (
	"math"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

const astarEpsilon = 1e-12

// zeroHeuristic is the admissible h=0 that degrades A* to Dijkstra.
func zeroHeuristic(_ graph.NodeID) float64 { return 0 }

// TestProperty_AStar_ZeroHeuristic_EqualsDijkstra asserts that A*
// with the zero heuristic produces the same cost as Dijkstra for all
// reachable vertices on directed graphs with positive float64 weights.
func TestProperty_AStar_ZeroHeuristic_EqualsDijkstra(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(5, 20).Draw(rt, "n")

		a := adjlist.New[int, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				rt.Fatalf("AddNode(%d): %v", i, err)
			}
		}

		// Spanning path to ensure some reachable pairs exist.
		for i := 0; i < n-1; i++ {
			w := rapid.Float64Range(0.1, 10.0).Draw(rt, "span_w")
			if err := a.AddEdge(i, i+1, w); err != nil {
				rt.Fatalf("spanning AddEdge(%d→%d): %v", i, i+1, err)
			}
		}

		extra := rapid.IntRange(0, 3*n).Draw(rt, "extra")
		for i := 0; i < extra; i++ {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			w := rapid.Float64Range(0.1, 10.0).Draw(rt, "w")
			if err := a.AddEdge(u, v, w); err != nil {
				rt.Fatalf("AddEdge(%d→%d): %v", u, v, err)
			}
		}

		c := csr.BuildFromAdjList(a)
		mapper := a.Mapper()

		srcKey := rapid.IntRange(0, n-1).Draw(rt, "src")
		dstKey := rapid.IntRange(0, n-1).Draw(rt, "dst")
		srcID, okS := mapper.Lookup(srcKey)
		dstID, okD := mapper.Lookup(dstKey)
		if !okS || !okD {
			return
		}

		// Dijkstra from src.
		dists, err := Dijkstra(c, srcID)
		if err != nil {
			// Should not happen with positive float64 weights; skip.
			return
		}
		dijkDist, dijkReachable := dists.Distance(dstID)

		// A* from src to dst with h=0.
		path, astarCost, astarErr := AStar(c, srcID, dstID, zeroHeuristic)

		switch {
		case !dijkReachable && astarErr != nil:
			// Both agree: no path.
			return
		case !dijkReachable && astarErr == nil:
			rt.Fatalf(
				"Dijkstra says unreachable but A* returned cost=%g path=%v (src=%d dst=%d)",
				astarCost, path, srcKey, dstKey,
			)
		case dijkReachable && astarErr != nil:
			rt.Fatalf(
				"Dijkstra dist=%g but A* returned error=%v (src=%d dst=%d)",
				dijkDist, astarErr, srcKey, dstKey,
			)
		default:
			// Both found a path; costs must agree.
			if math.Abs(astarCost-dijkDist) > astarEpsilon {
				rt.Fatalf(
					"A* cost=%g != Dijkstra dist=%g (diff=%g, src=%d dst=%d)",
					astarCost, dijkDist, math.Abs(astarCost-dijkDist), srcKey, dstKey,
				)
			}
		}
	})
}
