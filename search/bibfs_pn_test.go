package search

// Task 661: BiBFS on a long path graph P_n.
//
// Builds an undirected P_10000 via shapegen.Path, then verifies:
//   1. The path returned by BiBFS has the correct length (n nodes).
//   2. The first and last elements are the expected src and dst node IDs.
//   3. The hop distance BiBFS reports (len(path)-1) equals the BFS
//      level measured by a unidirectional BFS from src.
//
// Note: no wall-clock timing assertion is applied. BiBFS internally
// calls BuildReverse (O(V+E)) on every invocation, so on a path graph
// — where bidirectional search provides no structural advantage — it
// is consistently slower than unidirectional BFS. The timing benefit
// of BiBFS manifests on graphs with exponential BFS ball growth, not
// on linear chains.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestBiBFS_LongPath verifies BiBFS on an undirected P_10000.
func TestBiBFS_LongPath(t *testing.T) {
	t.Parallel()

	const n = 10_000
	// shapegen.Path with directed=false produces an undirected path,
	// which is required by BiBFS for symmetric traversal.
	g, err := shapegen.Path(n, false).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)

	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not in mapper")
	}
	dstID, ok2 := a.Mapper().Lookup(n - 1)
	if !ok2 {
		t.Fatalf("key %d not in mapper", n-1)
	}

	// --- Correctness ---
	path, bibfsErr := BiBFS(c, srcID, dstID)
	if bibfsErr != nil {
		t.Fatalf("BiBFS: %v", bibfsErr)
	}
	if len(path) != n {
		t.Fatalf("path length = %d, want %d (n nodes)", len(path), n)
	}
	if path[0] != srcID {
		t.Errorf("path[0] = %v, want srcID %v", path[0], srcID)
	}
	if path[n-1] != dstID {
		t.Errorf("path[n-1] = %v, want dstID %v", path[n-1], dstID)
	}

	bibfsDist := len(path) - 1 // n-1 edges

	// Unidirectional BFS to measure the reference hop count.
	bfsDist := -1
	BFS(c, srcID, func(node graph.NodeID, depth int) bool {
		if node == dstID {
			bfsDist = depth
			return false // stop early
		}
		return true
	})
	if bfsDist < 0 {
		t.Fatalf("BFS did not reach dst")
	}
	if bibfsDist != bfsDist {
		t.Fatalf("BiBFS dist = %d, BFS dist = %d (want equal)", bibfsDist, bfsDist)
	}

}
