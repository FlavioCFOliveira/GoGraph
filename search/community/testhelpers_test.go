package community

import (
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nmi computes the Normalised Mutual Information between two
// NodeID-indexed community label slices. k1 and k2 are the numbers
// of distinct communities in labels1 and labels2 respectively.
// Slots with a negative label are treated as absent and excluded from
// the computation.
func nmi(labels1, labels2 []int, k1, k2 int) float64 {
	n := len(labels1)
	// Joint frequency table.
	joint := make([][]float64, k1)
	for i := range joint {
		joint[i] = make([]float64, k2)
	}
	counted := 0
	for i := 0; i < n; i++ {
		if labels1[i] >= 0 && labels2[i] >= 0 {
			joint[labels1[i]][labels2[i]]++
			counted++
		}
	}
	if counted == 0 {
		return 1.0
	}
	total := float64(counted)
	for i := range joint {
		for j := range joint[i] {
			joint[i][j] /= total
		}
	}
	// Marginals.
	px := make([]float64, k1)
	py := make([]float64, k2)
	for i := range joint {
		for j := range joint[i] {
			px[i] += joint[i][j]
			py[j] += joint[i][j]
		}
	}
	// Entropies and mutual information.
	hx, hy, ixy := 0.0, 0.0, 0.0
	for _, p := range px {
		if p > 0 {
			hx -= p * math.Log(p)
		}
	}
	for _, p := range py {
		if p > 0 {
			hy -= p * math.Log(p)
		}
	}
	for i := range joint {
		for j := range joint[i] {
			pij := joint[i][j]
			if pij > 0 && px[i] > 0 && py[j] > 0 {
				ixy += pij * math.Log(pij/(px[i]*py[j]))
			}
		}
	}
	if hx+hy == 0 {
		return 1.0
	}
	return 2 * ixy / (hx + hy)
}

// groundTruth extracts the "block_id" property from every node in a
// PlantedPartition or LFR graph and returns a NodeID-indexed slice
// aligned with the CSR's MaxNodeID(). Slots that carry no mapping
// (ghost NodeIDs created by shard packing) remain -1.
func groundTruth(g *lpg.Graph[int, int64], a *adjlist.AdjList[int, int64], n int) []int {
	gt := make([]int, n)
	for i := range gt {
		gt[i] = -1
	}
	a.Mapper().Walk(func(id graph.NodeID, key int) bool {
		prop, ok := g.GetNodeProperty(key, "block_id")
		if ok {
			v, ok2 := prop.Int64()
			if ok2 {
				gt[id] = int(v)
			}
		}
		return true
	})
	return gt
}
