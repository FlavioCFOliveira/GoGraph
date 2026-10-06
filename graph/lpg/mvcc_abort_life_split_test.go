package lpg

// mvcc_abort_life_split_test.go — an aborted birth and death of one node are
// withdrawn together even when the abort lands in the middle of a reclaim pass
// (rmp #2949).
//
// Layer: short.

import (
	"context"
	"testing"
)

// TestReclaimAbortedLife_AbortBetweenTheLoopsKeepsARolledBackDeleteAlive drives
// the interleaving the straddler enumeration met at about one run in 20000: a
// transaction deletes a node and its rollback's undo revives it, the
// transaction is doomed by a conflict elsewhere, and a reclaim pass is already
// under way when the transaction's commit record is aborted — after the pass's
// birth loop passed over the node's revival as not yet aborted and before its
// death loop reads the death. The death was then withdrawn alone, and the
// revival, withdrawn by the next pass as if it were a create, tombstoned a node
// that was alive before the transaction.
func TestReclaimAbortedLife_AbortBetweenTheLoopsKeepsARolledBackDeleteAlive(t *testing.T) {
	ctx := context.Background()
	g, id := lifeGraph(t)
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("b") }); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	tx1 := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		_, _ = g.Writer(tx).RemoveNode("a")
		return nil
	}); err != nil {
		t.Fatalf("tx1 delete: %v", err)
	}
	// The rollback's undo replay revives what the delete tombstoned.
	if err := g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error {
		_ = g.Writer(tx).Revive("a") // a transaction records its refusal on itself
		return nil
	}); err != nil {
		t.Fatalf("tx1 undo revive: %v", err)
	}
	// A peer commits a write to b after tx1 began, and tx1 then writes b: tx1
	// is doomed, so it ends by aborting rather than by publishing.
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).SetNodeLabel("b", "X") }); err != nil {
		t.Fatalf("peer: %v", err)
	}
	_ = g.ApplyInVersionedTx(ctx, tx1, func(tx WriteTx) error { return g.Writer(tx).SetNodeLabel("b", "Y") })
	if tx1.w.err() == nil {
		t.Fatal("tx1 is not doomed, so it would publish and never reach the abort reclaim")
	}
	born, hasBorn, died, hasDied := lifePair(g, id)
	if !hasBorn || !hasDied || !born.wasAlive || born.info != died.info {
		t.Fatalf("the rolled-back delete left no revival pair: hasBorn=%v hasDied=%v wasAlive=%v", hasBorn, hasDied, born.wasAlive)
	}

	// The abort lands inside a reclaim pass, between the loops of a's shard,
	// after the birth loop has passed over a's revival as still pending.
	target := g.nodeLifeShardFor(id)
	fired, bornPassedPending := false, false
	g.reclaimAbortedLifeHookForTest = func(sh *nodeLifeShard) {
		if sh != target || fired {
			return
		}
		fired = true
		b, ok := sh.born[id] // the hook runs under sh's lock
		bornPassedPending = ok && b.at() != 0 && b.at() < ^uint64(0)
		tx1.w.record().Abort()
	}
	// The full sweep: an abort withdraws its own records by its write set, and
	// falls back to this sweep when that set is incomplete.
	g.withdrawAbortedAll()
	g.reclaimAbortedLifeHookForTest = nil
	if !fired || !bornPassedPending {
		t.Fatalf("the interleaving was not driven: hook fired=%v, birth passed over as pending=%v", fired, bornPassedPending)
	}
	// tx1 ends; its abort runs the next reclaim pass.
	g.EndVersionedTx(tx1)

	if _, hasBorn, _, hasDied := lifePair(g, id); hasBorn || hasDied {
		t.Errorf("the aborted transaction's life records survived: hasBorn=%v hasDied=%v", hasBorn, hasDied)
	}
	if g.IsTombstoned(id) {
		t.Error("the node of a rolled-back DELETE is tombstoned: its deletion was withdrawn alone and its revival was taken for an aborted create")
	}
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if !g.NodeExistsAsOf(id, snap) {
		t.Error("a reader after the rollback does not see the node")
	}
}
