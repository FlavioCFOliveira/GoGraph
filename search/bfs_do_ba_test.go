package search

// Task 577: BFSDirectionOpt on a Barabási-Albert graph.
//
// Verifies two properties on a BA(n=10000, m0=4, seed=42) power-law graph:
//
//  1. Distance equivalence: the distance map produced by BFSDirectionOpt
//     is identical to the one produced by plain BFS for every vertex.
//
//  2. Direction switch: at least one bottom-up step was triggered, confirming
//     that the Beamer alpha/beta regime fired on this power-law topology.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

func TestBFSDirectionOpt_BA_MatchesBFSAndSwitches(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(10_000, 4, 42).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)

	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}

	// Reference distance map from plain BFS.
	refDist := make(map[graph.NodeID]int, 10_000)
	BFS(c, srcID, func(node graph.NodeID, d int) bool {
		refDist[node] = d
		return true
	})

	// BFS-DO distance map.
	doDist := make(map[graph.NodeID]int, 10_000)
	var sawBottomUp bool
	obs := func(_ int, isBottomUp bool) {
		if isBottomUp {
			sawBottomUp = true
		}
	}
	_ = bfsDoCore(context.Background(), c, srcID, func(node graph.NodeID, d int) bool {
		doDist[node] = d
		return true
	}, obs)

	// Check direction switch.
	if !sawBottomUp {
		t.Error("BFSDirectionOpt never triggered a bottom-up step on BA(10000,4,42)")
	}

	// Check visited set size.
	if len(doDist) != len(refDist) {
		t.Fatalf("BFS-DO visited %d nodes, plain BFS visited %d", len(doDist), len(refDist))
	}

	// Check that every distance is identical.
	for node, wantD := range refDist {
		gotD, found := doDist[node]
		if !found {
			v, _ := a.Mapper().Resolve(node)
			t.Errorf("BFS-DO did not visit node key=%d (NodeID=%d)", v, node)
			continue
		}
		if gotD != wantD {
			v, _ := a.Mapper().Resolve(node)
			t.Errorf("dist[key=%d] BFS-DO=%d, plain BFS=%d", v, gotD, wantD)
		}
	}
}
