package lpg

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Regression tests for the ACID audit of rmp #2965: a durable store commit's
// edge apply ([Graph.addEdgeHIfAbsentInfo] inside a write bracket) took no
// adjacency claim, and the adjacency admitted an explicit transaction's write
// over another explicit transaction's uncommitted entry. The store's entry then
// embedded the other transaction's uncommitted arc; when that transaction
// aborted, its withdrawal restored the stacked entry with the arc still in it.

// doomOnNode makes tx hit a write-write conflict on a fresh node held by an
// in-flight peer, and returns the peer, which the caller must end.
func doomOnNode(t *testing.T, g *Graph[string, float64], tx WriteTx) WriteTx {
	t.Helper()
	requireNoErr(t, g.AddNode("doom"))
	blocker := g.BeginVersionedTx()
	if err := g.Writer(blocker).SetNodeProperty("doom", "k", StringValue("1")); err != nil {
		t.Fatalf("blocker write: %v", err)
	}
	_ = g.Writer(tx).SetNodeProperty("doom", "k", StringValue("2"))
	if tx.Err() == nil {
		t.Fatal("tx was not doomed by the blocker")
	}
	return blocker
}

// TestDurableEdgeApply_RefusedOverExplicitUncommittedEntry: the store's edge
// apply on a source an explicit transaction has appended to and not committed is
// refused, and the explicit transaction's abort leaves none of its arc behind,
// whether or not its undo replay ran first.
func TestDurableEdgeApply_RefusedOverExplicitUncommittedEntry(t *testing.T) {
	for _, mode := range []string{"abort", "abort-after-undo"} {
		t.Run(mode, func(t *testing.T) {
			g := newDirectTxGraph(t)
			requireNoErr(t, g.AddNode("a"), g.AddNode("c"), g.AddNode("d"))

			t1 := g.BeginVersionedTx()
			h1, err := g.Writer(t1).AddEdgeH("a", "c", 1)
			if err != nil {
				t.Fatalf("t1 append: %v", err)
			}
			applyErr := g.ApplyVersioned(func(wtx WriteTx) error {
				_, e := g.Writer(wtx).AddEdgeHIfAbsent("a", "d", 1, 999999)
				return e
			})
			if !errors.Is(applyErr, mvcc.ErrSerializationConflict) {
				t.Fatalf("store edge apply over t1's uncommitted entry: err = %v; want a serialization conflict", applyErr)
			}
			if mode == "abort-after-undo" {
				t1.EnterUndo()
				if !g.Writer(t1).RemoveEdgeByHandle("a", "c", h1) {
					t.Fatal("t1's undo inverse removed nothing")
				}
				t1.ExitUndo()
			}
			blocker := doomOnNode(t, g, t1)
			g.EndVersionedTx(t1)
			g.EndVersionedTx(blocker)
			g.ReclaimNow()

			if g.AdjList().HasEdge("a", "c") {
				t.Error("ATOMICITY: the aborted transaction's arc a->c is in the committed graph")
			}
			if g.AdjList().HasEdge("a", "d") {
				t.Error("the refused apply's arc a->d is in the committed graph")
			}
			if err := g.AdjList().CheckInvariants(); err != nil {
				t.Errorf("invariants: %v", err)
			}
			// The refusal was retryable: with t1 gone the same apply succeeds.
			if err := g.ApplyVersioned(func(wtx WriteTx) error {
				_, e := g.Writer(wtx).AddEdgeHIfAbsent("a", "d", 1, 999999)
				return e
			}); err != nil {
				t.Fatalf("retried apply: %v", err)
			}
			if !g.AdjList().HasEdge("a", "d") {
				t.Error("retried apply left no a->d")
			}
		})
	}
}

// TestAdjacency_ExplicitWriteRefusedOverExplicitEntryWithoutClaim pins the
// structural half of the fix: the adjacency refuses an explicit transaction's
// write over another explicit transaction's uncommitted entry on its own, with
// no claim taken by the layer above. A raw adjacency write through the
// transaction's own token is such a write.
func TestAdjacency_ExplicitWriteRefusedOverExplicitEntryWithoutClaim(t *testing.T) {
	g := newDirectTxGraph(t)
	requireNoErr(t, g.AddNode("a"), g.AddNode("c"), g.AddNode("d"))
	t1 := g.BeginVersionedTx()
	if _, err := g.Writer(t1).AddEdgeH("a", "c", 1); err != nil {
		t.Fatalf("t1 append: %v", err)
	}
	t2 := g.BeginVersionedTx()
	err := g.adj.Writer(t2.w.adjTx()).AddEdge("a", "d", 1)
	var c *mvcc.Conflict
	if !errors.As(err, &c) || c.Store != mvcc.StoreAdjacency {
		t.Fatalf("unclaimed explicit write over t1's entry: err = %v; want an adjacency conflict", err)
	}
	if g.AdjList().HasEdge("a", "d") {
		t.Error("the refused write changed a's entry")
	}
	g.EndVersionedTx(t2)
	g.EndVersionedTx(t1)
}

