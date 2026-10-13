package lpg

// mvcc_abort_release_test.go — rmp #3119 and #3120: a transaction that aborts
// gives back the never-born nodes it created under synthetic keys, and nothing
// else; the state of a large transaction is not kept for recycling.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func newReleaseGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("seed") }); err != nil {
		t.Fatal(err)
	}
	return g
}

func lookupID(t *testing.T, g *Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(k)
	if !ok {
		t.Fatalf("%q is not interned", k)
	}
	return id
}

// TestAbortRelease_ReleasesSyntheticNeverBornOnly aborts a transaction that
// created a synthetic key and an ordinary one: the synthetic one is released —
// unbound, a hole, no tombstone, no unborn mark — and the ordinary one keeps the
// state the abort has always left (bound, tombstoned, unborn).
func TestAbortRelease_ReleasesSyntheticNeverBornOnly(t *testing.T) {
	t.Parallel()
	g := newReleaseGraph(t)
	m := g.AdjList().Mapper()
	var mintedID graph.NodeID
	err := g.ApplyVersioned(func(tx WriteTx) error {
		w := g.Writer(tx)
		if err := w.AddNode("__cx_a"); err != nil {
			return err
		}
		if err := w.SetNodeLabel("__cx_a", "L"); err != nil {
			return err
		}
		if err := w.SetNodeProperty("__cx_a", "p", Int64Value(1)); err != nil {
			return err
		}
		if err := w.AddNode("plain"); err != nil {
			return err
		}
		mintedID = lookupID(t, g, "__cx_a")
		tx.Abandon()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup("__cx_a"); ok {
		t.Fatal("the minted key of an aborted creation still resolves")
	}
	if k, ok := m.Resolve(mintedID); ok {
		t.Fatalf("the released id %d still resolves to %q", mintedID, k)
	}
	if g.IsTombstonedStored(mintedID) || g.inUnborn(mintedID) {
		t.Fatalf("released id %d: tombstoned=%v unborn=%v, want neither", mintedID, g.IsTombstonedStored(mintedID), g.inUnborn(mintedID))
	}
	plainID := lookupID(t, g, "plain")
	if !g.IsTombstonedStored(plainID) || !g.inUnborn(plainID) {
		t.Fatalf("unminted aborted id %d: tombstoned=%v unborn=%v, want both", plainID, g.IsTombstonedStored(plainID), g.inUnborn(plainID))
	}
	if got := m.Len(); got != 2 { // seed + plain
		t.Fatalf("mapper Len = %d, want 2", got)
	}
	if got := g.LiveOrderStored(); got != 1 {
		t.Fatalf("LiveOrderStored = %d, want 1 (seed)", got)
	}
	// A later creation of the same key is a new node under a new id.
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("__cx_a") }); err != nil {
		t.Fatal(err)
	}
	again := lookupID(t, g, "__cx_a")
	if again == mintedID {
		t.Fatalf("the released id %d was issued again", mintedID)
	}
	if g.IsTombstoned(again) || g.HasNodeLabelByID(again, "L") {
		t.Fatalf("the recreated node is tombstoned=%v or carries the aborted label=%v", g.IsTombstoned(again), g.HasNodeLabelByID(again, "L"))
	}
	if got := g.LiveOrder(); got != 2 {
		t.Fatalf("LiveOrder = %d, want 2", got)
	}
}

