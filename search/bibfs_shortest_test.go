package search

// bibfs_shortest_test.go — Task 928, Sprint 78.
//
// Regression coverage for the level-complete intersection rule:
// BiBFS must return a path whose edge count equals the true
// unweighted shortest distance, even on topologies where the first
// cross-frontier collision encountered during neighbour iteration
// belongs to a strictly longer alternative path.
//
// Each test builds a small undirected graph, computes the BFS hop
// reference from src, runs BiBFS, and asserts the returned path
// length equals the reference. Two adjlist edge insertion orders
// are exercised per topology so neighbour iteration order cannot
// hide a regression.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// bibfsRefDist returns the BFS shortest-hop distance from src to dst
// on c, or -1 when dst is unreachable.
func bibfsRefDist[W any](c *csr.CSR[W], src, dst graph.NodeID) int {
	dist := -1
	BFS(c, src, func(node graph.NodeID, depth int) bool {
		if node == dst {
			dist = depth
			return false
		}
		return true
	})
	return dist
}

// addPath appends a simple chain a — k1 — k2 — … — b through the
// supplied integer keys; the caller owns the key namespace.
func addPath(t *testing.T, a *adjlist.AdjList[int, struct{}], keys ...int) {
	t.Helper()
	for i := 0; i < len(keys)-1; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], struct{}{}); err != nil {
			t.Fatalf("AddEdge(%d—%d): %v", keys[i], keys[i+1], err)
		}
	}
}

// assertBiBFSMatchesBFS runs BiBFS(src,dst) and asserts the returned
// path length (edges) equals the BFS reference, and that the path
// endpoints are src and dst respectively.
func assertBiBFSMatchesBFS(t *testing.T, c *csr.CSR[struct{}], src, dst graph.NodeID, label string) {
	t.Helper()
	want := bibfsRefDist(c, src, dst)
	if want < 0 {
		t.Fatalf("%s: BFS reference says dst unreachable", label)
	}
	path, err := BiBFS(c, src, dst)
	if err != nil {
		t.Fatalf("%s: BiBFS: %v", label, err)
	}
	if len(path) == 0 {
		t.Fatalf("%s: BiBFS returned empty path", label)
	}
	if path[0] != src {
		t.Errorf("%s: path[0] = %v, want src %v", label, path[0], src)
	}
	if path[len(path)-1] != dst {
		t.Errorf("%s: path[last] = %v, want dst %v", label, path[len(path)-1], dst)
	}
	if got := len(path) - 1; got != want {
		t.Fatalf("%s: BiBFS edges=%d, BFS reference=%d (path=%v)", label, got, want, path)
	}
}

// TestBiBFS_TwoDisjointPaths exercises the canonical failure topology
// flagged by the audit: src and dst connected by two disjoint paths
// of length 4 and 5. The first cross-frontier collision in the buggy
// implementation could land on the length-5 path; the level-complete
// rule must always return length 4.
//
// Two adjlist edge insertion orders are tested so neighbour
// iteration order cannot mask the regression: in one ordering the
// long path is inserted first, in the other the short path.
func TestBiBFS_TwoDisjointPaths(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, longFirst bool) (*csr.CSR[struct{}], graph.NodeID, graph.NodeID) {
		t.Helper()
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Key namespace:
		//   src=0, dst=100
		//   short path (4 edges): 0 — 1 — 2 — 3 — 100
		//   long  path (5 edges): 0 — 11 — 12 — 13 — 14 — 100
		if longFirst {
			addPath(t, a, 0, 11, 12, 13, 14, 100)
			addPath(t, a, 0, 1, 2, 3, 100)
		} else {
			addPath(t, a, 0, 1, 2, 3, 100)
			addPath(t, a, 0, 11, 12, 13, 14, 100)
		}
		c := csr.BuildFromAdjList(a)
		src, ok := a.Mapper().Lookup(0)
		if !ok {
			t.Fatalf("src key 0 not in mapper")
		}
		dst, ok := a.Mapper().Lookup(100)
		if !ok {
			t.Fatalf("dst key 100 not in mapper")
		}
		return c, src, dst
	}

	t.Run("long-edge-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, true)
		assertBiBFSMatchesBFS(t, c, src, dst, "long-first")
	})
	t.Run("short-edge-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, false)
		assertBiBFSMatchesBFS(t, c, src, dst, "short-first")
	})
}

