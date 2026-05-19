package search

import (
	"errors"
	"testing"
)

func TestJohnson_MatchesFloydWarshall(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	apspJ, err := JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("JohnsonAPSP: %v", err)
	}
	apspF := FloydWarshall(c)
	if apspJ.N() != apspF.N() {
		t.Fatalf("size mismatch")
	}
	for i := 0; i < apspJ.N(); i++ {
		for j := 0; j < apspJ.N(); j++ {
			vJ, okJ := apspJ.At(uint64ToNodeID(i), uint64ToNodeID(j))
			vF, okF := apspF.At(uint64ToNodeID(i), uint64ToNodeID(j))
			if okJ != okF {
				t.Fatalf("reachability mismatch at (%d,%d)", i, j)
			}
			if okJ && vJ != vF {
				t.Fatalf("(%d,%d): Johnson=%d Floyd=%d", i, j, vJ, vF)
			}
		}
	}
}

// TestDijkstraAPSP_PrimaryAPI exercises the renamed function with the
// same fixture; the alias JohnsonAPSP delegates so the existing test
// continues to validate identical behaviour.
func TestDijkstraAPSP_PrimaryAPI(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 4}, {0, 2, 1},
		{2, 1, 2}, {1, 3, 1},
		{2, 3, 5},
	})
	apsp, err := DijkstraAPSP(c)
	if err != nil {
		t.Fatalf("DijkstraAPSP: %v", err)
	}
	if apsp.N() != 4 {
		t.Fatalf("APSP.N() = %d, want 4", apsp.N())
	}
}

// TestDijkstraAPSP_NegativeEdgeRejected verifies non-negative-weight
// precondition is enforced with a typed sentinel.
func TestDijkstraAPSP_NegativeEdgeRejected(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, -1},
		{1, 2, 2},
	})
	_, err := DijkstraAPSP(c)
	if !errors.Is(err, ErrNegativeEdgeAPSP) {
		t.Fatalf("DijkstraAPSP on negative edge: err=%v want ErrNegativeEdgeAPSP", err)
	}
}