// TestCompact_KeepsTheVersionChainOfAnUncommittedEntry: [adjlist.AdjList.Compact]
// republished a trimmed entry WITHOUT its version chain, so the aborting
// transaction's withdrawal no longer recognised its own entry and the aborted arc
// stayed committed.
func TestCompact_KeepsTheVersionChainOfAnUncommittedEntry(t *testing.T) {
	g := newDirectTxGraph(t)
	requireNoErr(t, g.AddNode("a"), g.AddNode("b"), g.AddNode("c"))
	for i := 0; i < 4; i++ { // spare capacity in a's columns, so Compact trims
		requireNoErr(t, g.AddEdge("a", "b", 1))
	}
	t1 := g.BeginVersionedTx()
	if _, err := g.Writer(t1).AddEdgeH("a", "c", 1); err != nil {
		t.Fatal(err)
	}
	nb, _ := g.AdjList().LoadEntry(nodeID(t, g, "a"))
	if cap(nb) == len(nb) {
		t.Fatalf("precondition: a's entry has no slack (len=cap=%d), Compact would not trim it", len(nb))
	}
	g.AdjList().Compact(context.Background())
	if nb, _ = g.AdjList().LoadEntry(nodeID(t, g, "a")); cap(nb) != len(nb) {
		t.Fatalf("precondition: Compact did not trim a's entry (len=%d cap=%d)", len(nb), cap(nb))
	}
	// A snapshot reader still steps back over the uncommitted arc.
	snap := g.BeginRead()
	n := g.ReadAt(snap).OutDegree("a")
	g.EndRead(snap)
	if n != 4 {
		t.Errorf("snapshot reader sees %d arcs out of a after Compact; want 4 (t1 uncommitted)", n)
	}
	blocker := doomOnNode(t, g, t1)
	g.EndVersionedTx(t1)
	g.EndVersionedTx(blocker)
	g.ReclaimNow()
	if g.AdjList().HasEdge("a", "c") {
		t.Error("ATOMICITY: the aborted arc a->c survived because Compact dropped the version chain")
	}
	if err := g.AdjList().CheckInvariants(); err != nil {
		t.Errorf("invariants: %v", err)
	}
}

// TestApplyDurable_Outcomes covers the bracket's contract: a conflict with an
// explicit transaction refuses before durable runs; a durable failure and a
// panic out of durable abort, leaving nothing visible; success publishes.
func TestApplyDurable_Outcomes(t *testing.T) {
	write := func(g *Graph[string, float64]) func(WriteTx) error {
		return func(wtx WriteTx) error { return g.Writer(wtx).SetNodeProperty("n", "v", StringValue("x")) }
	}
	visible := func(g *Graph[string, float64]) bool {
		snap := g.BeginRead()
		defer g.EndRead(snap)
		_, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
		return ok
	}

	t.Run("explicit-conflict-refuses-before-durable", func(t *testing.T) {
		g := newDirectTxGraph(t)
		requireNoErr(t, g.AddNode("n"))
		t1 := g.BeginVersionedTx()
		requireNoErr(t, g.Writer(t1).SetNodeProperty("n", "v", StringValue("t1")))
		ran := false
		err := g.ApplyDurable(context.Background(), write(g), func() error { ran = true; return nil })
		g.EndVersionedTx(t1)
		if !errors.Is(err, mvcc.ErrSerializationConflict) {
			t.Fatalf("err = %v; want a serialization conflict", err)
		}
		if ran {
			t.Error("durable ran although the apply was refused")
		}
	})

	t.Run("durable-error-aborts", func(t *testing.T) {
		g := newDirectTxGraph(t)
		requireNoErr(t, g.AddNode("n"))
		boom := errors.New("fsync failed")
		if err := g.ApplyDurable(context.Background(), write(g), func() error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("err = %v; want %v", err, boom)
		}
		if visible(g) {
			t.Error("ATOMICITY: a transaction whose durable step failed is visible")
		}
		// The aborted versions are withdrawn: a direct write is not refused.
		requireNoErr(t, g.SetNodeProperty("n", "v", StringValue("after")))
	})

	t.Run("durable-panic-aborts", func(t *testing.T) {
		g := newDirectTxGraph(t)
		requireNoErr(t, g.AddNode("n"))
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not continue to the caller")
				}
			}()
			_ = g.ApplyDurable(context.Background(), write(g), func() error { panic("injected") })
		}()
		if visible(g) {
			t.Error("ATOMICITY: a transaction whose durable step panicked is visible")
		}
		requireNoErr(t, g.SetNodeProperty("n", "v", StringValue("after")))
	})

	t.Run("success-publishes", func(t *testing.T) {
		g := newDirectTxGraph(t)
		requireNoErr(t, g.AddNode("n"))
		var seen bool
		err := g.ApplyDurable(context.Background(), write(g), func() error {
			// Not yet visible while the durable step runs.
			seen = visible(g)
			return nil
		})
		if err != nil {
			t.Fatalf("ApplyDurable: %v", err)
		}
		if seen {
			t.Error("the transaction was visible before its durable step returned")
		}
		if !visible(g) {
			t.Error("a successful transaction is not visible")
		}
	})
}
