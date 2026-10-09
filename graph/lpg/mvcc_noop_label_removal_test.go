package lpg

// mvcc_noop_label_removal_test.go — rmp #2989: a label removal that changes
// nothing must not touch the deferred label-index removal of a committed one.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestRemoveNodeLabel_NoOpLeavesThePendingRemovalAlone commits a removal of L
// from a node while a reader keeps its bitmap removal deferred, then has two
// concurrent transactions remove the already-absent L and abort.
//
// A no-op removal claims nothing in the label store, so neither transaction
// conflicts with the other or with the committed removal. Each used to re-stamp
// the pending entry, and the one-level shadow kept only the stamp the second
// replaced — the first no-op's — so aborting both dropped the committed removal:
// the node stayed in L's bitmap after every record about it was reclaimed, and
// every present-time label scan returned a node that does not carry L.
func TestRemoveNodeLabel_NoOpLeavesThePendingRemovalAlone(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	// A second label keeps the node's label bag non-empty once L is gone, which
	// is the shape in which the no-op removal reaches the index maintenance.
	for _, l := range []string{"L", "M"} {
		if err := g.SetNodeLabel("a", l); err != nil {
			t.Fatal(err)
		}
	}
	g.ReclaimNow()
	lid := g.reg.intern("L")
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a was not interned")
	}

	hold := g.BeginRead() // keeps the committed removal below deferred
	if err := g.RemoveNodeLabel("a", "L"); err != nil {
		t.Fatal(err)
	}
	if g.idxPendingActive.Load() != 1 {
		t.Fatalf("setup: %d deferred removals pending, want the committed one", g.idxPendingActive.Load())
	}

	ctx := context.Background()
	t2, t3 := g.BeginVersionedTx(), g.BeginVersionedTx()
	for _, tx := range []WriteTx{t2, t3} {
		if err := g.ApplyInVersionedTx(ctx, tx, func(tx WriteTx) error {
			return g.Writer(tx).RemoveNodeLabel("a", "L")
		}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Err(); err != nil {
			t.Fatalf("setup: a removal of an absent label was refused: %v", err)
		}
	}
	for _, tx := range []WriteTx{t2, t3} {
		tx.Abandon()
		g.EndVersionedTx(tx)
	}
	g.EndRead(hold)
	g.ReclaimNow()

	if g.HasNodeLabel("a", "L") {
		t.Fatal("setup: the committed removal of L is not visible")
	}
	requireIndexed(t, g, lid, id, false,
		"two aborted no-op removals withdrew a committed removal's deferred bitmap entry (rmp #2989)")
}
