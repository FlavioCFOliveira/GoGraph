package csr_test

import (
	"reflect"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// shapes is the fixed representative set used by the rapid and
// deterministic table-driven tests. Constructed once at package level
// to avoid re-evaluating constructors on every rapid iteration.
//
// Some entries are undirected by construction (CompleteBipartite,
// BarabasiAlbert, WattsStrogatz): their Build methods ignore the
// caller's cfg.Directed flag and force Directed=false. The size
// assertions account for this via a.Directed().
var shapes = []shapegen.Shape[int, int64]{
	shapegen.EmptyGraph(),
	shapegen.SingleNode(),
	shapegen.SingleEdge(true, false, false), // directed K2
	shapegen.ParallelDigon(2),
	shapegen.IsolatedOnly(5),
	shapegen.Cycle(5, true),              // directed cycle
	shapegen.Complete(4, true),           // directed K4
	shapegen.CompleteBipartite(3, 3),     // undirected (forces cfg.Directed=false)
	shapegen.BarabasiAlbert(50, 2, 42),   // undirected (forces cfg.Directed=false)
	shapegen.WattsStrogatz(20, 4, 30, 0), // undirected (forces cfg.Directed=false)
}

// adjlistNeighbours returns the sorted slice of NodeIDs that adj
// reports as neighbours of key. Used in both the rapid and
// deterministic tests to avoid repeating the range-over-Neighbours
// pattern.
func adjlistNeighbours(a *adjlist.AdjList[int, int64], key int) []graph.NodeID {
	var ids []graph.NodeID
	for nb := range a.Neighbours(key) {
		if vID, ok := a.Mapper().Lookup(nb); ok {
			ids = append(ids, vID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// csrNeighbours returns the sorted slice of NodeIDs that c reports as
// neighbours of id.
func csrNeighbours(c *csr.CSR[int64], id graph.NodeID) []graph.NodeID {
	var ids []graph.NodeID
	for nb := range c.NeighboursByID(id) {
		ids = append(ids, nb)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// expectedCSRSize returns the number of directed arcs stored in the
// CSR built from a. For a directed AdjList, Size() already counts
// directed arcs. For an undirected AdjList, Size() counts undirected
// edges once, but BuildFromAdjList stores both (u,v) and (v,u), so
// the CSR's arc count is 2*Size().
func expectedCSRSize(a *adjlist.AdjList[int, int64]) uint64 {
	if a.Directed() {
		return a.Size()
	}
	return 2 * a.Size()
}

// checkEdgePreservation asserts that for every node in a, the CSR
// stores exactly the same multiset of neighbour NodeIDs. It is
// shared between the rapid and deterministic tests.
func checkEdgePreservation(t interface {
	Errorf(format string, args ...any)
}, shapeName string, a *adjlist.AdjList[int, int64], c *csr.CSR[int64]) {
	a.Mapper().Walk(func(id graph.NodeID, key int) bool {
		adj := adjlistNeighbours(a, key)
		got := csrNeighbours(c, id)
		if !reflect.DeepEqual(adj, got) {
			t.Errorf("shape=%s node key=%d id=%d: adjlist=%v csr=%v",
				shapeName, key, id, adj, got)
		}
		return true
	})
}

// TestCSR_BuildFromAdjList_PreservesEdges_Rapid uses rapid.Check to
// verify that BuildFromAdjList preserves every edge (as a sorted
// multiset of NodeIDs) across the full representative shape set.
func TestCSR_BuildFromAdjList_PreservesEdges_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		idx := rapid.IntRange(0, len(shapes)-1).Draw(rt, "shape_idx")
		shape := shapes[idx]

		g, err := shape.Build(adjlist.Config{Directed: true})
		if err != nil {
			rt.Fatalf("Build(%s): %v", shape.Name(), err)
		}
		a := g.AdjList()
		c := csr.BuildFromAdjList(a)

		if c.Order() != a.Order() {
			rt.Errorf("shape=%s Order mismatch: csr=%d adjlist=%d",
				shape.Name(), c.Order(), a.Order())
		}
		wantSize := expectedCSRSize(a)
		if c.Size() != wantSize {
			rt.Errorf("shape=%s Size mismatch: csr=%d want=%d (adjlist.Size=%d directed=%v)",
				shape.Name(), c.Size(), wantSize, a.Size(), a.Directed())
		}

		checkEdgePreservation(rt, shape.Name(), a, c)
	})
}

// TestCSR_BuildFromAdjList_PreservesEdges_Shapes is a deterministic
// table-driven version for fast feedback. Each sub-test builds one
// shape, constructs the CSR, and verifies Order, Size, and per-node
// edge preservation.
func TestCSR_BuildFromAdjList_PreservesEdges_Shapes(t *testing.T) {
	t.Parallel()
	for _, shape := range shapes {
		shape := shape
		t.Run(shape.Name(), func(t *testing.T) {
			t.Parallel()
			g, err := shape.Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)

			if c.Order() != a.Order() {
				t.Errorf("Order mismatch: csr=%d adjlist=%d", c.Order(), a.Order())
			}
			wantSize := expectedCSRSize(a)
			if c.Size() != wantSize {
				t.Errorf("Size mismatch: csr=%d want=%d (adjlist.Size=%d directed=%v)",
					c.Size(), wantSize, a.Size(), a.Directed())
			}

			checkEdgePreservation(t, shape.Name(), a, c)
		})
	}
}