// TestAbortRelease_CommitKeepsWithdrawnCreation commits a transaction that
// created a synthetic key and removed it again — a creation withdrawn inside
// a transaction that commits. The commit marker names the id, so it stays bound.
func TestAbortRelease_CommitKeepsWithdrawnCreation(t *testing.T) {
	t.Parallel()
	g := newReleaseGraph(t)
	err := g.ApplyVersioned(func(tx WriteTx) error {
		w := g.Writer(tx)
		if err := w.AddNode("__cx_b"); err != nil {
			return err
		}
		_, err := w.RemoveNode("__cx_b")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	id := lookupID(t, g, "__cx_b")
	if _, ok := g.AdjList().Mapper().Resolve(id); !ok {
		t.Fatalf("committed id %d is a hole", id)
	}
	if !g.IsTombstonedStored(id) {
		t.Fatalf("committed-dead id %d is not tombstoned", id)
	}
}

// TestReleaseNeverBorn_SkipsAnIDAnotherTransactionRecordedOn takes an id left
// unborn by an aborted creation, lets a second transaction revive it (an
// in-flight birth record), and then runs the release over the id: the life
// record held under the per-node lock must make it skip. The setup aborts with
// an empty created list so the first abort does not release the id itself.
func TestReleaseNeverBorn_SkipsAnIDAnotherTransactionRecordedOn(t *testing.T) {
	t.Parallel()
	g := newReleaseGraph(t)
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("__cx_c"); err != nil {
			return err
		}
		tx.w.createdMu.Lock()
		tx.w.created = tx.w.created[:0]
		tx.w.createdMu.Unlock()
		tx.Abandon()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	id := lookupID(t, g, "__cx_c")
	if !g.inUnborn(id) {
		t.Fatalf("setup: %d is not unborn after the abort", id)
	}
	other := g.BeginVersionedTx()
	if err := g.Writer(other).AddNode("__cx_c"); err != nil {
		t.Fatal(err)
	}
	stale := &writeCtx{created: []graph.NodeID{id}}
	g.releaseNeverBorn(stale)
	if got, ok := g.AdjList().Mapper().Lookup("__cx_c"); !ok || got != id {
		t.Fatalf("a key another transaction revived was released: Lookup = (%d,%v), want (%d,true)", got, ok, id)
	}
	g.EndVersionedTx(other)
	if g.IsTombstoned(id) {
		t.Fatalf("the reviving transaction committed but %d reads dead", id)
	}
}

// TestAbortRelease_RecycledStateAndStampsAreReleased aborts a transaction that
// created more nodes than recycled state keeps, and checks that no adjacency
// conflict stamp survives the abort and that the transaction's state was not
// kept for recycling (rmp #3120).
func TestAbortRelease_RecycledStateAndStampsAreReleased(t *testing.T) {
	t.Parallel()
	g := newReleaseGraph(t)
	const n = 3 * maxRetainedCreated
	var stampsInFlight int
	var big *writeCtx
	err := g.ApplyVersioned(func(tx WriteTx) error {
		big = tx.w
		w := g.Writer(tx)
		for i := 0; i < n; i++ {
			k := "__cx_" + itoa(i)
			if err := w.AddEdge(k, "seed", 1); err != nil {
				return err
			}
		}
		stampsInFlight = g.adjVer.len()
		tx.Abandon()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stampsInFlight == 0 {
		t.Fatal("setup: the transaction left no adjacency conflict stamp to release")
	}
	if got := g.MVCCStats().AdjConflictStamps; got != 0 {
		t.Fatalf("%d adjacency conflict stamps survived the abort (had %d in flight)", got, stampsInFlight)
	}
	if got := g.AdjList().Mapper().Len(); got != 1 {
		t.Fatalf("mapper Len = %d after the abort, want 1 (seed)", got)
	}
	if big == nil {
		t.Fatal("setup: the bracket exposed no write state")
	}
	if w := g.writeCtxFree.Load(); w == big {
		t.Fatalf("the state of a transaction with %d created nodes was kept for recycling", n)
	} else if w != nil && cap(w.created) > maxRetainedCreated {
		t.Fatalf("recycled state holds a created list of capacity %d, max %d", cap(w.created), maxRetainedCreated)
	}
	// A small transaction's state is still recycled.
	var small *writeCtx
	if err := g.ApplyVersioned(func(tx WriteTx) error { small = tx.w; return g.Writer(tx).AddNode("plain2") }); err != nil {
		t.Fatal(err)
	}
	if g.writeCtxFree.Load() != small {
		t.Fatal("a small transaction's state was not recycled")
	}
}
