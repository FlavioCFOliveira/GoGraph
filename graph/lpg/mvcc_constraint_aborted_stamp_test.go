package lpg

// mvcc_constraint_aborted_stamp_test.go — rmp #2998: an ABORTED constraint stamp
// must not take the commit it displaced with it.
//
// A node's constraint stamp is one slot. A transaction that can see the committed
// stamp overwrites it; when that transaction aborts the slot is cleared, and
// before the fix the displaced commit was gone with it — a transaction whose
// snapshot predates that commit was then ADMITTED to stamp the node, which is the
// write skew the store exists to refuse (the same defect rmp #2997 fixed for the
// adjacency stamps).
//
// The aborted row fails on the build without [constraintStamp.set]'s floor; the
// control row, with no aborted displacer, is refused on both builds.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func TestConstraintConflict_AbortedStampKeepsDisplacedCommit(t *testing.T) {
	for _, displaced := range []bool{true, false} {
		name := "aborted_displacer"
		if !displaced {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			g := New[string, int64](adjlist.Config{Directed: true})
			if err := g.AddNode("n"); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			id, ok := g.adj.Mapper().Lookup("n")
			if !ok {
				t.Fatal("node n was not interned")
			}

			old := g.beginLabelTx() // its snapshot predates the peer's commit

			peer := g.beginLabelTx()
			if err := g.conVer.note(id, peer.ctx); err != nil {
				t.Fatalf("peer stamp: %v", err)
			}
			if _, err := peer.commit(); err != nil {
				t.Fatalf("peer commit: %v", err)
			}

			if displaced {
				d := g.beginLabelTx() // sees the peer's commit, so it may stamp over it
				if err := g.conVer.note(id, d.ctx); err != nil {
					t.Fatalf("displacer stamp was refused, so it displaced nothing: %v", err)
				}
				d.abort() // clears the displacer's stamp synchronously
				if n := g.conVer.len(); n != 1 {
					t.Errorf("%d constraint stamp entries after the abort, want 1 "+
						"(the peer's displaced commit)", n)
				}
			}

			err := g.conVer.note(id, old.ctx)
			var cf *mvcc.Conflict
			if !errors.As(err, &cf) || cf.Store != mvcc.StoreNodeConstraint {
				t.Fatalf("old snapshot's stamp: err=%v, want a serialization conflict in %q; "+
					"the aborted displacer's cleared stamp took the peer's commit with it "+
					"(rmp #2998)", err, mvcc.StoreNodeConstraint)
			}
			if cf.ConcurrentWriter() {
				t.Fatalf("blocking head %d is an in-flight id; the peer had committed, so "+
					"this is first-committer-wins", cf.HeadTS)
			}
			if _, err := old.commit(); !errors.Is(err, mvcc.ErrSerializationConflict) {
				t.Fatalf("old commit: err=%v, want a serialization conflict", err)
			}

			// Bounded: with no transaction open the floor is at or below the
			// watermark, so the reclaimer drops the entry like any other stamp.
			g.ReclaimNow()
			if n := g.conVer.len(); n != 0 {
				t.Fatalf("%d constraint stamp entries survive a reclaim with no open transaction", n)
			}
		})
	}
}
