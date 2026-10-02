package lpg

// mvcc_life_test.go — white-box regressions for the versioned node EXISTENCE
// store (mvcc_life.go).
//
// The black-box witness for the same defect is internal/sim
// TestMVCCRegression_SplitLifePairKeepsNodeVisibleToOldReaders. This file pins
// the mechanism at the layer that owns it, so a future change to the record
// layout fails HERE, on the records themselves, rather than three layers up on
// a row count.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// lifePair reports the birth and death records held for id.
func lifePair[N comparable, W any](g *Graph[N, W], id graph.NodeID) (born lifeStamp, hasBorn bool, died lifeStamp, hasDied bool) {
	sh := g.nodeLifeShardFor(id)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	born, hasBorn = sh.born[id]
	died, hasDied = sh.died[id]
	return born, hasBorn, died, hasDied
}

// lifeGraph builds a graph with one committed, labelled node and returns it
// with the node's id.
func lifeGraph(t *testing.T) (*Graph[string, float64], graph.NodeID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("a"); err != nil {
			return err
		}
		return g.Writer(tx).SetNodeLabel("a", "Person")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a was not interned")
	}
	return g, id
}

// TestNodeLife_RolledBackDeleteSurvivesAnUnrelatedDelete is the SPLIT-PAIR case
// rmp #2445 left uncovered (rmp #2724).
//
// #2445 replaced a chain-level `primordial` flag with [aliveBefore], which
// infers the state before a died+born pair from the pair's own write order.
// That inference is sound only while BOTH halves belong to one transaction. The
// store is one record deep per direction, so a later, unrelated delete replaces
// the died half; the surviving pair reads born-then-died, and every reader
// older than the rollback is told a node whose birth committed before it began
// never existed.
//
// The invariant, stated positively: a node whose birth committed before a
// reader's snapshot, and whose only death is uncommitted or newer than that
// snapshot, must be visible to that reader — and must stay visible however many
// later transactions touch it.
func TestNodeLife_RolledBackDeleteSurvivesAnUnrelatedDelete(t *testing.T) {
	ctx := context.Background()
	g, id := lifeGraph(t)

	// The reader pins WHILE the doomed delete is in flight, so its snapshot is
	// older than the instant the rollback publishes. That is the one factor
	// separating this from the #2445 case, whose reader survives to the end.
	tx1 := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx1 delete: %v", err)
	}
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if !g.NodeExistsAsOf(id, snap) {
		t.Fatal("the reader lost the node to a PENDING delete")
	}

	// The undo replay of a rolled-back DELETE: it revives what the statement
	// tombstoned. A rolled-back apply PUBLISHES a real instant (mvcc_write.go),
	// so this is not an abort the reclaimer will withdraw.
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		_ = g.Writer(tx).Revive("a") // a transaction records its refusal on itself
		return nil
	}); err != nil {
		t.Fatalf("tx1 undo revive: %v", err)
	}
	g.EndVersionedTx(tx1)
	if !g.NodeExistsAsOf(id, snap) {
		t.Fatal("the reader lost the node to a rolled-back delete (rmp #2445)")
	}

	// A later, unrelated delete replaces the died half of the pair the rollback
	// left behind. It never commits, so the pinned reader must not notice.
	tx2 := g.BeginVersionedTx()
	defer g.EndVersionedTx(tx2)
	if err := g.ApplyInVersionedTx(ctx, tx2, func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx2 delete: %v", err)
	}

	born, hasBorn, died, hasDied := lifePair(g, id)
	if !hasBorn || !hasDied {
		t.Fatalf("the split pair was not reached: hasBorn=%v hasDied=%v", hasBorn, hasDied)
	}
	// NON-VACUITY. The record-level assertion below is only the reader's answer
	// while NEITHER event is visible to it — the branch [aliveBefore] serves. If
	// the reader could see either one, aliveBefore would not be consulted and
	// asserting on it would prove nothing.
	if born.visibleTo(snap.startTS, snap.txID) || died.visibleTo(snap.startTS, snap.txID) {
		t.Fatalf("the reader is not older than both records, so this is not the "+
			"aliveBefore branch: born{ts=%d seq=%d} died{ts=%d seq=%d} startTS=%d",
			born.at(), born.seq, died.at(), died.seq, snap.startTS)
	}
	if !aliveBefore(born, died) {
		t.Errorf("REPRODUCED at the record level: the surviving pair reads "+
			"born-then-died, so a reader older than both is told the node was "+
			"never alive; born{ts=%d seq=%d wasAlive=%v} died{ts=%d seq=%d}",
			born.at(), born.seq, born.wasAlive, died.at(), died.seq)
	}
	if !g.NodeExistsAsOf(id, snap) {
		t.Errorf("REPRODUCED: a reader older than the rollback lost a committed " +
			"node to a later unrelated PENDING delete")
	}
}

