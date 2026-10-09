package lpg

// mvcc_retire_hold_test.go — rmp #2963: the churn gate across a node
// retirement's strip and tombstone flip.
//
// [Graph.removeNodeInfo] used to take a scoped churn hold across the whole
// retirement (rmp #2686). It was removed because two other holders already raise
// the gate of every label in the bag before the first mutation and keep it up
// past the flip: the death claim, and the deferred strip. This guard pins that
// property directly at the seam between the strip and the flip: any change that
// leaves the gate shut in that window fails here deterministically.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestRetireHold_GateRaisedAcrossStripAndFlip retires a node carrying two
// labels and, between the strip and the flip, asserts the gate of both labels
// is raised and the still-alive node is still returned by a present-time scan.
// It then writes there — a re-assert of a carried label and a label the node
// did not carry — and asserts no label bitmap keeps the dead node, before and
// after reclamation.
//
// The writes run inside the retirement's own transaction ("one transaction"),
// the only way they can land in the window, and as separate direct writes
// ("direct"), which the retirement's pending death claim refuses.
func TestRetireHold_GateRaisedAcrossStripAndFlip(t *testing.T) {
	for _, bracket := range []bool{true, false} {
		name := "direct"
		if bracket {
			name = "one transaction"
		}
		t.Run(name, func(t *testing.T) {
			g := New[string, float64](adjlist.Config{})
			t.Cleanup(func() { _ = g.Close() })
			if err := g.AddNode("a"); err != nil {
				t.Fatal(err)
			}
			for _, l := range []string{"L", "M"} {
				if err := g.SetNodeLabel("a", l); err != nil {
					t.Fatal(err)
				}
			}
			g.ReclaimNow()
			id, ok := g.adj.Mapper().Lookup("a")
			if !ok {
				t.Fatal("a was not interned")
			}
			carried := []LabelID{g.reg.intern("L"), g.reg.intern("M")}
			added := g.reg.intern("N")
			for _, lid := range carried {
				if n := g.labelChurn.load(lid); n != 0 {
					t.Fatalf("setup: label %d gate reads %d before the retirement, so the "+
						"retirement would not be what raises it", lid, n)
				}
			}

			fired := false
			var hookTx WriteTx
			g.retireStripFlipHookForTest = func() {
				g.retireStripFlipHookForTest = nil
				fired = true
				for _, lid := range carried {
					if n := g.labelChurn.load(lid); n == 0 {
						t.Errorf("label %d gate is shut between the strip and the tombstone "+
							"flip: a present-time reader takes the raw bitmap across the "+
							"flip and is handed the dead node (rmp #2963)", lid)
					}
					if !g.LabelBitmapAsOf(lid, nil).Contains(uint64(id)) {
						t.Errorf("label %d: the node is alive before the flip but a "+
							"present-time scan lost it", lid)
					}
				}
				w := g.Writer(hookTx)
				for _, l := range []string{"L", "N"} {
					err := w.SetNodeLabel("a", l)
					if bracket && err != nil {
						t.Fatalf("SetNodeLabel(%q) inside the bracket: %v", l, err)
					}
					if !bracket && err != nil && !errors.Is(err, ErrDirectWriteConflict) {
						t.Fatalf("SetNodeLabel(%q) as a second direct write: %v", l, err)
					}
				}
			}
			if bracket {
				if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
					hookTx = tx
					if ok, _ := g.Writer(tx).RemoveNode("a"); !ok {
						t.Errorf("RemoveNode inside the bracket was refused: %v", tx.Err())
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := g.RemoveNode("a"); err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the seam between the strip and the flip never ran")
			}
			if !g.IsTombstoned(id) {
				t.Fatal("setup: the node is not removed")
			}
			all := append([]LabelID{added}, carried...)
			for _, lid := range all {
				if g.LabelBitmapAsOf(lid, nil).Contains(uint64(id)) {
					t.Errorf("label %d: a present-time scan returned the removed node "+
						"before reclamation", lid)
				}
			}
			g.ReclaimNow()
			for _, lid := range all {
				requireIndexed(t, g, lid, id, false, "after reclamation, a removed node")
			}
		})
	}
}
