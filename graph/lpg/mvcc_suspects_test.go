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

	// The sweep lands between the acquire and the post-acquire sample.
	bm := g.labelBitmapAsOfFiltered(nil, oneLabel(lid),
		func() (*roaring64.Bitmap, bool) {
			c := g.nodeIdx.Intersect(uint32(lid))
			g.labelDeltaActive.Store(0)
			g.nodeLifeActive.Store(0)
			g.idxPendingActive.Store(0)
			return c, true
		},
		func(bag labelBag) bool { return bag.has(lid) })

	if bm.Contains(uint64(id)) {
		t.Fatalf("the node whose label was REMOVED is still in the corrected bitmap: the suspect " +
			"sample was drained by the sweep during the clone, so the correction ran over an empty " +
			"set. A count taken from this has no predicate left to catch it (rmp #2326).")
	}
}

// TestLabelBitmapAsOf_SpanningSurvivesTheDeferredClone is the #2326 interleaving
// re-run over the #2863 acquire, and it is the test that earns the deferred copy.
//
// # What changed, and why it could have broken this
//
// #2326 established that the suspect set must be sampled on BOTH sides of the
// instant the answer describes. #2863 stopped copying the bitmap at that instant:
// [Graph.LabelBitmapAsOf] now acquires [label.Index.BitmapShared]'s shared image
// there and copies only later, after the second sample, and only when a correction
// is actually owed.
//
// If that copy reproduced the index's PRESENT state rather than the acquire
// instant, the correction would run against an image nothing had bracketed and
// #2326 would be back — silently, because the answer is right whenever nothing
// changes in the window. So this injects a change into exactly that window.
//
// # The injection
//
// The closure acquires the shared image AND THEN drains all three churn counters,
// exactly as the sweep does in the test above — but here `owned` is false, so the
// clone happens downstream of the drain. The node whose label was removed must
// still come out corrected. It can only do so if the pre-acquire sample was
// unioned in AND the late clone reproduced the acquired image.
func TestLabelBitmapAsOf_SpanningSurvivesTheDeferredClone(t *testing.T) {
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

	snap := g.BeginRead()
	defer g.EndRead(snap)
	if err := g.ApplyAtomically(func() error { g.RemoveNodeLabel("a", "L"); return nil }); err != nil {
		t.Fatalf("RemoveNodeLabel: %v", err)
	}
	if !g.nodeIdx.BitmapShared(uint32(lid)).Contains(uint64(id)) {
		t.Fatal("precondition: the deferred removal should have LEFT the entry in the shared image")
	}

	var acquired *roaring64.Bitmap
	bm := g.labelBitmapAsOfFiltered(nil, oneLabel(lid),
		func() (*roaring64.Bitmap, bool) {
			acquired = g.nodeIdx.BitmapShared(uint32(lid))
			g.labelDeltaActive.Store(0)
			g.nodeLifeActive.Store(0)
			g.idxPendingActive.Store(0)
			return acquired, false
		},
		func(bag labelBag) bool { return bag.has(lid) })

	if bm.Contains(uint64(id)) {
		t.Fatalf("the node whose label was REMOVED is still in the corrected bitmap. The clone is " +
			"now taken AFTER the post-acquire sample, so either the pre-acquire sample was not " +
			"unioned in, or the late clone reproduced a later instant than the acquire (rmp #2863 " +
			"on top of rmp #2326).")
	}
	// WHITE-BOX: the correction must have gone to a PRIVATE copy. Had it mutated
	// the shared image in place, every other reader of L would now be reading a
	// bitmap this call edited — a corruption no differential assertion can see,
	// because the answer returned is correct either way.
	if bm == acquired {
		t.Fatal("the corrected bitmap IS the shared image: correctBitmapOver mutated a bitmap the " +
			"index still publishes to every other reader")
	}
	if !acquired.Contains(uint64(id)) {
		t.Fatal("the shared image lost the member the correction removed: the correction wrote " +
			"through into the index's published image")
	}
}
