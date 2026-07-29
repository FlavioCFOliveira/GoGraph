//go:build soak

package csrorder

import (
	"fmt"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// distribution_soak_test.go — reproduction of the §2.4 leverage table.
//
// docs/design-degree-adaptive-adjacency.md §2.4 publishes the costFrac figures
// that decide how much a threshold is worth, and §12 records that the harness
// behind them was a temporary test, removed after use. That is the same defect
// that refutes §2.4's own probe column: a load-bearing measurement that cannot be
// reproduced is not evidence. This test reproduces the table's rows at their
// ORIGINAL parameters and pins their costFrac values, so the surviving half of
// §2.4 is verifiable rather than inherited on trust.
//
// It is soak-layer because the Barabási–Albert generator is O(n²) by construction
// and the n=100k row costs minutes on its own — far outside the short layer's
// 60 s per-package budget. Run with:
//
//	go test -tags=soak -run TestDistribution_Reproduces24Table -v ./bench/csrorder/...

// tolerance is the absolute margin allowed on a reproduced costFrac.
//
// The generators are deterministic in (parameters, seed), but §2.4 did not record
// the seed it used, so a reproduction can only be expected to land on the same
// distribution rather than the identical graph. 1.5 percentage points is tight
// enough that a genuine regression in a generator would fail, and loose enough
// that an unrecorded seed does not.
const tolerance = 0.015

// row is one published §2.4 measurement to reproduce.
type row struct {
	name string
	// build produces the fixture's adjacency.
	build func() (*adjlist.AdjList[int, int64], error)
	// wantAvgOut and wantMaxOut are §2.4's reported degree summary.
	//
	// wantAvgOut is compared against [DegreeProfile.MeanDegreeAllNodes], not
	// against MeanDegree. §2.4's "avg out" column divides arcs by EVERY interned
	// node, including nodes with no out-arc at all. The distinction is invisible
	// on the Barabási–Albert rows, where the generator is undirected so every node
	// carries at least m0 arcs and the two means agree exactly, and it is a 1.62x
	// difference on the RMAT row, where 25 149 of 65 536 nodes end up with no
	// out-arc (14.58 all-nodes against 23.66 over arc-bearing sources). Checking
	// the wrong mean makes a faithful reproduction look like a generator
	// regression, which is how this was found.
	wantAvgOut float64
	wantMaxOut int
	// wantCostFrac maps threshold to the published costFrac.
	wantCostFrac map[int]float64
}

// published is §2.4's table, transcribed. These are the figures that SURVIVED the
// refutation: §2.4's probe-cost column (0.659 vs 1.865 ns at degree 8; 164 vs 5.31
// ns at degree 4096) is refuted and appears nowhere here, but its leverage table
// was measured against this repository's own generators and is reproducible.
var published = []row{
	{
		name: "Barabasi-Albert n=100k m=8",
		build: func() (*adjlist.AdjList[int, int64], error) {
			return buildIntAdj(shapegen.BarabasiAlbert(100_000, 8, 1))
		},
		wantAvgOut:   15.98,
		wantMaxOut:   1612,
		wantCostFrac: map[int]float64{16: 0.8921, 32: 0.7870, 64: 0.6718, 128: 0.5571},
	},
	{
		name: "Barabasi-Albert n=50k m=4",
		build: func() (*adjlist.AdjList[int, int64], error) {
			return buildIntAdj(shapegen.BarabasiAlbert(50_000, 4, 1))
		},
		wantAvgOut:   7.97,
		wantMaxOut:   679,
		wantCostFrac: map[int]float64{16: 0.8046, 32: 0.6933, 64: 0.5769, 128: 0.4618},
	},
	{
		name: "RMAT scale=16 ef=16",
		build: func() (*adjlist.AdjList[int, int64], error) {
			return buildIntAdj(shapegen.RMAT(16, 16, 57, 19, 19, 5, 1))
		},
		wantAvgOut:   14.58,
		wantMaxOut:   6215,
		wantCostFrac: map[int]float64{16: 0.9968, 32: 0.9947, 64: 0.9778, 128: 0.9341},
	},
}

// buildIntAdj builds a generator shape and returns its adjacency.
func buildIntAdj(s shapegen.Shape[int, int64]) (*adjlist.AdjList[int, int64], error) {
	g, err := s.Build(adjlist.Config{})
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", s.Name(), err)
	}
	return g.AdjList(), nil
}

// TestDistribution_Reproduces24Table re-measures §2.4's leverage table.
func TestDistribution_Reproduces24Table(t *testing.T) {
	for _, r := range published {
		t.Run(r.name, func(t *testing.T) {
			adj, err := r.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			// Degree summary first, so a generator whose SHAPE drifted is reported
			// as such rather than only as a costFrac miss.
			base := ProfileDegrees(adj, 16)
			t.Logf("%s: %s", r.name, base)
			if math.Abs(base.MeanDegreeAllNodes-r.wantAvgOut) > 0.5 {
				t.Errorf("mean out-degree over all nodes: got %.2f, §2.4 published %.2f",
					base.MeanDegreeAllNodes, r.wantAvgOut)
			}
			// The maximum is the single most seed-sensitive statistic in a
			// preferential-attachment graph, so it is checked only for order of
			// magnitude — a 2x band. A drift beyond that means the tail changed.
			if ratio := float64(base.MaxDegree) / float64(r.wantMaxOut); ratio < 0.5 || ratio > 2 {
				t.Errorf("max out-degree: got %d, §2.4 published %d (%.2fx) — the "+
					"generator's tail has changed shape", base.MaxDegree, r.wantMaxOut, ratio)
			}

			for th, want := range r.wantCostFrac {
				got := ProfileDegrees(adj, th).CostFrac
				if math.Abs(got-want) > tolerance {
					t.Errorf("T=%d costFrac: got %.4f, §2.4 published %.4f (delta %.4f > %.4f)",
						th, got, want, math.Abs(got-want), tolerance)
				}
			}
		})
	}
}

// TestDistribution_RMATVersusPowerLawGap pins the specific comparison §8 turns
// into a benchmarking rule: at T=64 RMAT reports 97.78% of scan cost above
// threshold against Barabási–Albert's 67.18%. That ~30-point gap is why an
// RMAT-only benchmark "will look like a triumph and then fail to reproduce".
func TestDistribution_RMATVersusPowerLawGap(t *testing.T) {
	baAdj, err := buildIntAdj(shapegen.BarabasiAlbert(100_000, 8, 1))
	if err != nil {
		t.Fatalf("barabasi-albert: %v", err)
	}
	rmatAdj, err := buildIntAdj(shapegen.RMAT(16, 16, 57, 19, 19, 5, 1))
	if err != nil {
		t.Fatalf("rmat: %v", err)
	}

	ba := ProfileDegrees(baAdj, 64).CostFrac
	rm := ProfileDegrees(rmatAdj, 64).CostFrac
	gap := rm - ba
	t.Logf("T=64 costFrac — barabasi-albert %.2f%%, rmat %.2f%%, gap %.2f points",
		100*ba, 100*rm, 100*gap)

	const publishedGap = 0.9778 - 0.6718
	if math.Abs(gap-publishedGap) > 2*tolerance {
		t.Errorf("gap: got %.4f, §2.4 implies %.4f", gap, publishedGap)
	}
}
