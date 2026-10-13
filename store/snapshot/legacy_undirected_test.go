package snapshot

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestAssignLegacyMirrorHandles pins how a legacy undirected readback written
// without a handle column gets one (rmp #3072): the k-th arc from a to b and
// the k-th arc from b to a share a handle, whichever is stored first, and a
// self-loop has its own. Arcs: 0->1, 0->1, 0->0 | 1->0, 1->0, 1->2 | 2->1.
func TestAssignLegacyMirrorHandles(t *testing.T) {
	t.Parallel()
	rb := CSRReadback{
		Vertices: []uint64{0, 3, 6, 7},
		Edges:    []graph.NodeID{1, 1, 0, 0, 0, 2, 1},
	}
	assignLegacyMirrorHandles(&rb)
	if want := []uint64{1, 2, 3, 1, 2, 4, 4}; !slices.Equal(rb.Handles, want) {
		t.Fatalf("handles = %v, want %v", rb.Handles, want)
	}
	kept := []uint64{9, 9, 9, 9, 9, 9, 9}
	rb.Handles = slices.Clone(kept)
	assignLegacyMirrorHandles(&rb)
	if !slices.Equal(rb.Handles, kept) {
		t.Fatalf("a handle column was rewritten: %v", rb.Handles)
	}
}
