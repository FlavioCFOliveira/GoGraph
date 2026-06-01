package search

// Task 639: BellmanFord on acyclic graphs with negative weights.
//
// Uses shapegen.NegativeWeightAcyclic to produce DAGs with mixed-sign
// edge weights (no negative cycles). BellmanFord must:
//   - not return ErrNegativeCycle
//   - produce distances satisfying the triangle inequality:
//     for every edge u→v with weight w, dist[v] ≤ dist[u] + w

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestBellmanFord_AcyclicNegWeights runs BellmanFord on DAGs with
// mixed-sign weights produced by NegativeWeightAcyclic and verifies
// that no ErrNegativeCycle is returned and the triangle inequality
// holds for every edge.
func TestBellmanFord_AcyclicNegWeights(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n       int
		signMix int
		seed    uint64
	}{
		{n: 8, signMix: 50, seed: 0xABCD},
		{n: 64, signMix: 30, seed: 0x1234},
		{n: 64, signMix: 70, seed: 0x5678},
	}
	for _, tc := range cases {
		tc := tc
		name := fmt.Sprintf("n%d_mix%d", tc.n, tc.signMix)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.NegativeWeightAcyclic(tc.n, tc.signMix, tc.seed).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			srcID, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("key 0 not in mapper")
			}

			d, bfErr := BellmanFord(c, srcID)
			if bfErr != nil {
				t.Fatalf("BellmanFord: unexpected error %v", bfErr)
			}

			// Triangle inequality: for every edge u→v with weight w,
			// dist[v] ≤ dist[u] + w (only checked when both u and v are
			// reachable from src).
			verts := c.VerticesSlice()
			edges := c.EdgesSlice()
			weights := c.WeightsSlice()
			maxID := c.MaxNodeID()

			for u := graph.NodeID(0); u < maxID; u++ {
				distU, reachU := d.Distance(u)
				if !reachU {
					continue
				}
				start := verts[uint64(u)]
				end := verts[uint64(u)+1]
				for k := start; k < end; k++ {
					v := edges[k]
					w := weights[k]
					distV, reachV := d.Distance(v)
					if !reachV {
						continue
					}
					// Use integer arithmetic to avoid float64 rounding — the
					// weights are int64 so there is no precision loss.
					if distV > distU+w {
						t.Errorf(
							"triangle inequality violated: dist[%d]=%d > dist[%d]=%d + w=%d",
							v, distV, u, distU, w,
						)
					}
				}
			}
		})
	}
}
