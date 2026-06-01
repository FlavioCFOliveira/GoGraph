package centrality

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestPageRank_Hypercube runs PageRank on the 8-dimensional hypercube
// (256 vertices, d-regular undirected graph) and verifies that the
// stationary distribution is uniform to high precision.
func TestPageRank_Hypercube(t *testing.T) {
	t.Parallel()

	g, err := shapegen.Hypercube(8).Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("Hypercube.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	opts := PageRankOptions{
		Damping:       0.85,
		MaxIterations: 500,
		Tolerance:     1e-12,
	}
	ranks, iters, err := PageRank(c, opts)
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}

	liveMask := c.LiveMask()

	// Collect live-node ranks (exclude ghost slots from min/max).
	var minR, maxR float64
	minR = math.MaxFloat64
	maxR = -math.MaxFloat64
	var totalMass float64
	for i, r := range ranks {
		totalMass += r
		if i < len(liveMask) && liveMask[i] {
			if r < minR {
				minR = r
			}
			if r > maxR {
				maxR = r
			}
		}
	}

	// 1. Uniform distribution: max - min <= 1e-10 over live nodes.
	if maxR-minR > 1e-10 {
		t.Fatalf("rank spread = %.3e (max=%.15f min=%.15f), want <= 1e-10", maxR-minR, maxR, minR)
	}

	// 2. Mass conservation within 1e-12.
	if math.Abs(totalMass-1.0) > 1e-12 {
		t.Fatalf("mass sum = %.15f, want 1.0 (delta %.3g)", totalMass, math.Abs(totalMass-1.0))
	}

	// 3. Convergence within 50 iterations.
	if iters > 50 {
		t.Fatalf("iterations = %d, want <= 50", iters)
	}
}
