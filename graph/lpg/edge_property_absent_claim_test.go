package lpg

// edge_property_absent_claim_test.go — rmp #3006: removing a relationship
// property the pair does not carry must not claim the source node's adjacency
// entry, and must still be refused wherever a peer's write makes the verdict
// "absent" unsafe.
//
// The Cypher-level gate (cypher/noop_conflict_stamp_3006_3008_test.go) cannot
// isolate the per-pair primitive: a Cypher REMOVE also removes from the
// relationship instance's by-handle bag, whose own conflict test refuses the
// stored-vs-visible arm below by itself. This file drives the primitive alone.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// absentClaimGraph builds a→b carrying p and a→c, and interns `absent` on c so
// a removal of it reaches the adjacency.
func absentClaimGraph(t *testing.T) *Graph[string, int64] {
	t.Helper()
	g := New[string, int64](adjlist.Config{})
	for _, err := range []error{
		g.AddEdge("a", "b", 1),
		g.AddEdge("a", "c", 1),
		g.SetEdgeProperty("a", "b", "p", Int64Value(1)),
		g.SetNodeProperty("c", "absent", Int64Value(1)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestDelEdgeProperty_AbsentKeyTakesNoClaim_3006: T1 removes a key a→b does not
// carry; a peer then writes a→c's property and commits. The peer must not be
// refused, T1 must have written no version, and both commits succeed.
func TestDelEdgeProperty_AbsentKeyTakesNoClaim_3006(t *testing.T) {
	g := absentClaimGraph(t)
	t1 := g.beginLabelTx()
	g.delEdgePropertyInfo("a", "b", "absent", t1.ctx)
	if n := t1.ctx.tx.Versions(); n != 0 {
		t.Errorf("removing an absent key wrote %d versions, want 0", n)
	}
	peer := g.beginLabelTx()
	if err := g.setEdgePropertyInfo("a", "c", "q", Int64Value(2), peer.ctx); err != nil {
		t.Fatalf("peer write on a's other arc beside an open removal of an absent key: %v", err)
	}
	if _, err := peer.commit(); err != nil {
		t.Fatalf("peer commit: %v", err)
	}
	if _, err := t1.commit(); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}
}

// TestDelEdgeProperty_AbsentKeyStillRefusedOverPeer_3006: the verdict "absent"
// must not be taken over a peer's write T1 cannot see.
//
//   - peer_removal: the peer's UNCOMMITTED removal of p heads the stored entry,
//     so p is absent from it while T1 still sees p. Admitted without a claim,
//     T1's acknowledged removal would be undone by the peer's rollback.
//   - peer_set: the peer's uncommitted write of the key T1 removes.
//   - peer_set_committed: the same write, committed after T1's snapshot.
func TestDelEdgeProperty_AbsentKeyStillRefusedOverPeer_3006(t *testing.T) {
	for _, c := range []struct {
		name, key string
		peer      func(g *Graph[string, int64], tx *writeCtx)
		commit    bool
	}{
		{"peer_removal", "p", func(g *Graph[string, int64], tx *writeCtx) { g.delEdgePropertyInfo("a", "b", "p", tx) }, false},
		{"peer_set", "absent", func(g *Graph[string, int64], tx *writeCtx) {
			_ = g.setEdgePropertyInfo("a", "b", "absent", Int64Value(5), tx)
		}, false},
		{"peer_set_committed", "absent", func(g *Graph[string, int64], tx *writeCtx) {
			_ = g.setEdgePropertyInfo("a", "b", "absent", Int64Value(5), tx)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := absentClaimGraph(t)
			t1 := g.beginLabelTx() // its snapshot predates the peer's write
			peer := g.beginLabelTx()
			c.peer(g, peer.ctx)
			if err := peer.ctx.err(); err != nil {
				t.Fatalf("peer write: %v", err)
			}
			if c.commit {
				if _, err := peer.commit(); err != nil {
					t.Fatalf("peer commit: %v", err)
				}
			}
			g.delEdgePropertyInfo("a", "b", c.key, t1.ctx)
			var cf *mvcc.Conflict
			if err := t1.ctx.err(); !errors.As(err, &cf) || cf.Store != mvcc.StoreAdjacency {
				t.Errorf("T1's removal over the peer's write: err=%v, want a conflict in %q", err, mvcc.StoreAdjacency)
			}
			if _, err := t1.commit(); !errors.Is(err, mvcc.ErrSerializationConflict) {
				t.Errorf("T1 commit: %v, want a serialization conflict", err)
			}
			if !c.commit {
				peer.abort()
			}
		})
	}
}
