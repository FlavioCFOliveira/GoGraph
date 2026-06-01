package label_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestCardinalityExplosion_Short verifies that n distinct labels each with
// exactly one node are indexed correctly: Scan returns the single node,
// Count returns 1, and the total across all labels equals n.
func TestCardinalityExplosion_Short(t *testing.T) {
	t.Parallel()
	const n = 100_000
	cardinalityExplosion(t, n)
}

func cardinalityExplosion(t *testing.T, n int) {
	t.Helper()
	idx := label.NewIndex()
	for i := 0; i < n; i++ {
		idx.Add(uint32(i), graph.NodeID(i))
	}

	var total uint64
	for i := 0; i < n; i++ {
		lbl := uint32(i)
		node := graph.NodeID(i)

		s := idx.Scan(lbl)
		if len(s) != 1 || s[0] != node {
			t.Fatalf("Scan(%d) = %v, want [%d]", lbl, s, node)
		}
		c := idx.Count(lbl)
		if c != 1 {
			t.Fatalf("Count(%d) = %d, want 1", lbl, c)
		}
		total += c
	}

	if total != uint64(n) {
		t.Fatalf("total node count = %d, want %d", total, n)
	}
}
