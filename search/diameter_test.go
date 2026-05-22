package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"

	"pgregory.net/rapid"
)

func TestDiameter_Path(t *testing.T) {
	t.Parallel()
	// Path 0-1-2-3-4: diameter = 4.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	lo, hi, exact := Diameter(c)
	if lo != 4 || hi != 4 || !exact {
		t.Fatalf("Diameter = (%d, %d, %v), want (4, 4, true)", lo, hi, exact)
	}
}

func TestDiameter_Cycle(t *testing.T) {
	t.Parallel()
	// Cycle 0-1-2-3-4-0: diameter = floor(5/2) = 2.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, (i+1)%5, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	lo, _, _ := Diameter(c)
	if lo != 2 {
		t.Fatalf("Cycle5 diameter lo = %d, want 2", lo)
	}
}

func TestDiameter_Star(t *testing.T) {
	t.Parallel()
	// Star: hub 0 connected to 1..4. Diameter = 2 (any leaf to any leaf).
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 1; i <= 4; i++ {
		if err := a.AddEdge(0, i, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	lo, _, _ := Diameter(c)
	if lo != 2 {
		t.Fatalf("Star diameter lo = %d, want 2", lo)
	}
}

func TestDiameter_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	c := csr.BuildFromAdjList(a)
	lo, hi, exact := Diameter(c)
	if lo != 0 || hi != 0 || !exact {
		t.Fatalf("Empty diameter = (%d, %d, %v), want (0, 0, true)", lo, hi, exact)
	}
}

// TestDiameter_CompleteBipartite checks K(2,3): every vertex on the
// small side reaches every vertex on the large side in one hop, and
// any two vertices on the same side meet in two hops via either
// vertex on the other side. Diameter is 2.
func TestDiameter_CompleteBipartite(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Left side {0,1}, right side {2,3,4}; full bipartite edges.
	for u := 0; u < 2; u++ {
		for v := 2; v < 5; v++ {
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	lo, hi, exact := Diameter(c)
	if lo != 2 || hi != 2 || !exact {
		t.Fatalf("K(2,3) diameter = (%d, %d, %v), want (2, 2, true)", lo, hi, exact)
	}
}

// bruteDiameter computes the diameter of the connected component
// containing the first live, non-isolated vertex by running BFS
// from every live, non-isolated vertex and taking the maximum
// finite distance encountered. Used as the ground truth against
// which the iFUB result is checked.
func bruteDiameter(c *csr.CSR[struct{}]) int {
	n := int(c.MaxNodeID())
	if n == 0 {
		return 0
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	mask := c.LiveMask()
	scratch := make([]int, n)
	best := 0
	for u := 0; u < n; u++ {
		if !mask[u] || verts[u+1] == verts[u] {
			continue
		}
		_, distU := bfsFarthest(verts, edges, graph.NodeID(u), scratch)
		for _, d := range distU {
			if d > best {
				best = d
			}
		}
	}
	return best
}

// TestDiameter_ExactVsBruteVBFS uses rapid to generate small random
// undirected graphs (12-30 vertices) and verifies the iFUB bound
// envelope lo <= bruteDiameter <= hi, plus that exact==true implies
// lo == bruteDiameter. The scratch-aliasing bug fixed in D3 produced
// a non-tight hi on many of these inputs.
func TestDiameter_ExactVsBruteVBFS(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(12, 30).Draw(rt, "n")
		// Build a simple undirected graph; up to n*2 random edges.
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Ensure n nodes exist; add a spanning path so the graph is
		// connected enough to have a meaningful diameter.
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < n-1; i++ {
			if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		extra := rapid.IntRange(0, n).Draw(rt, "extra")
		for k := 0; k < extra; k++ {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			if u != v {
				if err := a.AddEdge(u, v, struct{}{}); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
		}
		c := csr.BuildFromAdjList(a)
		lo, hi, exact := Diameter(c)
		brute := bruteDiameter(c)
		if lo > brute {
			rt.Fatalf("lo=%d > brute=%d", lo, brute)
		}
		if hi < brute {
			rt.Fatalf("hi=%d < brute=%d (lo=%d, exact=%v)", hi, brute, lo, exact)
		}
		if exact && lo != brute {
			rt.Fatalf("exact==true but lo=%d != brute=%d", lo, brute)
		}
	})
}
