package search

// Task 868: Johnson APSP on a sparse layered graph with mixed-sign
// weights but no negative cycle.
//
// A DAG structure (edges only from smaller to larger vertex) guarantees
// no negative cycle regardless of edge-weight signs. The fixture has
// 8 vertices in 4 layers of 2; every layer feeds the next.
//
// Cross-check: for every source vertex s, BellmanFord(c, NodeID(s))
// must agree with JohnsonAPSP.At(NodeID(s), NodeID(t)) for all t.
//
// Acceptance criteria:
//  1. JohnsonAPSP returns no error.
//  2. For every (s,t): reachability reported by Johnson equals
//     reachability reported by BellmanFord.
//  3. For every reachable (s,t): Johnson distance == BellmanFord
//     distance.

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// layeredNegEdges is a small layered DAG with mixed-sign int64 weights.
// Layers: {0,1}, {2,3}, {4,5}, {6,7}.
// All edges go from lower-numbered to higher-numbered vertices,
// guaranteeing acyclicity (and therefore no negative cycle).
var layeredNegEdges = []weightedEdge{
	// Layer 0 -> Layer 1
	{0, 2, 2}, {0, 3, -1},
	{1, 2, 5}, {1, 3, 3},
	// Layer 1 -> Layer 2
	{2, 4, -2}, {2, 5, 1},
	{3, 4, 4}, {3, 5, -3},
	// Layer 2 -> Layer 3
	{4, 6, 1}, {4, 7, 2},
	{5, 6, -1}, {5, 7, 3},
}

func TestJohnsonAPSP_SparseNegatives(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	// Intern all 8 vertices explicitly so BellmanFord has a defined
	// NodeID for every vertex regardless of edge coverage.
	for i := 0; i < 8; i++ {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	for _, e := range layeredNegEdges {
		if err := a.AddEdge(e.from, e.to, e.w); err != nil {
			t.Fatalf("AddEdge %d->%d: %v", e.from, e.to, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	// AC 1: JohnsonAPSP must succeed (no negative cycle in a DAG).
	apspJ, err := JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("JohnsonAPSP: %v", err)
	}

	// Cross-check every (s,t) pair against BellmanFord.
	for s := 0; s < 8; s++ {
		sID, ok := mapper.Lookup(s)
		if !ok {
			t.Fatalf("source key %d not in mapper", s)
		}
		bfDist, err := BellmanFord(c, sID)
		if err != nil {
			t.Fatalf("BellmanFord(src=%d): %v", s, err)
		}

		for tt := 0; tt < 8; tt++ {
			tID, ok := mapper.Lookup(tt)
			if !ok {
				t.Fatalf("target key %d not in mapper", tt)
			}
			jDist, jOK := apspJ.At(sID, tID)
			bDist, bOK := bfDist.Distance(tID)

			// Self-distance: s == tt (same user key).
			if s == tt {
				if !jOK {
					t.Errorf("(%d,%d) self: Johnson reports unreachable", s, tt)
				}
				if jDist != 0 {
					t.Errorf("(%d,%d) self: Johnson dist=%d, want 0", s, tt, jDist)
				}
				continue
			}

			// AC 2: reachability agreement.
			if jOK != bOK {
				t.Errorf("(%d,%d): reachability Johnson=%v BellmanFord=%v",
					s, tt, jOK, bOK)
				continue
			}
			// AC 3: distance agreement when reachable.
			if jOK && jDist != bDist {
				t.Errorf("(%d,%d): Johnson=%d BellmanFord=%d", s, tt, jDist, bDist)
			}
		}
	}
}
