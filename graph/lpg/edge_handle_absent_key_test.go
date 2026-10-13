package lpg

// edge_handle_absent_key_test.go — rmp #3009: removing a by-handle property the
// instance's bag does not carry must write no version, and must still be refused
// wherever a peer's write makes the verdict "absent" unsafe.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// absentHandleGraph builds one edge a→b whose instance bag carries p and q, and
// interns `absent` on b so the removal reaches the bag. It returns the handle.
func absentHandleGraph(t *testing.T) (*Graph[string, int64], uint64) {
	t.Helper()
	g := New[string, int64](adjlist.Config{})
	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		g.SetEdgePropertyByHandle("a", "b", h, "p", Int64Value(1)),
		g.SetEdgePropertyByHandle("a", "b", h, "q", Int64Value(1)),
		g.SetNodeProperty("b", "absent", Int64Value(1)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return g, h
}

// TestDelEdgePropertyByHandle_AbsentKeyWritesNoVersion_3009: T1 removes a key the
// bag does not carry; a peer then writes another key of the same instance and
// commits. T1 must have written no version, the peer must not be refused, and
// both commits succeed.
func TestDelEdgePropertyByHandle_AbsentKeyWritesNoVersion_3009(t *testing.T) {
	g, h := absentHandleGraph(t)
	t1 := g.beginLabelTx()
	g.delEdgePropertyByHandleInfo("a", "b", h, "absent", t1.ctx)
	if n := t1.ctx.tx.Versions(); n != 0 {
		t.Errorf("removing an absent by-handle key wrote %d versions, want 0", n)
	}
	peer := g.beginLabelTx()
	if err := g.setEdgePropertyByHandleInfo("a", "b", h, "q", Int64Value(2), peer.ctx); err != nil {
		t.Fatalf("peer write of another key of the instance: %v", err)
	}
	if err := peer.ctx.err(); err != nil {
		t.Fatalf("peer refused beside an open removal of an absent key: %v", err)
	}
	if _, err := peer.commit(); err != nil {
		t.Fatalf("peer commit: %v", err)
	}
	if _, err := t1.commit(); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}
}

// TestDelEdgePropertyByHandle_AbsentKeyStillRefusedOverPeer_3009: the verdict
// "absent" must not be taken over a peer's write T1 cannot see.
//
//   - peer_removal: the peer's UNCOMMITTED removal of p leaves the stored bag
//     without p (q keeps the bag alive) while T1 still sees p.
//   - peer_set: the peer's uncommitted write of the key T1 removes.
//   - peer_set_committed: the same write, committed after T1's snapshot.
func TestDelEdgePropertyByHandle_AbsentKeyStillRefusedOverPeer_3009(t *testing.T) {
	for _, c := range []struct {
		name, key string
		peer      func(g *Graph[string, int64], h uint64, tx *writeCtx)
		commit    bool
	}{
		{"peer_removal", "p", func(g *Graph[string, int64], h uint64, tx *writeCtx) {
			g.delEdgePropertyByHandleInfo("a", "b", h, "p", tx)
		}, false},
		{"peer_set", "absent", func(g *Graph[string, int64], h uint64, tx *writeCtx) {
			_ = g.setEdgePropertyByHandleInfo("a", "b", h, "absent", Int64Value(5), tx)
		}, false},
		{"peer_set_committed", "absent", func(g *Graph[string, int64], h uint64, tx *writeCtx) {
			_ = g.setEdgePropertyByHandleInfo("a", "b", h, "absent", Int64Value(5), tx)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, h := absentHandleGraph(t)
			t1 := g.beginLabelTx() // its snapshot predates the peer's write
			peer := g.beginLabelTx()
			c.peer(g, h, peer.ctx)
			if err := peer.ctx.err(); err != nil {
				t.Fatalf("peer write: %v", err)
			}
			if c.commit {
				if _, err := peer.commit(); err != nil {
					t.Fatalf("peer commit: %v", err)
				}
			}
			g.delEdgePropertyByHandleInfo("a", "b", h, c.key, t1.ctx)
			var cf *mvcc.Conflict
			if err := t1.ctx.err(); !errors.As(err, &cf) || cf.Store != mvcc.StoreEdgePropsHandle {
				t.Errorf("T1's removal over the peer's write: err=%v, want a conflict in %q", err, mvcc.StoreEdgePropsHandle)
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
