package scenarios_test

import (
	"context"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/community"
)

// TestFraudDetection_KHopTriangleLeiden builds a 20-node "transaction"
// graph organised in 3 tight clusters with sparse cross-cluster edges,
// then runs a fraud-detection pipeline:
//
//  1. BFS up to depth 2 from a "fraudulent" node to collect its k-hop
//     neighbourhood (should contain all cluster-0 nodes).
//  2. CountTriangles: per-node counts should be non-zero within clusters.
//  3. Leiden community detection: at least 2 distinct communities should
//     be discovered.
//
// Cluster layout:
//
//	Cluster 0: nodes 0..6  (7 nodes) — fully connected clique
//	Cluster 1: nodes 7..13 (7 nodes) — fully connected clique
//	Cluster 2: nodes 14..19 (6 nodes) — fully connected clique
//	Bridge edges: 6→7, 13→14
func TestFraudDetection_KHopTriangleLeiden(t *testing.T) {
	t.Parallel()

	const (
		c0Start, c0End = 0, 6
		c1Start, c1End = 7, 13
		c2Start, c2End = 14, 19

		// "Fraudulent" seed node inside cluster 0.
		fraudNode = 0
	)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	addClique := func(lo, hi int) {
		for i := lo; i <= hi; i++ {
			for j := i + 1; j <= hi; j++ {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					t.Fatalf("AddEdge(%d,%d): %v", i, j, err)
				}
			}
		}
	}

	addClique(c0Start, c0End)
	addClique(c1Start, c1End)
	addClique(c2Start, c2End)

	// Bridge edges connecting adjacent clusters.
	for _, e := range [][2]int{{c0End, c1Start}, {c1End, c2Start}} {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge bridge (%d,%d): %v", e[0], e[1], err)
		}
	}

	c := csr.BuildFromAdjList(a)

	// --- k-hop BFS (depth ≤ 2) from fraudNode ---
	fraudID, ok := a.Mapper().Lookup(fraudNode)
	if !ok {
		t.Fatalf("fraudNode %d not interned", fraudNode)
	}

	khop := make(map[int]struct{})
	bfsErr := search.BFSCtx(context.Background(), c, fraudID, func(id graph.NodeID, depth int) bool {
		if depth > 2 {
			return false
		}
		key, _ := a.Mapper().Resolve(id)
		khop[key] = struct{}{}
		return true
	})
	if bfsErr != nil {
		t.Fatalf("BFSCtx: %v", bfsErr)
	}

	// All cluster-0 nodes must be in the 2-hop set (they share the same clique).
	for n := c0Start; n <= c0End; n++ {
		if _, found := khop[n]; !found {
			t.Errorf("node %d (cluster 0) not found in 2-hop neighbourhood of %d", n, fraudNode)
		}
	}

	// --- CountTriangles ---
	total, perNode := search.CountTriangles(c)
	if total == 0 {
		t.Error("CountTriangles: total = 0, expected > 0 for clique clusters")
	}
	// Every node inside cluster 0 must participate in at least one triangle.
	for n := c0Start; n <= c0End; n++ {
		id, idOK := a.Mapper().Lookup(n)
		if !idOK {
			t.Errorf("node %d not in mapper", n)
			continue
		}
		if int(id) >= len(perNode) || perNode[id] == 0 {
			t.Errorf("node %d (cluster 0) has zero triangles", n)
		}
	}

	// --- Leiden community detection ---
	p, leidenErr := community.LeidenCtx(context.Background(), c, community.DefaultLeidenOptions())
	if leidenErr != nil {
		t.Fatalf("LeidenCtx: %v", leidenErr)
	}
	if p.NumCommunities < 2 {
		t.Errorf("Leiden found %d communities, want >= 2", p.NumCommunities)
	}

	// Count distinct community IDs seen across live nodes (sentinel -1 excluded).
	seen := make(map[int]struct{})
	for _, cid := range p.Community {
		if cid >= 0 {
			seen[cid] = struct{}{}
		}
	}
	if len(seen) < 2 {
		t.Errorf("Leiden Community slice has %d distinct IDs, want >= 2", len(seen))
	}
}
