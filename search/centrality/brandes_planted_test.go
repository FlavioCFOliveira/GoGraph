package centrality

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestBetweenness_PlantedBoundary asserts that, on a planted-partition
// graph, vertices that sit on the block boundary (at least one
// neighbour in a different block) carry on average at least twice the
// betweenness of purely internal vertices.
//
// Parameters produce a dense intra-block structure (pInPercent=50)
// with sparse inter-block wiring (pOutPercent=2), making boundary
// vertices the clear structural bottlenecks for cross-community paths.
func TestBetweenness_PlantedBoundary(t *testing.T) {
	t.Parallel()

	const (
		k           = 4
		blockSize   = 50
		pInPercent  = 50
		pOutPercent = 2
		seed        = 42
		minBCRatio  = 2.0 // mean(boundary) >= 2 * mean(internal)
	)

	g, err := shapegen.PlantedPartition(k, blockSize, pInPercent, pOutPercent, seed).
		Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("PlantedPartition.Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)

	type classified struct {
		nodeID     graph.NodeID
		blockID    int64
		isBoundary bool
	}
	all := make([]classified, 0, k*blockSize)

	// Walk iterates all interned keys. For each node, retrieve its
	// block_id property and decide whether any neighbour belongs to a
	// different block (boundary) or all neighbours share the same
	// block (internal).
	a.Mapper().Walk(func(nid graph.NodeID, key int) bool {
		prop, ok := g.GetNodeProperty(key, "block_id")
		if !ok {
			t.Errorf("node key %d has no block_id property", key)
			return true
		}
		bid, _ := prop.Int64()

		boundary := false
		for nbr := range a.Neighbours(key) {
			nbrProp, nbrOK := g.GetNodeProperty(nbr, "block_id")
			if !nbrOK {
				continue
			}
			nbrBid, _ := nbrProp.Int64()
			if nbrBid != bid {
				boundary = true
				break
			}
		}

		all = append(all, classified{
			nodeID:     nid,
			blockID:    bid,
			isBoundary: boundary,
		})
		return true
	})

	if len(all) == 0 {
		t.Fatalf("no nodes found after Walk")
	}

	var (
		boundarySum float64
		boundaryN   int
		internalSum float64
		internalN   int
	)
	for _, cl := range all {
		v := bc[uint64(cl.nodeID)]
		if cl.isBoundary {
			boundarySum += v
			boundaryN++
		} else {
			internalSum += v
			internalN++
		}
	}

	if boundaryN == 0 {
		t.Fatalf("no boundary vertices found (pOutPercent=%d may be too low)", pOutPercent)
	}
	if internalN == 0 {
		t.Fatalf("no internal vertices found (all nodes are boundary, pOutPercent is too high)")
	}

	meanBoundary := boundarySum / float64(boundaryN)
	meanInternal := internalSum / float64(internalN)

	// When meanInternal is zero (perfectly isolated cliques the
	// cross-cluster edges only touch boundary nodes) any positive
	// meanBoundary already satisfies the dominance criterion.
	if meanInternal > 0 && meanBoundary < minBCRatio*meanInternal {
		t.Fatalf(
			"boundary betweenness not dominant: mean(boundary)=%.4f mean(internal)=%.4f ratio=%.4f want >= %.1f",
			meanBoundary, meanInternal, meanBoundary/meanInternal, minBCRatio,
		)
	}
	if meanInternal == 0 && meanBoundary == 0 {
		t.Fatalf("all betweenness values are zero — graph may be trivially structured")
	}
}
