package lpg

// mvcc_suspects_test.go — rmp #2326: the suspect sample spans the bitmap clone.
//
// Layer: short.

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestLabelBitmapAsOf_CorrectsWhenTheSweepLandsDuringTheClone pins the #2326 fix
// deterministically, by injecting the losing interleaving instead of racing for it.
//
// # The race
//
// [Graph.labelBitmapAsOfFiltered] clones the raw bitmap, decides whether to correct
// it, and samples the suspects. The suspect sample is GATED on three churn counters
// — it has to be, or a rolled-back SET gets added back — so a sweep that drains
// idxPendingActive between the clone and the sample made the set EMPTY. The
// correction then ran over nothing while the clone still carried the entry the sweep
// had just removed from the index. A scan can still reject that member per row; a
// COUNT cannot, and the wrong answer is final.
//
// The clone is taken through a caller-supplied closure, so the sweep landing mid-way
// is expressible exactly: this drains all three counters INSIDE the closure. Before
// the fix the only sample happened afterwards and came back empty; now the pre-clone
// sample is unioned in, and it cannot have been drained by a sweep that had not yet
// run.
func TestLabelBitmapAsOf_CorrectsWhenTheSweepLandsDuringTheClone(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.ApplyAtomically(func() error { return g.SetNodeLabel("a", "L") }); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	lid := g.reg.Intern("L")
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("node a not found")
	}

	// Arm versioning so the removal is DEFERRED: the entry stays in the raw bitmap
	// and only a correction can take it out.
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if err := g.ApplyAtomically(func() error { g.RemoveNodeLabel("a", "L"); return nil }); err != nil {
		t.Fatalf("RemoveNodeLabel: %v", err)
	}
	if !g.nodeIdx.Intersect(uint32(lid)).Contains(uint64(id)) {
		t.Fatal("precondition: the deferred removal should have LEFT the entry in the bitmap")
	}

	// The sweep lands between the clone and the post-clone sample.
	bm := g.labelBitmapAsOfFiltered(nil,
		func() *roaring64.Bitmap {
			c := g.nodeIdx.Intersect(uint32(lid))
			g.labelDeltaActive.Store(0)
			g.nodeLifeActive.Store(0)
			g.idxPendingActive.Store(0)
			return c
		},
		func(bag labelBag) bool { return bag.has(lid) })

	if bm.Contains(uint64(id)) {
		t.Fatalf("the node whose label was REMOVED is still in the corrected bitmap: the suspect " +
			"sample was drained by the sweep during the clone, so the correction ran over an empty " +
			"set. A count taken from this has no predicate left to catch it (rmp #2326).")
	}
}
