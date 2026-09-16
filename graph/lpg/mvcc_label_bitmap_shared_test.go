package lpg

// mvcc_label_bitmap_shared_test.go — [Graph.LabelBitmapAsOf]'s zero-copy path
// (rmp #2863).
//
// The correctness of the path is pinned next door, in mvcc_suspects_test.go,
// where the sweep is injected into the acquire window. What is pinned HERE is
// that the path is taken at all, and what it costs — because a result-identical
// optimisation is invisible to every assertion about the result.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// sharedBitmapRig returns a graph carrying label L on n nodes, with its own
// start-up churn drained, so the no-correction gate is the one under test rather
// than the fixture's history.
func sharedBitmapRig(t *testing.T, n int) (*Graph[string, float64], LabelID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	for i := 0; i < n; i++ {
		name := string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := g.AddNode(name); err != nil {
			t.Fatalf("AddNode(%s): %v", name, err)
		}
		if err := g.ApplyAtomically(func() error { return g.SetNodeLabel(name, "L") }); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", name, err)
		}
	}
	// Drain the rig's OWN history: without this the gate is live for L and every
	// read takes the correcting path, which is the opposite of what is measured.
	g.ReclaimNow()
	return g, g.reg.Intern("L")
}

// TestLabelBitmapAsOf_QuietLabelReturnsTheIndexImageUncopied is the white-box
// statement of the whole change: when no label the read concerns has a live
// suspect, the answer IS the index's published image and not a copy of it.
//
// Pointer identity is the assertion because nothing weaker can distinguish the
// fix from the code it replaced — the old clone returned an EQUAL bitmap on this
// exact path, which is why the copy went unnoticed long enough to reach 52.75%
// of all allocation on 35_mvcc_mixed_workload.
func TestLabelBitmapAsOf_QuietLabelReturnsTheIndexImageUncopied(t *testing.T) {
	g, lid := sharedBitmapRig(t, 40)

	img := g.nodeIdx.BitmapShared(uint32(lid))
	got := g.LabelBitmapAsOf(lid, nil)
	if got != img {
		t.Fatalf("LabelBitmapAsOf returned a different object from the index's published image: " +
			"the quiet path is still copying, which is the whole of rmp #2863")
	}
	if got.GetCardinality() != 40 {
		t.Fatalf("LabelBitmapAsOf reports %d members, want 40", got.GetCardinality())
	}
}

// TestLabelBitmapAsOf_QuietLabelAllocatesNothing measures that same path the way
// the finding was measured.
//
// The number to beat is not zero in the abstract: it is whatever the clone cost,
// and the control below keeps that comparison live rather than remembered.
func TestLabelBitmapAsOf_QuietLabelAllocatesNothing(t *testing.T) {
	g, lid := sharedBitmapRig(t, 200)
	_ = g.LabelBitmapAsOf(lid, nil) // warm the index image

	if n := testing.AllocsPerRun(200, func() { _ = g.LabelBitmapAsOf(lid, nil) }); n != 0 {
		t.Errorf("LabelBitmapAsOf allocated %.2f objects on a quiet label, want 0", n)
	}
	if n := testing.AllocsPerRun(200, func() { _ = g.nodeIdx.Intersect(uint32(lid)) }); n == 0 {
		t.Error("Intersect allocates nothing either, so the zero above demonstrates no difference")
	}
}

// TestLabelBitmapAsOf_CorrectingPathDoesNotTouchTheIndexImage is the safety half.
//
// The quiet path hands out a shared object; the correcting path must therefore
// copy before it mutates, or one snapshot's correction would rewrite the bitmap
// every other reader of the label is holding. That corruption is silent — the
// correcting caller still gets the right answer — so it is asserted against the
// INDEX's image, not against the return value.
func TestLabelBitmapAsOf_CorrectingPathDoesNotTouchTheIndexImage(t *testing.T) {
	g, lid := sharedBitmapRig(t, 30)

	id, ok := g.adj.Mapper().Lookup("aa")
	if !ok {
		t.Fatal("node aa not found")
	}
	// Pin a snapshot, then remove the label under it. The removal is deferred, so
	// the raw index keeps the entry and only a correction can take it out — i.e.
	// the correcting path is forced.
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if err := g.ApplyAtomically(func() error { g.RemoveNodeLabel("aa", "L"); return nil }); err != nil {
		t.Fatalf("RemoveNodeLabel: %v", err)
	}

	img := g.nodeIdx.BitmapShared(uint32(lid))
	imgCard := img.GetCardinality()

	got := g.LabelBitmapAsOf(lid, nil)
	if got.Contains(uint64(id)) {
		t.Fatal("the removed label is still reported: the correcting path did not run, so this " +
			"test is not covering what it claims to")
	}
	if got == img {
		t.Fatal("the corrected bitmap IS the index's published image")
	}
	if got := img.GetCardinality(); got != imgCard {
		t.Fatalf("the index's published image changed under the correction: %d -> %d members. "+
			"Every other reader of this label is now holding a bitmap this call edited.",
			imgCard, got)
	}
	if !img.Contains(uint64(id)) {
		t.Fatal("the correction removed its member from the index's published image rather than " +
			"from its own copy")
	}
}