// TestNodeLife_InTxCreateDeleteRecreateStaysInvisible is the GUARD on the fix
// above, and it is the direction rmp #2443 closed.
//
// A transaction that CREATES a node, deletes it and creates it again writes the
// same died-then-born shape a rolled-back delete does, and its third write is
// also a revive over its own death. The difference is that this transaction
// owns the birth it displaces, so the node was NOT alive before it — and a
// reader older than the transaction must still see nothing, however many later
// deletes replace the died half.
//
// Without the second clause of [nodeLifeShard.aliveBeforeTx] this test fails:
// the phantom bare node #2443 fixed comes back for every reader older than the
// transaction, and it comes back PERMANENTLY, because the pair no longer heals.
func TestNodeLife_InTxCreateDeleteRecreateStaysInvisible(t *testing.T) {
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	defer func() { _ = g.Close() }()
	// One committed node so the graph is not empty and the reader's snapshot is
	// a real instant rather than the beginning of time.
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("seed") }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pinned BEFORE the transaction exists: nothing it does may ever be visible.
	snap := g.BeginRead()
	defer g.EndRead(snap)

	tx1 := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("a"); err != nil {
			return err
		}
		g.Writer(tx).RemoveNode("a")
		return g.Writer(tx).AddNode("a")
	}); err != nil {
		t.Fatalf("tx1 create/delete/create: %v", err)
	}
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a was not interned")
	}
	if g.NodeExistsAsOf(id, snap) {
		t.Fatal("PHANTOM: a reader older than the transaction sees its uncommitted node (rmp #2443)")
	}
	g.EndVersionedTx(tx1)
	if g.NodeExistsAsOf(id, snap) {
		t.Fatal("PHANTOM: a reader older than the transaction sees a node it never contained")
	}

	// The same later, unrelated delete that turns the rollback pair into a split
	// one. It must not turn this pair into a phantom.
	tx2 := g.BeginVersionedTx()
	defer g.EndVersionedTx(tx2)
	if err := g.ApplyInVersionedTx(ctx, tx2, func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx2 delete: %v", err)
	}
	born, hasBorn, died, hasDied := lifePair(g, id)
	if !hasBorn || !hasDied {
		t.Fatalf("the split pair was not reached: hasBorn=%v hasDied=%v", hasBorn, hasDied)
	}
	if born.visibleTo(snap.startTS, snap.txID) || died.visibleTo(snap.startTS, snap.txID) {
		t.Fatalf("the reader is not older than both records, so this is not the aliveBefore branch")
	}
	if g.NodeExistsAsOf(id, snap) {
		t.Errorf("PHANTOM: a reader older than the transaction that created AND "+
			"deleted the node sees it after an unrelated delete; "+
			"born{ts=%d seq=%d wasAlive=%v} died{ts=%d seq=%d}",
			born.at(), born.seq, born.wasAlive, died.at(), died.seq)
	}
}