// TestBiBFS_AsymmetricDiamond exercises a diamond with one short arm
// (2 edges) and one long arm (3 edges). The collision is forced on
// the middle expansion where both arms' tail nodes enter the same
// frontier; the buggy first-collision rule could return the longer
// arm depending on neighbour order.
func TestBiBFS_AsymmetricDiamond(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, longFirst bool) (*csr.CSR[struct{}], graph.NodeID, graph.NodeID) {
		t.Helper()
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Diamond:
		//   src=0, dst=10
		//   short arm (2 edges): 0 — 1 — 10
		//   long  arm (3 edges): 0 — 21 — 22 — 10
		if longFirst {
			addPath(t, a, 0, 21, 22, 10)
			addPath(t, a, 0, 1, 10)
		} else {
			addPath(t, a, 0, 1, 10)
			addPath(t, a, 0, 21, 22, 10)
		}
		c := csr.BuildFromAdjList(a)
		src, ok := a.Mapper().Lookup(0)
		if !ok {
			t.Fatalf("src key 0 not in mapper")
		}
		dst, ok := a.Mapper().Lookup(10)
		if !ok {
			t.Fatalf("dst key 10 not in mapper")
		}
		return c, src, dst
	}

	t.Run("long-arm-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, true)
		assertBiBFSMatchesBFS(t, c, src, dst, "diamond-long-first")
	})
	t.Run("short-arm-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, false)
		assertBiBFSMatchesBFS(t, c, src, dst, "diamond-short-first")
	})
}

// TestBiBFS_LadderParallelRungs exercises a ladder graph: src and
// dst sit on opposite ends, connected by multiple parallel rungs of
// strictly different lengths. The first collision encountered in the
// adjlist edge order is deliberately not the shortest; the
// level-complete rule must still return the minimum.
//
// Layout (keys are integer labels — undirected edges):
//
//	src=0
//	rung A (3 edges): 0 — A1 — A2 — dst
//	rung B (4 edges): 0 — B1 — B2 — B3 — dst
//	rung C (3 edges): 0 — C1 — C2 — dst
//	rung D (5 edges): 0 — D1 — D2 — D3 — D4 — dst
//
// Shortest is 3. Two edge insertion orders (longest first, shortest
// first) exercise both branches of the bug.
func TestBiBFS_LadderParallelRungs(t *testing.T) {
	t.Parallel()

	const (
		srcK = 0
		dstK = 999
	)

	build := func(t *testing.T, longestFirst bool) (*csr.CSR[struct{}], graph.NodeID, graph.NodeID) {
		t.Helper()
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Rung key namespaces are widely separated so the integer
		// keys map to NodeIDs in the order we insert edges.
		rungA := []int{srcK, 110, 120, dstK}           // 3 edges
		rungB := []int{srcK, 210, 220, 230, dstK}      // 4 edges
		rungC := []int{srcK, 310, 320, dstK}           // 3 edges
		rungD := []int{srcK, 410, 420, 430, 440, dstK} // 5 edges

		if longestFirst {
			addPath(t, a, rungD...)
			addPath(t, a, rungB...)
			addPath(t, a, rungA...)
			addPath(t, a, rungC...)
		} else {
			addPath(t, a, rungA...)
			addPath(t, a, rungC...)
			addPath(t, a, rungB...)
			addPath(t, a, rungD...)
		}
		c := csr.BuildFromAdjList(a)
		src, ok := a.Mapper().Lookup(srcK)
		if !ok {
			t.Fatalf("src key %d not in mapper", srcK)
		}
		dst, ok := a.Mapper().Lookup(dstK)
		if !ok {
			t.Fatalf("dst key %d not in mapper", dstK)
		}
		return c, src, dst
	}

	t.Run("longest-rung-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, true)
		assertBiBFSMatchesBFS(t, c, src, dst, "ladder-longest-first")
	})
	t.Run("shortest-rung-first", func(t *testing.T) {
		t.Parallel()
		c, src, dst := build(t, false)
		assertBiBFSMatchesBFS(t, c, src, dst, "ladder-shortest-first")
	})
}
