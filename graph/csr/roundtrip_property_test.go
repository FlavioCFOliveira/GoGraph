package csr_test

import (
	"sort"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// roundtripShapes is the representative set for the round-trip rapid test.
var roundtripShapes = []shapegen.Shape[int, int64]{
	shapegen.EmptyGraph(),
	shapegen.SingleNode(),
	shapegen.SingleEdge(true, false, false),
	shapegen.SingleEdge(false, false, false),
	shapegen.SingleEdge(true, false, true), // self-loop
	shapegen.ParallelDigon(3),
	shapegen.IsolatedOnly(10),
	shapegen.Cycle(7, true),
	shapegen.Cycle(7, false),
	shapegen.Complete(5, true),
	shapegen.Complete(5, false),
	shapegen.CompleteBipartite(4, 4),
	shapegen.BarabasiAlbert(100, 3, 42),
	shapegen.WattsStrogatz(30, 4, 20, 0),
	shapegen.Path(10, true),
	shapegen.Path(10, false),
}

// nodeEdge is a directed node-pair used in round-trip comparison.
type nodeEdge struct{ u, v graph.NodeID }

// collectAdjlistEdges enumerates every directed arc stored in a and
// returns them as a sorted slice of nodeEdge pairs. The sort order is
// (u ascending, v ascending), matching the sort applied to CSR edges
// so both slices are directly comparable.
func collectAdjlistEdges(a *adjlist.AdjList[int, int64]) []nodeEdge {
	var edges []nodeEdge
	a.Mapper().Walk(func(id graph.NodeID, key int) bool {
		for nb := range a.Neighbours(key) {
			vID, ok := a.Mapper().Lookup(nb)
			if ok {
				edges = append(edges, nodeEdge{id, vID})
			}
		}
		return true
	})
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].u != edges[j].u {
			return edges[i].u < edges[j].u
		}
		return edges[i].v < edges[j].v
	})
	return edges
}

// collectCSREdges enumerates every directed arc in c and returns them
// as a sorted slice of nodeEdge pairs (u ascending, v ascending).
// This helper is also used by build_order_property_test.go.
func collectCSREdges(c *csr.CSR[int64]) []nodeEdge {
	var edges []nodeEdge
	maxID := c.MaxNodeID()
	for src := graph.NodeID(0); src < maxID; src++ {
		for dst := range c.NeighboursByID(src) {
			edges = append(edges, nodeEdge{src, dst})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].u != edges[j].u {
			return edges[i].u < edges[j].u
		}
		return edges[i].v < edges[j].v
	})
	return edges
}

// TestCSR_RoundTrip_PreservesEdges_10kIterations uses rapid.Check to
// verify that, for any shape drawn from roundtripShapes, building a
// CSR from the adjlist and projecting it back to (u, v) pairs yields
// an edge multiset identical to the original adjlist's.
func TestCSR_RoundTrip_PreservesEdges_10kIterations(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		idx := rapid.IntRange(0, len(roundtripShapes)-1).Draw(rt, "shape")
		shape := roundtripShapes[idx]

		g, err := shape.Build(adjlist.Config{Directed: true})
		if err != nil {
			rt.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		c := csr.BuildFromAdjList(a)

		adjEdges := collectAdjlistEdges(a)
		csrEdges := collectCSREdges(c)

		if len(adjEdges) != len(csrEdges) {
			rt.Errorf("shape=%s edge count mismatch: adjlist=%d csr=%d",
				shape.Name(), len(adjEdges), len(csrEdges))
			return
		}
		for i := range adjEdges {
			if adjEdges[i] != csrEdges[i] {
				rt.Errorf("shape=%s edge[%d]: adjlist=(%d,%d) csr=(%d,%d)",
					shape.Name(), i,
					adjEdges[i].u, adjEdges[i].v,
					csrEdges[i].u, csrEdges[i].v)
			}
		}
	})
}
