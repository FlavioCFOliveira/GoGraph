package search

// Task 681: Yen on an undirected star — k > number of available paths.
//
// An undirected star S_8 (centre=0, leaves=1..7, total 8 nodes) has
// exactly one loopless path from any leaf to any other leaf:
// leaf → centre → leaf. Requesting k=10 must silently truncate to the
// number of paths that actually exist (1), returning a slice of length
// 1 rather than 10 and without returning an error.
//
// The star is built manually using adjlist.Config{Directed:false}
// because shapegen.Star is always directed (the orientation of edges is
// the defining property of the catalogue shape). A hand-built undirected
// star is topologically identical and avoids relying on catalogue
// implementation details.
//
// Acceptance criteria:
//   - len(result) == 1
//   - result[0].Nodes == [leaf, centre, leaf] (in mapper NodeID form)
//   - YenKShortest does not panic, return nil, or return > 1 path.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestYen_StarTruncated_KExceedsPathCount(t *testing.T) {
	t.Parallel()

	const nLeaves = 7
	const n = 1 + nLeaves // centre + 7 leaves = 8 nodes

	// Build undirected star: centre=0, leaves=1..7.
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for leaf := 1; leaf < n; leaf++ {
		if err := a.AddEdge(0, leaf, 1); err != nil {
			t.Fatalf("AddEdge(0→%d): %v", leaf, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	// Query: leaf 1 → leaf 2 with k=10.
	src, ok := m.Lookup(1)
	if !ok {
		t.Fatal("key 1 not in mapper")
	}
	dst, ok := m.Lookup(2)
	if !ok {
		t.Fatal("key 2 not in mapper")
	}
	centre, ok := m.Lookup(0)
	if !ok {
		t.Fatal("key 0 (centre) not in mapper")
	}

	paths := YenKShortest(c, src, dst, 10)

	if len(paths) != 1 {
		t.Fatalf("expected exactly 1 path, got %d", len(paths))
	}

	p := paths[0]
	if len(p.Nodes) != 3 {
		t.Fatalf("path length = %d, want 3 (leaf→centre→leaf)", len(p.Nodes))
	}
	if p.Nodes[0] != graph.NodeID(src) {
		t.Fatalf("path[0] = %v, want src (%v)", p.Nodes[0], src)
	}
	if p.Nodes[1] != graph.NodeID(centre) {
		t.Fatalf("path[1] = %v, want centre (%v)", p.Nodes[1], centre)
	}
	if p.Nodes[2] != graph.NodeID(dst) {
		t.Fatalf("path[2] = %v, want dst (%v)", p.Nodes[2], dst)
	}
}
