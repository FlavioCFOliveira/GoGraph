package search

// Task 789: Bridges on C_n cycle — zero bridges for all n.
//
// An undirected cycle graph C_n is 2-edge-connected for n >= 3: every edge
// belongs to the single cycle, so removing any one edge leaves the graph
// connected. Therefore no edge is a bridge and no vertex is an articulation
// point.
//
// Tested for n in {3, 4, 16, 1024}.

import (
	"testing"

	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestBridges_Cn verifies that HopcroftTarjanBCC reports zero bridges and
// zero articulation points on undirected cycle graphs of varying sizes.
func TestBridges_Cn(t *testing.T) {
	t.Parallel()

	for _, n := range []int{3, 4, 16, 1024} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Cycle(n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Cycle(%d).Build: %v", n, err)
			}
			c := csr.BuildFromAdjList(g.AdjList())
			res := HopcroftTarjanBCC(c)

			if len(res.Bridges) != 0 {
				t.Errorf("C_%d: got %d bridges, want 0 (got %v)", n, len(res.Bridges), res.Bridges)
			}
			if len(res.Articulation) != 0 {
				t.Errorf("C_%d: got %d articulation points, want 0 (got %v)", n, len(res.Articulation), res.Articulation)
			}
		})
	}
}
