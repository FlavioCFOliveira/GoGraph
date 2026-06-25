package community

// weight_contract_test.go — regression gate for the 2026-06-25 reliability
// audit finding #1758. Leiden and LabelPropagation deliberately treat every
// edge as unit-weight (1.0) regardless of the CSR weight type W (see their
// godoc). This gate pins that contract: a graph with non-uniform edge weights
// and its unit-weight twin (identical topology) MUST produce the SAME
// partition. If a future change makes either algorithm weight-aware without a
// dedicated weighted entry point, this fails loudly — the documented contract
// changed silently.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// fourCycle builds the undirected 4-cycle 0-1-2-3-0 as a directed CSR (both
// directions present), with the supplied per-directed-edge weights in the
// fixed edge order: 0->1, 0->3, 1->0, 1->2, 2->1, 2->3, 3->2, 3->0.
func fourCycle(w [8]float64) *csr.CSR[float64] {
	vertices := []uint64{0, 2, 4, 6, 8}
	edges := []graph.NodeID{1, 3, 0, 2, 1, 3, 2, 0}
	weights := w[:]
	return csr.FromArrays(vertices, edges, weights, 4, 8)
}

func samePartition(a, b Partition) bool {
	if a.NumCommunities != b.NumCommunities || len(a.Community) != len(b.Community) {
		return false
	}
	for i := range a.Community {
		if a.Community[i] != b.Community[i] {
			return false
		}
	}
	return true
}

func TestLeiden_IgnoresEdgeWeights(t *testing.T) {
	t.Parallel()
	// Heavy 0-1 and 2-3, light 1-2 and 3-0: if weights were honoured this
	// would bias toward {0,1},{2,3}. Twin has all unit weights.
	weighted := fourCycle([8]float64{100, 1, 100, 1, 1, 100, 100, 1})
	unit := fourCycle([8]float64{1, 1, 1, 1, 1, 1, 1, 1})

	pw := Leiden(weighted, DefaultLeidenOptions())
	pu := Leiden(unit, DefaultLeidenOptions())
	if !samePartition(pw, pu) {
		t.Fatalf("Leiden honoured edge weights (contract #1758 broken):\n weighted=%v\n unit    =%v", pw.Community, pu.Community)
	}
}

func TestLabelPropagation_IgnoresEdgeWeights(t *testing.T) {
	t.Parallel()
	weighted := fourCycle([8]float64{100, 1, 100, 1, 1, 100, 100, 1})
	unit := fourCycle([8]float64{1, 1, 1, 1, 1, 1, 1, 1})

	pw := LabelPropagation(weighted, DefaultLabelPropagationOptions())
	pu := LabelPropagation(unit, DefaultLabelPropagationOptions())
	if !samePartition(pw, pu) {
		t.Fatalf("LabelPropagation honoured edge weights (contract #1758 broken):\n weighted=%v\n unit    =%v", pw.Community, pu.Community)
	}
}
