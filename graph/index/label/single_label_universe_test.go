package label_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestSingleLabelUniverse_AddRange verifies that AddRange over [0, n-1]
// produces the full universe of n nodes under a single label, that the
// operation is idempotent, and that a subsequent RemoveRange over the same
// range leaves the label empty.
func TestSingleLabelUniverse_AddRange(t *testing.T) {
	t.Parallel()
	const n = 100_000

	idx := label.NewIndex()

	// Populate the full range.
	idx.AddRange(0, 0, graph.NodeID(n-1))

	got := idx.Scan(0)
	if len(got) != n {
		t.Fatalf("Scan(0) length = %d, want %d", len(got), n)
	}
	for j, node := range got {
		if node != graph.NodeID(j) {
			t.Fatalf("Scan(0)[%d] = %d, want %d", j, node, j)
		}
	}

	if c := idx.Count(0); c != n {
		t.Fatalf("Count(0) = %d, want %d", c, n)
	}

	// Idempotence: second AddRange must not change the result.
	idx.AddRange(0, 0, graph.NodeID(n-1))
	if c := idx.Count(0); c != n {
		t.Fatalf("Count(0) after idempotent AddRange = %d, want %d", c, n)
	}

	// RemoveRange clears the full range.
	idx.RemoveRange(0, 0, graph.NodeID(n-1))
	if c := idx.Count(0); c != 0 {
		t.Fatalf("Count(0) after RemoveRange = %d, want 0", c)
	}
	if s := idx.Scan(0); len(s) != 0 {
		t.Fatalf("Scan(0) after RemoveRange length = %d, want 0", len(s))
	}
}