// TestNodeLife_OrdinaryResurrectionIsNotMistakenForAnUndo is the second guard:
// a genuine delete-then-recreate across TWO transactions is a real birth, and
// the node must be dead for a reader pinned between the two.
//
// Without the first clause of [nodeLifeShard.aliveBeforeTx] — the one that
// requires the death to be the reviving transaction's OWN — this birth would
// claim the node had been alive all along, and a reader whose snapshot sits in
// the gap would be handed a node that was deleted before it began.
func TestNodeLife_OrdinaryResurrectionIsNotMistakenForAnUndo(t *testing.T) {
	ctx := context.Background()
	g, id := lifeGraph(t)

	if err := g.ApplyVersioned(func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("committed delete: %v", err)
	}
	// Pinned in the GAP: after the committed delete, before the recreate.
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if g.NodeExistsAsOf(id, snap) {
		t.Fatal("the reader sees a node deleted before it began")
	}

	tx := g.BeginVersionedTx()
	defer g.EndVersionedTx(tx)
	if err := g.ApplyInVersionedTx(ctx, tx, func(w WriteTx) error {
		return g.Writer(w).AddNode("a")
	}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	born, hasBorn, _, _ := lifePair(g, id)
	if !hasBorn {
		t.Fatal("the recreate wrote no birth record")
	}
	if born.wasAlive {
		t.Error("an ordinary resurrection was recorded as the undo of its own death")
	}
	if g.NodeExistsAsOf(id, snap) {
		t.Error("a reader pinned in the gap between a committed delete and a " +
			"recreate was handed the node")
	}
}

// TestNodeLife_RepeatedDeleteReviveKeepsTheTransactionsPriorState covers the
// third clause of [nodeLifeShard.aliveBeforeTx]: the propagation.
//
// A transaction that deletes, revives, deletes and revives the same node writes
// its second revive over a birth IT owns — the first revive's. Reading that
// literally ("the transaction owns the displaced birth, so it created the
// node") is wrong: what the second revive must carry is what the FIRST one
// established, namely that the node was alive before the transaction began. The
// propagation is bounded to one transaction's own records, which is what
// separates it from the `primordial` flag rmp #2445 removed.
func TestNodeLife_RepeatedDeleteReviveKeepsTheTransactionsPriorState(t *testing.T) {
	ctx := context.Background()
	g, id := lifeGraph(t)

	tx1 := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx1 first delete: %v", err)
	}
	// Pinned while the first delete is in flight, exactly as the split-pair
	// reproduction does: older than everything the transaction publishes.
	snap := g.BeginRead()
	defer g.EndRead(snap)

	for i, step := range []func(WriteTx) error{
		func(tx WriteTx) error { return g.Writer(tx).Revive("a") },
		func(tx WriteTx) error { g.Writer(tx).RemoveNode("a"); return nil },
		func(tx WriteTx) error { return g.Writer(tx).Revive("a") },
	} {
		if err := g.ApplyInVersionedTx(ctx, tx1, step); err != nil {
			t.Fatalf("tx1 step %d: %v", i, err)
		}
	}
	g.EndVersionedTx(tx1)

	born, hasBorn, _, _ := lifePair(g, id)
	if !hasBorn {
		t.Fatal("the second revive wrote no birth record")
	}
	if !born.wasAlive {
		t.Error("the second revive forgot what the first established: the node " +
			"was alive before the transaction touched it")
	}

	tx2 := g.BeginVersionedTx()
	defer g.EndVersionedTx(tx2)
	if err := g.ApplyInVersionedTx(ctx, tx2, func(tx WriteTx) error {
		g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx2 delete: %v", err)
	}
	if !g.NodeExistsAsOf(id, snap) {
		born, _, died, _ := lifePair(g, id)
		t.Errorf("a reader older than the delete/revive cycle lost a committed "+
			"node; born{ts=%d seq=%d wasAlive=%v} died{ts=%d seq=%d}",
			born.at(), born.seq, born.wasAlive, died.at(), died.seq)
	}
}

// TestNodeLife_GateIsRaisedBeforeTheRecordIsObservable pins that
// [Graph.nodeLifeActive] is published under the life shard lock that publishes
// the record (rmp #2842).
//
// The gate is read lock-free as "no life record exists", so a reader that can
// find a record must never load it at zero. The seam runs at the earliest
// instant any other goroutine can observe the record — right after the lock is
// released — and reads both there. Against the old ordering, which raised the
// gate after the unlock, every record written from a zero gate fails this.
//
// Both directions are driven from a zero gate: a birth on a fresh graph, and a
// death after every life record has been reclaimed.
func TestNodeLife_GateIsRaisedBeforeTheRecordIsObservable(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	t.Cleanup(func() { _ = g.Close() })
	if !g.mvccArmed {
		t.Fatal("setup: the graph is not MVCC-armed, so no life record is ever written")
	}

	var observed, atZero int
	g.nodeLifePublishedHookForTest = func(id graph.NodeID) {
		if _, hasBorn, _, hasDied := lifePair(g, id); !hasBorn && !hasDied {
			return
		}
		observed++
		if g.nodeLifeActive.Load() == 0 {
			atZero++
		}
	}

	if got := g.nodeLifeActive.Load(); got != 0 {
		t.Fatalf("setup: a fresh graph's life gate reads %d, want 0", got)
	}
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if observed != 1 {
		t.Fatalf("the birth published %d observable life records, want 1", observed)
	}

	// Drop every life record so the death below is written from a zero gate.
	// Single-threaded here, which is the exclusion reclaimNodeLife requires.
	g.reclaimNodeLife(^uint64(0))
	if got := g.nodeLifeActive.Load(); got != 0 {
		t.Fatalf("setup: after reclaiming every life record the gate reads %d, want 0", got)
	}
	if err := g.RemoveNode("a"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	g.nodeLifePublishedHookForTest = nil

	if observed != 2 {
		t.Fatalf("the birth and the death published %d observable life records, want 2", observed)
	}
	if atZero != 0 {
		t.Fatalf("%d of %d life records were observable while nodeLifeActive read 0: the "+
			"gate was raised after the shard lock was released, so a lock-free reader "+
			"skips a record it could already see (rmp #2842)", atZero, observed)
	}
}

// lifeRecordCount counts the birth and death records actually held, across
// every life shard.
func lifeRecordCount[N comparable, W any](g *Graph[N, W]) int64 {
	var n int64
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.RLock()
		n += int64(len(sh.born) + len(sh.died))
		sh.mu.RUnlock()
	}
	return n
}

// TestNodeLife_GateCountsRecordsNotWrites pins that [Graph.nodeLifeActive]
// counts the life records held, not the life writes made (rmp #2962).
//
// The store is one record deep per direction, so a second birth or death of the
// same node OVERWRITES the first. Counting that write as a new record made the
// gate drift upward for good: the reclaimers subtract one per record they free,
// so the overwritten one was never subtracted, the gate never returned to zero,
// and [MVCCStats.NodeLifeRecords] over-reported.
func TestNodeLife_GateCountsRecordsNotWrites(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	t.Cleanup(func() { _ = g.Close() })
	// Pinned so the direct writes' own reclamation frees nothing mid-sequence.
	hold := g.BeginRead()

	steps := []struct {
		name string
		op   func() error
	}{
		{"birth", func() error { return g.AddNode("a") }},
		{"death", func() error { return g.RemoveNode("a") }},
		{"revival overwrites the birth", func() error { return g.AddNode("a") }},
		{"second death overwrites the death", func() error { return g.RemoveNode("a") }},
	}
	for _, s := range steps {
		if err := s.op(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got, want := g.nodeLifeActive.Load(), lifeRecordCount(g); got != want {
			t.Errorf("after %s the gate reads %d but %d life records are held (rmp #2962)",
				s.name, got, want)
		}
	}
	if got := lifeRecordCount(g); got != 2 {
		t.Fatalf("setup: %d life records held, want 2 (one per direction)", got)
	}

	g.EndRead(hold)
	g.reclaimNodeLife(^uint64(0))
	if got := lifeRecordCount(g); got != 0 {
		t.Fatalf("setup: %d life records survive a reclaim at the maximum watermark", got)
	}
	if got := g.nodeLifeActive.Load(); got != 0 {
		t.Fatalf("every life record is reclaimed but the gate reads %d: overwrites were "+
			"counted as new records (rmp #2962)", got)
	}
	if got := g.MVCCStats().NodeLifeRecords; got != 0 {
		t.Fatalf("MVCCStats.NodeLifeRecords = %d with no life record held, want 0", got)
	}
}
