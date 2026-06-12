package search

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildDiamondPlus constructs a 5-node, 7-edge directed graph that
// provokes duplicate path generation in an unguarded Yen implementation.
//
// Topology (all edge weights 1):
//
//	0 → 1 → 3 → 4   (path A: cost 3)
//	0 → 2 → 3 → 4   (path B: cost 3)
//	0 → 1 → 4        (path C: cost 2)
//	0 → 2 → 4        (path D: cost 2)
//
// The shortcut edges 1→4 and 2→4 create two spur points (nodes 1 and 2)
// that, during different rounds, can regenerate the same full path — e.g.
// round 1 spur at node 0 yields 0→1→3→4 and 0→2→3→4 as candidates,
// while round 2 spur nodes can regenerate one of them again.
func buildDiamondPlus(t *testing.T) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	t.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	edges := [][3]int{{0, 1, 1}, {0, 2, 1}, {1, 3, 1}, {2, 3, 1}, {3, 4, 1}, {1, 4, 1}, {2, 4, 1}}
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], int64(e[2])); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
		}
	}
	return csr.BuildFromAdjList(a), a
}

// nodeSeqEqual returns true when two NodeID slices contain the same
// elements in the same order.
func nodeSeqEqual(a, b []graph.NodeID) bool {
	return slices.Equal(a, b)
}

// TestYenKShortest_NoDuplicates asserts that YenKShortest never returns
// two paths with identical node sequences, even when multiple spur nodes
// or multiple rounds would regenerate the same full path.
//
// This test MUST fail on the unguarded implementation (no seen-map check)
// and MUST pass after the deduplication fix.
func TestYenKShortest_NoDuplicates(t *testing.T) {
	t.Parallel()
	c, a := buildDiamondPlus(t)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(4)

	const k = 8
	paths := YenKShortest(c, src, dst, k)
	if len(paths) == 0 {
		t.Fatal("expected at least one path")
	}

	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if nodeSeqEqual(paths[i].Nodes, paths[j].Nodes) {
				t.Errorf("duplicate path at indices %d and %d: %v", i, j, paths[i].Nodes)
			}
		}
	}
}

// TestYenKShortest_NoDuplicates_RoundTrip verifies that when the number
// of loopless paths from src to dst is fewer than k, the returned slice
// length equals the actual distinct-path count — i.e. the result is NOT
// padded with duplicates to reach k.
//
// The diamond-plus graph (nodes 0→4) has exactly 4 distinct loopless
// paths (A, B, C, D — see buildDiamondPlus). Requesting k=8 must return
// exactly 4 paths, not 8.
func TestYenKShortest_NoDuplicates_RoundTrip(t *testing.T) {
	t.Parallel()
	c, a := buildDiamondPlus(t)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(4)

	const k = 8
	const wantDistinct = 4
	paths := YenKShortest(c, src, dst, k)

	if len(paths) != wantDistinct {
		t.Fatalf("got %d paths, want exactly %d distinct paths", len(paths), wantDistinct)
	}

	// Secondary check: all returned paths must also be pairwise distinct.
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if nodeSeqEqual(paths[i].Nodes, paths[j].Nodes) {
				t.Errorf("duplicate path at indices %d and %d: %v", i, j, paths[i].Nodes)
			}
		}
	}
}
