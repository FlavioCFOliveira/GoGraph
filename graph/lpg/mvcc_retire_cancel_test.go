package lpg

// mvcc_retire_cancel_test.go — rmp #2964: who may withdraw a deferred label-index
// removal.
//
// A retirement defers its label-bitmap removals BEFORE the tombstone flip. A
// label re-asserted in between used to cancel them, so the deleted node stayed
// in the bitmap after every record about it was reclaimed and every present-time
// label scan returned it. The mirror case is a revival: it restored the bitmap
// entry WITHOUT cancelling the retirement's removal, so the next sweep took the
// restored entry out and a live node carrying the label vanished from scans.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// retireFixture builds node a carrying label L, with every record about the
// setup reclaimed so nothing but the step under test can explain what follows.
func retireFixture(t *testing.T) (*Graph[string, float64], LabelID, graph.NodeID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatal(err)
	}
	g.ReclaimNow()
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a was not interned")
	}
	return g, g.reg.intern("L"), id
}

// requireIndexed asserts both the raw bitmap and a present-time label scan
// agree on whether id is a member of lid.
func requireIndexed(t *testing.T, g *Graph[string, float64], lid LabelID, id graph.NodeID, want bool, why string) {
	t.Helper()
	if got := g.nodeIdx.Has(uint32(lid), id); got != want {
		t.Errorf("%s: raw label bitmap membership = %v, want %v", why, got, want)
	}
	if got := g.LabelBitmapAsOf(lid, nil).Contains(uint64(id)); got != want {
		t.Errorf("%s: present-time label scan membership = %v, want %v", why, got, want)
	}
}

// TestRetireCancel_LabelReassertedMidRetirementLeavesNoDeadEntry re-asserts the
// node's label between the retirement's strip and its tombstone flip.
//
// The interleaving the defect needs — a re-assert that LANDS mid-retirement —
// is one transaction's: inside an exclusive bracket both write through the
// bracket's transaction, so the retirement and the re-assert share it. Two separate
// direct writes cannot produce it any more (rmp #2947): the re-assert is a
// second implicit transaction, refused by the retirement's pending death claim,
// so the direct variant checks the invariant with the re-assert refused.
func TestRetireCancel_LabelReassertedMidRetirementLeavesNoDeadEntry(t *testing.T) {
	for _, bracket := range []bool{true, false} {
		name := "direct"
		if bracket {
			name = "one transaction"
		}
		t.Run(name, func(t *testing.T) {
			g, lid, id := retireFixture(t)
			fired := false
			// The bracket's transaction once it is open; the zero value makes
			// the re-assert a direct write of its own.
			var hookTx WriteTx
			g.retireStripFlipHookForTest = func() {
				g.retireStripFlipHookForTest = nil
				fired = true
				if g.idxPendingActive.Load() == 0 {
					t.Fatal("setup: the strip deferred no removal, so there is nothing to cancel")
				}
				// The node is still alive here: the flip has not happened.
				err := g.Writer(hookTx).SetNodeLabel("a", "L")
				if bracket && err != nil {
					t.Fatalf("re-assert inside the bracket: %v", err)
				}
				if !bracket && err != nil && !errors.Is(err, ErrDirectWriteConflict) {
					t.Fatalf("re-assert as a second direct write: %v", err)
				}
			}
			if bracket {
				if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
					hookTx = tx
					if !g.Writer(tx).RemoveNode("a") {
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
			g.ReclaimNow()
			if !g.IsTombstoned(id) {
				t.Fatal("setup: the node is not removed")
			}
			requireIndexed(t, g, lid, id, false,
				"a node retired while a label was re-asserted on it (rmp #2964)")
		})
	}
}

func TestRetireCancel_LabelOnADeadNodeCancelsNothing(t *testing.T) {
	g, lid, id := retireFixture(t)
	hold := g.BeginRead() // keeps the removals below deferred
	if err := g.RemoveNodeLabel("a", "L"); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatal(err)
	}
	g.EndRead(hold)
	g.ReclaimNow()
	requireIndexed(t, g, lid, id, false,
		"a label set on an already removed node withdrew the pending removal (rmp #2964)")
}

func TestRetireCancel_RevivalWithdrawsTheRetirementsRemoval(t *testing.T) {
	g, lid, id := retireFixture(t)
	hold := g.BeginRead() // keeps the retirement's removal deferred
	if err := g.RemoveNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	g.EndRead(hold)
	g.ReclaimNow()
	if g.IsTombstoned(id) {
		t.Fatal("setup: the node was not revived")
	}
	requireIndexed(t, g, lid, id, true,
		"a revived node carrying L lost its entry to the retirement's deferred removal (rmp #2964)")
}
