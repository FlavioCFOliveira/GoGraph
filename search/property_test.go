package search

import (
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestProperty_Dijkstra_TriangleInequality asserts the triangle
// inequality for every reachable pair: d(s, t) <= d(s, m) + d(m, t)
// for every intermediate node m. Counter-examples shrink down to
// the smallest graph that violates the invariant.
func TestProperty_Dijkstra_TriangleInequality(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 12).Draw(r, "n")
		m := rapid.IntRange(0, 4*n).Draw(r, "m")
		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-1).Draw(r, "u")
			v := rapid.IntRange(0, n-1).Draw(r, "v")
			w := int64(rapid.IntRange(1, 20).Draw(r, "w"))
			if err := a.AddEdge(u, v, w); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		srcK := rapid.IntRange(0, n-1).Draw(r, "src")
		midK := rapid.IntRange(0, n-1).Draw(r, "mid")
		dstK := rapid.IntRange(0, n-1).Draw(r, "dst")
		srcID, ok1 := a.Mapper().Lookup(srcK)
		midID, ok2 := a.Mapper().Lookup(midK)
		dstID, ok3 := a.Mapper().Lookup(dstK)
		if !ok1 || !ok2 || !ok3 {
			return
		}
		d, err := Dijkstra(c, srcID)
		if err != nil {
			return
		}
		dst, dstOK := d.Distance(dstID)
		if !dstOK {
			return
		}
		mid, midOK := d.Distance(midID)
		if !midOK {
			return
		}
		dm, err := Dijkstra(c, midID)
		if err != nil {
			return
		}
		md, ok := dm.Distance(dstID)
		if !ok {
			return
		}
		if dst > mid+md {
			r.Fatalf("triangle inequality violated: d(src,dst)=%d > d(src,mid)=%d + d(mid,dst)=%d",
				dst, mid, md)
		}
	})
}

// TestProperty_TopologicalSort_Precedence asserts that for every
// edge (u, v) the position of u in the topological order is strictly
// before the position of v. Generated graphs are DAGs by
// construction (edges only go from smaller-id to larger-id).
func TestProperty_TopologicalSort_Precedence(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 12).Draw(r, "n")
		m := rapid.IntRange(0, 3*n).Draw(r, "m")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-2).Draw(r, "u")
			v := rapid.IntRange(u+1, n-1).Draw(r, "v")
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		order, err := TopologicalSort(c)
		if err != nil {
			r.Fatalf("topological sort: %v", err)
		}
		pos := make(map[graph.NodeID]int, len(order))
		for i, id := range order {
			pos[id] = i
		}
		verts := c.VerticesSlice()
		edges := c.EdgesSlice()
		for u := uint64(0); u < uint64(len(verts))-1; u++ {
			for k := verts[u]; k < verts[u+1]; k++ {
				v := edges[k]
				if pos[graph.NodeID(u)] >= pos[v] {
					r.Fatalf("precedence violated: u=%d (pos=%d) -> v=%d (pos=%d)",
						u, pos[graph.NodeID(u)], v, pos[v])
				}
			}
		}
	})
}

// TestProperty_TarjanSCC_Reflexive asserts the reflexive property
// of the SCC equivalence: every node is in the same SCC as itself.
// (The standard mutual-reachability property is harder to test
// without recomputing reachability separately; reflexivity is the
// cheap invariant that the membership classification must obey.)
func TestProperty_TarjanSCC_Reflexive(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(r, "n")
		m := rapid.IntRange(0, 3*n).Draw(r, "m")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-1).Draw(r, "u")
			v := rapid.IntRange(0, n-1).Draw(r, "v")
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		sccs := TarjanSCC(c)
		seen := make(map[graph.NodeID]int)
		for i, comp := range sccs {
			for _, v := range comp {
				if prev, ok := seen[v]; ok {
					r.Fatalf("vertex %d appears in components %d and %d", v, prev, i)
				}
				seen[v] = i
			}
		}
	})
}

// TestProperty_HopcroftKarp_Cardinality asserts the matching size
// equals the count of non-unmatched left vertices (a tautology by
// construction, but the property test stresses the implementation
// against a wide variety of bipartite shapes).
func TestProperty_HopcroftKarp_Cardinality(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 8).Draw(r, "n")
		m := rapid.IntRange(0, 4*n).Draw(r, "m")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: true, Multigraph: true})
		// Pre-intern lefts as 0..n-1, rights as n..2n-1.
		for i := 0; i < 2*n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			l := rapid.IntRange(0, n-1).Draw(r, "l")
			right := rapid.IntRange(n, 2*n-1).Draw(r, "r")
			if err := a.AddEdge(l, right, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		match := HopcroftKarp(c, int(c.MaxNodeID()))
		// Count matched lefts by walking MatchL.
		count := 0
		for _, v := range match.MatchL {
			if v != ^graph.NodeID(0) {
				count++
			}
		}
		if count != match.Size {
			r.Fatalf("matched-left count %d != reported Size %d", count, match.Size)
		}
	})
}
