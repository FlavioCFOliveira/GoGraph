package lpg

// mvcc_life_displaced_3001_test.go — regression for rmp #3001.
//
// The node-life store is one record deep per direction. The undo replay of a
// rolled-back DELETE revives the node, and that revival is a BIRTH record that
// overwrote the node's committed birth. The abort then withdrew both of the
// transaction's records by deleting them, so the committed birth was gone and a
// reader older than it fell back to the present tombstone bitmap: a node created
// after the reader's snapshot became visible to it.

import (
	"context"
	"testing"
)

// TestNodeLife_AbortedDeleteKeepsYoungNodeInvisible_3001 drives the exact
// sequence at the store: a reader pins, a node is created and committed, then a
// transaction deletes it, revives it in its undo replay and aborts. The reader
// must not see the node at any step, and the committed birth must be the record
// left behind.
func TestNodeLife_AbortedDeleteKeepsYoungNodeInvisible_3001(t *testing.T) {
	ctx := context.Background()
	g, _ := lifeGraph(t)
	snap := g.BeginRead()
	defer g.EndRead(snap)

	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("young") }); err != nil {
		t.Fatalf("create: %v", err)
	}
	id, ok := g.adj.Mapper().Lookup("young")
	if !ok {
		t.Fatal("young was not interned")
	}
	committedBirth, hasBorn, _, _ := lifePair(g, id)
	if !hasBorn || committedBirth.visibleTo(snap.startTS, snap.txID) {
		t.Fatalf("precondition: the birth must be committed and in the reader's future (hasBorn=%v)", hasBorn)
	}
	check := func(step string) {
		t.Helper()
		if g.NodeExistsAsOf(id, snap) {
			t.Errorf("%s: a reader older than the node's creation sees it", step)
		}
		if g.NodeInternedAsOf(id, snap) {
			t.Errorf("%s: a reader older than the node's creation sees it interned", step)
		}
	}
	check("before the delete")

	tx := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx, func(w WriteTx) error {
		_, _ = g.Writer(w).RemoveNode("young")
		return nil
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("delete in flight")

	tx.EnterUndo()
	if err := g.ApplyInVersionedTx(ctx, tx, func(w WriteTx) error {
		_ = g.Writer(w).Revive("young")
		return nil
	}); err != nil {
		t.Fatalf("undo revive: %v", err)
	}
	tx.ExitUndo()
	check("undo revive in flight")

	tx.Abandon()
	g.EndVersionedTx(tx)
	g.ReclaimNow()
	check("after the abort")

	born, hasBorn, _, hasDied := lifePair(g, id)
	if !hasBorn || born.info != committedBirth.info || born.seq != committedBirth.seq {
		t.Errorf("the committed birth was not restored: hasBorn=%v seq=%d want %d", hasBorn, born.seq, committedBirth.seq)
	}
	if hasDied {
		t.Errorf("the aborted death survived the abort")
	}
	if !g.NodeExistsAsOf(id, nil) {
		t.Errorf("the present lost the node the abort left alive")
	}
}

// TestNodeLife_ReaderBetweenDeathAndResurrection_3001 is the counterexample that
// refuted carrying only a displaced birth's INSTANT (DST crash seed 932): a node
// deleted and re-created, then deleted again by a transaction that rolls back. A
// reader older than the committed death must see the node; a reader between the
// death and the resurrection must not.
func TestNodeLife_ReaderBetweenDeathAndResurrection_3001(t *testing.T) {
	ctx := context.Background()
	g, id := lifeGraph(t)
	old := g.BeginRead()
	defer g.EndRead(old)
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		_, err := g.Writer(tx).RemoveNode("a")
		return err
	}); err != nil {
		t.Fatalf("committed delete: %v", err)
	}
	between := g.BeginRead()
	defer g.EndRead(between)
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).Revive("a") }); err != nil {
		t.Fatalf("committed resurrection: %v", err)
	}
	check := func(step string) {
		t.Helper()
		if !g.NodeExistsAsOf(id, old) {
			t.Errorf("%s: a reader older than the committed death lost the node", step)
		}
		if g.NodeExistsAsOf(id, between) {
			t.Errorf("%s: a reader between the death and the resurrection sees the node", step)
		}
	}
	check("before the rolled-back delete")

	tx := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx, func(w WriteTx) error {
		_, _ = g.Writer(w).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("delete in flight")
	tx.EnterUndo()
	if err := g.ApplyInVersionedTx(ctx, tx, func(w WriteTx) error {
		_ = g.Writer(w).Revive("a")
		return nil
	}); err != nil {
		t.Fatalf("undo revive: %v", err)
	}
	tx.ExitUndo()
	check("undo revive in flight")
	tx.Abandon()
	g.EndVersionedTx(tx)
	g.ReclaimNow()
	check("after the abort")
}
