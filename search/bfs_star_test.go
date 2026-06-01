package search

// Task 562: BFS on star graph S_{nLeaves}.
//
// shapegen.Star(n, outgoing) builds n total nodes: centre=0, leaves=1..n-1.
// The shape is always directed; cfg.Directed is overridden to true internally.
// For BFS reachability in both directions (centre→leaves and leaf→other-leaves)
// we build the star manually as an undirected adjlist so that BFS can traverse
// spokes in either direction.
//
// Centre-start (src=key 0):
//   - Every leaf (keys 1..nLeaves) is at dist=1.
//   - Total visited == nLeaves+1.
//
// Leaf-start (src=key 1):
//   - Centre (key 0) is at dist=1.
//   - All other leaves (keys 2..nLeaves) are at dist=2.
//   - Total visited == nLeaves+1.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestBFS_StarGraph_CentreStart(t *testing.T) {
	t.Parallel()
	for _, nLeaves := range []int{5, 100, 10_000} {
		nLeaves := nLeaves
		t.Run("leaves="+itoa(nLeaves), func(t *testing.T) {
			t.Parallel()
			a := buildUndirectedStar(t, nLeaves)
			c := csr.BuildFromAdjList(a)
			srcID, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("centre key 0 not in mapper")
			}
			dist := make(map[int]int, nLeaves+1)
			BFS(c, srcID, func(node graph.NodeID, d int) bool {
				v, vok := a.Mapper().Resolve(node)
				if !vok {
					t.Errorf("resolve failed for NodeID %d", node)
					return false
				}
				dist[v] = d
				return true
			})
			if len(dist) != nLeaves+1 {
				t.Fatalf("visited %d nodes, want %d", len(dist), nLeaves+1)
			}
			if dist[0] != 0 {
				t.Fatalf("centre dist = %d, want 0", dist[0])
			}
			for leaf := 1; leaf <= nLeaves; leaf++ {
				if dist[leaf] != 1 {
					t.Fatalf("leaf %d dist = %d, want 1", leaf, dist[leaf])
				}
			}
		})
	}
}

func TestBFS_StarGraph_LeafStart(t *testing.T) {
	t.Parallel()
	for _, nLeaves := range []int{5, 100, 10_000} {
		nLeaves := nLeaves
		t.Run("leaves="+itoa(nLeaves), func(t *testing.T) {
			t.Parallel()
			a := buildUndirectedStar(t, nLeaves)
			c := csr.BuildFromAdjList(a)
			srcID, ok := a.Mapper().Lookup(1)
			if !ok {
				t.Fatalf("leaf key 1 not in mapper")
			}
			dist := make(map[int]int, nLeaves+1)
			BFS(c, srcID, func(node graph.NodeID, d int) bool {
				v, vok := a.Mapper().Resolve(node)
				if !vok {
					t.Errorf("resolve failed for NodeID %d", node)
					return false
				}
				dist[v] = d
				return true
			})
			if len(dist) != nLeaves+1 {
				t.Fatalf("visited %d nodes, want %d", len(dist), nLeaves+1)
			}
			// Centre.
			if dist[0] != 1 {
				t.Fatalf("centre dist = %d, want 1", dist[0])
			}
			// Start leaf itself.
			if dist[1] != 0 {
				t.Fatalf("start leaf dist = %d, want 0", dist[1])
			}
			// All other leaves are two hops away (via centre).
			for leaf := 2; leaf <= nLeaves; leaf++ {
				if dist[leaf] != 2 {
					t.Fatalf("leaf %d dist = %d, want 2", leaf, dist[leaf])
				}
			}
		})
	}
}

// buildUndirectedStar returns an undirected adjlist containing one
// centre node (key=0) and nLeaves leaf nodes (keys 1..nLeaves). The
// centre is connected to every leaf by an undirected edge.
func buildUndirectedStar(tb testing.TB, nLeaves int) *adjlist.AdjList[int, int64] {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	// Add all nodes first so the mapper is fully populated even for
	// the leaf-start test (where not all nodes may be reached).
	if err := a.AddNode(0); err != nil {
		tb.Fatalf("AddNode(0): %v", err)
	}
	for leaf := 1; leaf <= nLeaves; leaf++ {
		if err := a.AddNode(leaf); err != nil {
			tb.Fatalf("AddNode(%d): %v", leaf, err)
		}
	}
	for leaf := 1; leaf <= nLeaves; leaf++ {
		if err := a.AddEdge(0, leaf, 0); err != nil {
			tb.Fatalf("AddEdge(0, %d): %v", leaf, err)
		}
	}
	return a
}
