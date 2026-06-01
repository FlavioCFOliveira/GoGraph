package label_test

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestZipfDistribution verifies the label index under a skewed label
// distribution: n nodes are assigned labels drawn from a Zipf-like
// distribution (PCG-seeded, reproducible), concentrating most membership
// in a small number of hot labels while leaving tail labels sparse.
//
// A reference map is built in parallel with the index; for 100 randomly
// sampled labels the Scan output is checked against the reference.
func TestZipfDistribution(t *testing.T) {
	t.Parallel()
	const (
		n      = 200_000
		nLabel = 10_000
		probe  = 100
	)

	//nolint:gosec // G404: math/rand/v2 is intentional — deterministic seed for test reproducibility, not security.
	rng := rand.New(rand.NewPCG(42, 0))
	idx := label.NewIndex()
	oracle := make(map[uint32][]graph.NodeID, nLabel)

	for i := 0; i < n; i++ {
		// Zipf-like: draw from a geometric-like distribution capped at
		// nLabel. Most draws land near 0; the tail thins out toward
		// nLabel-1. This is achieved by squaring a uniform draw, which
		// concentrates mass at low labels.
		u := rng.Float64()
		lbl := uint32(u * u * float64(nLabel))
		node := graph.NodeID(i)
		idx.Add(lbl, node)
		oracle[lbl] = append(oracle[lbl], node)
	}

	// Sample probe labels uniformly at random from those that actually
	// received at least one node.
	present := make([]uint32, 0, nLabel)
	for lbl := range oracle {
		present = append(present, lbl)
	}
	//nolint:gosec // G404: math/rand/v2 is intentional — deterministic seed for test reproducibility, not security.
	rng2 := rand.New(rand.NewPCG(7, 3))
	for k := 0; k < probe && len(present) > 0; k++ {
		pick := rng2.IntN(len(present))
		lbl := present[pick]

		want := oracle[lbl]
		got := idx.Scan(lbl)
		if len(got) != len(want) {
			t.Fatalf("Scan(%d) length = %d, want %d", lbl, len(got), len(want))
		}
		// Scan returns sorted NodeIDs; oracle list is insertion-order.
		// Build a set from the oracle and verify every Scan entry is present.
		set := make(map[graph.NodeID]struct{}, len(want))
		for _, node := range want {
			set[node] = struct{}{}
		}
		for _, node := range got {
			if _, ok := set[node]; !ok {
				t.Fatalf("Scan(%d) contains unexpected node %d", lbl, node)
			}
		}
	}
}
