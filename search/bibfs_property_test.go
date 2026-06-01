package search

// bibfs_property_test.go — Task 721, Sprint 62.
//
// Property: on a connected undirected graph, BiBFS path length (in
// edges) equals the BFS hop count from src to dst.

import (
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestProperty_BiBFS_PathLength_EQ_BFSDistance asserts that BiBFS
// returns a path whose edge count equals the BFS shortest-hop count
// on connected undirected graphs.
func TestProperty_BiBFS_PathLength_EQ_BFSDistance(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(5, 30).Draw(rt, "n")

		// Build an undirected graph. adjlist encodes undirected edges
		// as two symmetric directed arcs when Directed=false.
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				rt.Fatalf("AddNode(%d): %v", i, err)
			}
		}

		// Spanning path to guarantee connectivity: 0—1—2—…—(n-1).
		for i := 0; i < n-1; i++ {
			if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
				rt.Fatalf("spanning AddEdge(%d—%d): %v", i, i+1, err)
			}
		}

		// Additional random undirected edges.
		extra := rapid.IntRange(0, 3*n).Draw(rt, "extra")
		for i := 0; i < extra; i++ {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			if u == v {
				continue // skip self-loops; they don't affect hop count
			}
			_ = a.AddEdge(u, v, struct{}{}) // ignore duplicate-edge errors
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
		if srcID == dstID {
			return // trivial case; skip
		}

		// BFS hop count from src to dst.
		bfsDist := -1
		BFS(c, srcID, func(id graph.NodeID, depth int) bool {
			if id == dstID {
				bfsDist = depth
				return false
			}
			return true
		})
		if bfsDist < 0 {
			// Not reachable — spanning path ensures this cannot happen
			// for nodes 0..n-1, but be defensive.
			return
		}

		// BiBFS path from src to dst.
		path, err := BiBFS(c, srcID, dstID)
		if errors.Is(err, ErrNoPath) {
			// BFS found a path but BiBFS did not — property violated.
			rt.Fatalf("BFS reached dst (depth=%d) but BiBFS returned ErrNoPath (src=%d dst=%d)",
				bfsDist, srcKey, dstKey)
		}
		if err != nil {
			rt.Fatalf("BiBFS error: %v", err)
		}

		pathLen := len(path) - 1 //nolint:gocritic // arithmetic comment, not commented-out code
		if pathLen != bfsDist {
			rt.Fatalf(
				"BiBFS path length=%d edges != BFS hops=%d (src=%d dst=%d path=%v)",
				pathLen, bfsDist, srcKey, dstKey, path,
			)
		}
	})
}
