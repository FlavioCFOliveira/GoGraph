package search

// Task 669: BidirectionalDijkstra on a deterministic 1000-node graph.
//
// Builds a directed graph with random non-negative float64 weights
// using a seeded PCG PRNG, then runs both BidirectionalDijkstra and
// Dijkstra for 50 deterministic (src, dst) pairs and asserts that
// the distances agree within 1e-12.

import (
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBidirectionalDijkstra_DeterministicGraph verifies that
// BidirectionalDijkstra and Dijkstra agree on a seeded random
// directed float64-weight graph with 1000 nodes and ~5000 edges.
func TestBidirectionalDijkstra_DeterministicGraph(t *testing.T) {
	t.Parallel()

	const (
		numNodes  = 1000
		numEdges  = 5000
		numPairs  = 50
		graphSeed = 0xDEAD_BEEF_1234_5678
		querySeed = 0xCAFE_BABE_ABCD_EF01
	)

	// Build graph with non-negative float64 weights.
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for i := 0; i < numNodes; i++ {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	gr := rand.New(rand.NewPCG(graphSeed, graphSeed^0xFF)) //nolint:gosec // deterministic test RNG
	for i := 0; i < numEdges; i++ {
		from := gr.IntN(numNodes)
		to := gr.IntN(numNodes)
		w := gr.Float64() * 100.0 // [0, 100)
		if err := a.AddEdge(from, to, w); err != nil {
			t.Fatalf("AddEdge(%d→%d): %v", from, to, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	rev := c.BuildReverse()

	// Generate 50 deterministic (src, dst) pairs.
	qr := rand.New(rand.NewPCG(querySeed, querySeed^0xFF)) //nolint:gosec // deterministic test RNG
	for i := 0; i < numPairs; i++ {
		srcKey := qr.IntN(numNodes)
		dstKey := qr.IntN(numNodes)
		srcID, ok1 := a.Mapper().Lookup(srcKey)
		dstID, ok2 := a.Mapper().Lookup(dstKey)
		if !ok1 || !ok2 {
			continue
		}

		dij, err := Dijkstra(c, srcID)
		if err != nil {
			t.Fatalf("pair %d: Dijkstra: %v", i, err)
		}
		dijDist, dijOK := dij.Distance(dstID)

		_, biCost, biErr := BidirectionalDijkstraOn(c, rev, srcID, dstID)

		switch {
		case dijOK && biErr != nil:
			t.Errorf("pair %d (src=%d dst=%d): Dijkstra found dist=%g but BiDijkstra returned %v",
				i, srcKey, dstKey, dijDist, biErr)
		case !dijOK && biErr == nil:
			t.Errorf("pair %d (src=%d dst=%d): Dijkstra: unreachable but BiDijkstra returned cost=%g",
				i, srcKey, dstKey, biCost)
		case dijOK && biErr == nil:
			diff := biCost - dijDist
			if diff < 0 {
				diff = -diff
			}
			if diff > 1e-12 {
				t.Errorf("pair %d (src=%d dst=%d): BiDijkstra=%g Dijkstra=%g diff=%g > 1e-12",
					i, srcKey, dstKey, biCost, dijDist, diff)
			}
		}
	}
}
