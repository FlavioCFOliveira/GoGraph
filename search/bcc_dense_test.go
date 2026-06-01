package search

// Task 779: BCC on dense single-block graphs — exactly one BCC, no bridges,
// no articulation points.
//
// Two fixtures are tested:
//  1. Undirected K_8 (complete graph, 8 nodes): single BCC.
//  2. Petersen graph (10 nodes, undirected): single BCC.
//
// Both graphs are 2-edge-connected and 2-vertex-connected, so neither has
// bridges nor articulation points, and the entire graph forms one BCC.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestHopcroftTarjanBCC_Dense_K8 tests BCC on an undirected K_8.
//
// K_8 is 7-connected; the whole graph is a single biconnected component.
// Expected: 1 component, 0 bridges, 0 articulation points.
func TestHopcroftTarjanBCC_Dense_K8(t *testing.T) {
	t.Parallel()

	const n = 8
	g, err := shapegen.Complete(n, false).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Complete(%d).Build: %v", n, err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	res := HopcroftTarjanBCC(c)

	if len(res.Components) != 1 {
		t.Errorf("Components: got %d, want 1", len(res.Components))
	}
	if len(res.Bridges) != 0 {
		t.Errorf("Bridges: got %d, want 0 (got %v)", len(res.Bridges), res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Errorf("Articulation: got %d, want 0 (got %v)", len(res.Articulation), res.Articulation)
	}
}

// TestHopcroftTarjanBCC_Dense_Petersen tests BCC on the Petersen graph.
//
// The Petersen graph is 3-regular, 3-connected, and bridgeless; the whole
// graph forms a single biconnected component.
// Expected: 1 component, 0 bridges, 0 articulation points.
func TestHopcroftTarjanBCC_Dense_Petersen(t *testing.T) {
	t.Parallel()

	g, err := shapegen.Petersen().Build(defaultCfg())
	if err != nil {
		t.Fatalf("Petersen().Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	res := HopcroftTarjanBCC(c)

	if len(res.Components) != 1 {
		t.Errorf("Components: got %d, want 1", len(res.Components))
	}
	if len(res.Bridges) != 0 {
		t.Errorf("Bridges: got %d, want 0 (got %v)", len(res.Bridges), res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Errorf("Articulation: got %d, want 0 (got %v)", len(res.Articulation), res.Articulation)
	}
}
