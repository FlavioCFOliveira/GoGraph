package lpg

// mvcc_conflict_aborted_stamp_test.go — rmp #2997: an ABORTED adjacency stamp
// must not take the commit it displaced with it.
//
// Each side of a node's adjacency stamp is one slot. A transaction that can see
// the committed stamp overwrites it; when that transaction aborts, the slot is
// cleared, and before the fix the displaced commit was gone with it — a
// transaction whose snapshot predates that commit then removed the node over it.
// On the Cypher path that is a committed DETACH DELETE beside an incoming arc it
// never saw (cypher.TestDetachDelete_RefusedOverArcCommittedAfterSnapshot).
//
// One row per side: the displaced commit is an APPEND (the arc a peer added) or
// an EXCLUSIVE write (an arc a peer removed). Both rows fail on the build without
// [adjStamps.set]'s floor, with the removal admitted; the control rows, with no
// aborted displacer, are refused on both builds.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// errNotApplied reports a scripted removal the graph did not admit.
var errNotApplied = errors.New("removal not admitted")

func TestConflict_AbortedStampKeepsDisplacedCommit(t *testing.T) {
	cases := []struct {
		name string
		// peer commits a write to hub after the deleter's snapshot.
		peer func(tx *labelTx[string, int64]) error
		// displacer, begun after the peer's commit, writes the same side of hub's
		// stamp and is aborted; nil runs the control.
		displacer func(tx *labelTx[string, int64]) error
	}{
		{
			name:      "append_side",
			peer:      func(tx *labelTx[string, int64]) error { return tx.addEdge("x", "hub", 1) },
			displacer: func(tx *labelTx[string, int64]) error { return tx.addEdge("y", "hub", 1) },
		},
		{
			name: "exclusive_side",
			peer: func(tx *labelTx[string, int64]) error {
				if !tx.removeEdge("z", "hub") {
					return errNotApplied
				}
				return nil
			},
			displacer: func(tx *labelTx[string, int64]) error {
				if !tx.removeEdge("w", "hub") {
					return errNotApplied
				}
				return nil
			},
		},
		{
			name: "append_side_control",
			peer: func(tx *labelTx[string, int64]) error { return tx.addEdge("x", "hub", 1) },
		},
		{
			name: "exclusive_side_control",
			peer: func(tx *labelTx[string, int64]) error {
				if !tx.removeEdge("z", "hub") {
					return errNotApplied
				}
				return nil
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
			for _, n := range []string{"hub", "x", "y"} {
				if err := g.AddNode(n); err != nil {
					t.Fatalf("AddNode(%s): %v", n, err)
				}
			}
			for _, src := range []string{"z", "w"} {
				if err := g.AddEdge(src, "hub", 1); err != nil {
					t.Fatalf("AddEdge(%s, hub): %v", src, err)
				}
			}

			deleter := g.beginLabelTx() // its snapshot predates the peer's commit

			peer := g.beginLabelTx()
			if err := c.peer(peer); err != nil {
				t.Fatalf("peer write: %v", err)
			}
			if _, err := peer.commit(); err != nil {
				t.Fatalf("peer commit: %v", err)
			}

			if c.displacer != nil {
				d := g.beginLabelTx() // sees the peer's commit, so it may stamp over it
				if err := c.displacer(d); err != nil {
					t.Fatalf("displacer write was refused, so it displaced nothing: %v", err)
				}
				d.abort() // clears the displacer's stamp synchronously
			}

			if deleter.removeNode("hub") {
				_, err := deleter.commit()
				t.Fatalf("the removal of hub was admitted over a write committed after the "+
					"deleter's snapshot (commit err=%v); the aborted displacer's cleared "+
					"stamp took the peer's commit with it (rmp #2997)", err)
			}
			_, err := deleter.commit()
			var cf *mvcc.Conflict
			if !errors.As(err, &cf) || cf.Store != mvcc.StoreAdjacency {
				t.Fatalf("deleter commit: err=%v, want a serialization conflict in %q", err, mvcc.StoreAdjacency)
			}
			if cf.ConcurrentWriter() {
				t.Fatalf("blocking head %d is an in-flight id; the peer had committed, so this is "+
					"first-committer-wins", cf.HeadTS)
			}

			// Bounded: with no transaction open the floor is at or below the
			// watermark, so the reclaimer drops the entry like any other stamp.
			g.ReclaimNow()
			if n := g.adjVer.len(); n != 0 {
				t.Fatalf("%d adjacency stamp entries survive a reclaim with no open transaction", n)
			}
		})
	}
}
