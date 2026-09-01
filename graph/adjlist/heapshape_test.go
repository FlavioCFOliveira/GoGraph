package adjlist

import (
	"testing"
	"unsafe"
)

// heapshape_test.go — the adjacency half of the per-resident-node heap-shape
// table (rmp #2650).
//
// This is one table with graph/lpg/heapshape_test.go, split only because
// adjEntry is unexported in this package and unsafe.Sizeof cannot reach across
// a package boundary. The full rationale — why the resident heap shape is gated
// at all, the B-10 measurements it guards, and the Memgraph practice it follows
// — is documented once, there.
//
// adjEntry is the single most leveraged struct in the module for resident cost:
// one lives per node that has any outgoing edge, it is the object the lock-free
// read path publishes with one atomic store, and every field in it is either a
// pointer or a slice header. Its size is therefore also its pointer-slot count
// times eight, and rmp #2650 measured both prices on this host: ~4.2 ns of mark
// time per live heap object per collection, and ~0.52 ns per pointer slot. A
// new slice header here costs three words and three pointer slots on every node
// with an edge.
func TestHeapShape_PerNode_AdjEntry(t *testing.T) {
	t.Parallel()

	// W is instantiated at float64 because that is the weight type every
	// GoGraph benchmark, example and audit fixture uses, so the number below is
	// the one the B-10 measurements actually describe. The type parameter does
	// not change the size here: every field of adjEntry is a pointer, an atomic
	// pointer or a slice header, none of which embeds a W.
	const want = 120
	if got := unsafe.Sizeof(adjEntry[float64]{}); got != want {
		t.Fatalf("unsafe.Sizeof(adjEntry[float64]{}) = %d, want %d.\n"+
			"Composition: aux AuxColumn (interface, 16) + ver atomic.Pointer (8) + four "+
			"slice headers at 24 each — neighbours, weights, handles, labels — = 120, of "+
			"which 15 of the 15 words are pointer-bearing or slice headers.\n"+
			"One adjEntry is live per node with outgoing edges, so a change here is a "+
			"change to the module's resident memory AND to the mark cost of every "+
			"resident graph. It needs a re-measurement of the B-10 figures in "+
			"docs/audit-bottlenecks-2026-08-27.md §3.5, not a new constant here.",
			got, want)
	}
	if got := unsafe.Alignof(adjEntry[float64]{}); got != 8 {
		t.Fatalf("unsafe.Alignof(adjEntry[float64]{}) = %d, want 8", got)
	}
}
