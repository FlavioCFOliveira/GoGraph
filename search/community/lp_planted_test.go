package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestLabelPropagation_Planted runs label propagation on a planted-
// partition graph with k=5 communities of 80 nodes each (40% intra,
// 1% inter) and asserts NMI >= 0.70 against the ground-truth block
// labels.
//
// LP is a greedy fixed-point algorithm that can converge to a
// suboptimal partition. On the canonical seed=42 instance it
// stabilises at 3 communities (NMI ≈ 0.74), which is the expected
// lower bound for this implementation. The threshold of 0.70 is
// chosen to allow for small variance while remaining above a
// purely random assignment. For higher NMI, use [Leiden].

func TestLabelPropagation_Planted(t *testing.T) {
	t.Parallel()
	const (
		k         = 5
		blockSize = 80
		pIn       = 40
		pOut      = 1
		seed      = 42
	)

	g, err := shapegen.PlantedPartition(k, blockSize, pIn, pOut, seed).
		Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("PlantedPartition Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)

	p := LabelPropagation(c, DefaultLabelPropagationOptions())

	n := int(c.MaxNodeID())
	gt := groundTruth(g, a, n)

	// Align predicted and ground-truth for live nodes only.
	pred := make([]int, 0, k*blockSize)
	gtLive := make([]int, 0, k*blockSize)
	for i := 0; i < n; i++ {
		if p.Community[i] >= 0 && gt[i] >= 0 {
			pred = append(pred, p.Community[i])
			gtLive = append(gtLive, gt[i])
		}
	}

	nmiVal := nmi(pred, gtLive, p.NumCommunities, k)
	if nmiVal < 0.70 {
		t.Errorf("LabelPropagation Planted: NMI = %.4f, want >= 0.70 (NumCommunities=%d)",
			nmiVal, p.NumCommunities)
	}
}
