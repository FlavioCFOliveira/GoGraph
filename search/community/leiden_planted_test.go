package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestLeiden_Planted runs Leiden on a planted-partition graph with
// k=5 communities of 80 nodes each (40% intra, 1% inter) and asserts
// NMI >= 0.95 against the ground-truth block labels. Also verifies
// bit-for-bit determinism across two independent runs.
func TestLeiden_Planted(t *testing.T) {
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

	p := Leiden(c, DefaultLeidenOptions())

	n := int(c.MaxNodeID())
	gt := groundTruth(g, a, n)

	// Align predicted and ground-truth labels for live nodes only.
	pred := make([]int, 0, k*blockSize)
	gtLive := make([]int, 0, k*blockSize)
	for i := 0; i < n; i++ {
		if p.Community[i] >= 0 && gt[i] >= 0 {
			pred = append(pred, p.Community[i])
			gtLive = append(gtLive, gt[i])
		}
	}

	nmiVal := nmi(pred, gtLive, p.NumCommunities, k)
	if nmiVal < 0.95 {
		t.Errorf("Leiden Planted: NMI = %.4f, want >= 0.95 (NumCommunities=%d)",
			nmiVal, p.NumCommunities)
	}

	// Determinism.
	p2 := Leiden(c, DefaultLeidenOptions())
	for i, v := range p.Community {
		if p2.Community[i] != v {
			t.Fatalf("non-deterministic: Community[%d] = %d vs %d on second run", i, v, p2.Community[i])
		}
	}
}
