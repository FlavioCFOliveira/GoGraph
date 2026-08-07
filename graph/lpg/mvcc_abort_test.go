package lpg

// mvcc_abort_test.go — rmp #2300 AC4: a transaction that loses a write-write
// conflict ABORTS, so nothing it wrote is ever visible.
//
// # The atomicity violation this pins, measured
//
// Until [Graph.endWrite] gained its abort branch, EVERY write bracket published its
// commit record — including one whose transaction had been refused. The caller was
// told the transaction failed and part of it was visible anyway:
//
//	transaction writes n.v = 1
//	transaction hits a conflict on its second write, which is refused
//	the bracket returns mvcc.ErrSerializationConflict
//	a FRESH SNAPSHOT then reads n.v = 1        <- the violation
//
// It had not surfaced because the Cypher engine carries an undo log that physically
// restores the stored value, so on that path the chain nets out and publication is
// harmless — which is what graph/lpg/mvcc_write.go's file comment describes and why
// publishing on ROLLBACK is right. But the undo log is a cypher structure: a caller
// using [Graph.ApplyVersioned] directly has none, and the durable store's apply is
// such a caller. Rollback and abort are different things and the substrate has to be
// atomic without help.
//
// # What is deliberately NOT asserted here
//
// AC4 also asks that the aborted versions be RECLAIMABLE. They are not yet: the
// reclaimer's watermark test cannot free a chain whose head reads [mvcc.AbortedTS],
// which sits above [mvcc.TxIDBase]. That is rmp #2318 ("an aborted transaction
// retains its versions FOREVER and leaves the stored value dirty"), a separate task
// in this sprint with its own dependency on the background vacuum (rmp #2308).
// TestAbort_VersionsAreNotYetReclaimable_2318 pins the CURRENT behaviour so that
// closing #2318 has something to flip, rather than leaving the gap unrecorded.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestAbort_ConflictedTransactionIsNeverVisible is AC4's first half. It fails
// against the build that published unconditionally, reading back the refused
// transaction's write.
func TestAbort_ConflictedTransactionIsNeverVisible(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := g.SetNodeLabel("n", "Seed"); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	var record *mvcc.CommitInfo
	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		// Two writes to two DIFFERENT stores before the conflict, so the assertion
		// covers a transaction that had already spread across the substrate.
		if e := wv.SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		if e := wv.SetNodeLabel("n", "Added"); e != nil {
			return e
		}
		// Doom it exactly as a real conflict does: record one.
		if e := tx.w.conflictErr("node properties", ^uint64(0)); e == nil {
			t.Fatal("conflictErr returned nil; the transaction was not doomed")
		}
		record = tx.w.tx.OpenRecord()
		return tx.w.err()
	})
	if err == nil {
		t.Fatal("the bracket reported success for a transaction that hit a conflict")
	}
	if record == nil {
		t.Fatal("the transaction allocated no commit record, so it versioned nothing " +
			"and this test would prove nothing")
	}

	if ts := record.TS(); ts != mvcc.AbortedTS {
		t.Fatalf("the refused transaction's commit record reads %d, want mvcc.AbortedTS "+
			"(%d). A published record makes the transaction's partial work VISIBLE even "+
			"though its caller was told it failed (rmp #2300 AC4).", ts, uint64(mvcc.AbortedTS))
	}

	// The observable half: a snapshot taken after the failure must see the
	// pre-transaction state in every store the transaction touched.
	snap := g.BeginRead()
	view := g.ReadAt(snap)
	v, okV := view.GetNodeProperty("n", "v")
	labels := view.NodeLabels("n")
	g.EndRead(snap)

	if !okV {
		t.Fatal("the seeded property vanished")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Fatalf("a snapshot taken after the refused transaction reads v=%d, want the "+
			"pre-transaction 0. Part of a failed transaction is visible — an ATOMICITY "+
			"violation (rmp #2300 AC4).", got)
	}
	for _, l := range labels {
		if l == "Added" {
			t.Fatalf("a snapshot taken after the refused transaction sees its label, "+
				"labels=%v. Part of a failed transaction is visible.", labels)
		}
	}
}

// TestAbort_ASuccessfulTransactionStillPublishes is the negative control for the
// test above: the abort branch must fire ONLY for a transaction that was refused.
// Without this, returning AbortedTS unconditionally would satisfy the first test and
// make every write invisible.
func TestAbort_ASuccessfulTransactionStillPublishes(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var record *mvcc.CommitInfo
	err := g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(7)); e != nil {
			return e
		}
		record = tx.w.tx.OpenRecord()
		return nil
	})
	if err != nil {
		t.Fatalf("bracket: %v", err)
	}
	if record == nil {
		t.Fatal("the transaction allocated no commit record")
	}
	if ts := record.TS(); ts >= mvcc.TxIDBase {
		t.Fatalf("a SUCCESSFUL transaction's record reads %d, which is not a commit "+
			"timestamp (>= mvcc.TxIDBase). The abort branch is firing for a transaction "+
			"that was never refused, so no write would ever become visible.", ts)
	}

	snap := g.BeginRead()
	v, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
	g.EndRead(snap)
	if !ok {
		t.Fatal("the property vanished")
	}
	if got, _ := v.Int64(); got != 7 {
		t.Fatalf("a snapshot after a successful transaction reads v=%d, want 7", got)
	}
}

// TestAbort_AnAbortedObjectStaysWritable is the liveness half, and it is the
// regression test for a bug the abort branch itself created.
//
// Marking a refused transaction's record [mvcc.AbortedTS] fixed visibility and broke
// writability: AbortedTS sits above [mvcc.TxIDBase], so the plain
// "conflict = not visible" rule refused EVERY later writer to that object, forever.
// Measured immediately — examples/27_concurrent_txn's writers exhausted a
// nine-attempt retry chain on the first account any transfer aborted on, and
// `make ci` went red on a workload that had been green.
//
// [mvcc.Conflicts] therefore exempts an aborted head. This test drives the whole
// sequence through the substrate: abort on an object, then write it again, twice, and
// read the result back.
func TestAbort_AnAbortedObjectStaysWritable(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Abort on "n".
	_ = g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		_ = tx.w.conflictErr("node properties", ^uint64(0))
		return tx.w.err()
	})

	// Two further transactions must both succeed. Two, not one: a single success
	// could come from the aborted version having been displaced rather than
	// exempted, which would leave the SECOND writer facing it again.
	for round, want := range []int64{5, 6} {
		err := g.ApplyVersioned(func(tx WriteTx) error {
			return g.Writer(tx).SetNodeProperty("n", "v", Int64Value(want))
		})
		if err != nil {
			t.Fatalf("write %d after an abort on the same object was REFUSED: %v.\n"+
				"An aborted version heads the chain, and mvcc.AbortedTS sits above "+
				"mvcc.TxIDBase, so the plain not-visible rule refuses every later writer "+
				"and the object is permanently unwritable (rmp #2300).", round+1, err)
		}
	}

	snap := g.BeginRead()
	v, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
	g.EndRead(snap)
	if !ok {
		t.Fatal("the property vanished")
	}
	if got, _ := v.Int64(); got != 6 {
		t.Fatalf("v = %d after two writes following an abort, want 6", got)
	}
}

// TestAbort_VersionsAreWithdrawnAtAbort is the INVERSION this test asked for.
//
// It used to pin the gap: a version whose commit record reads [mvcc.AbortedTS] can
// never satisfy a reclaimer's `at() <= watermark` test, because AbortedTS sits above
// [mvcc.TxIDBase], so an aborted transaction's versions were retained for the life of
// the process. Its failure message said "invert this test and close #2318 rather than
// restoring the old behaviour", and rmp #2318 did exactly that.
//
// The withdrawal is SYNCHRONOUS, at abort, so the assertion is on what abort itself
// leaves behind rather than on what a later sweep can reach; see
// [Graph.withdrawAbortedNow] for why a present-time read leaves no correct
// asynchronous option.
func TestAbort_VersionsAreWithdrawnAtAbort(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = g.ReclaimNow()
	base := g.VersionCount()

	_ = g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		_ = tx.w.conflictErr("node properties", ^uint64(0))
		return tx.w.err()
	})

	if left := g.VersionCount() - base; left != 0 {
		t.Errorf("the aborted transaction left %d version record(s) behind, want 0", left)
	}
	// And the write it was refused for is not visible, which is what makes the
	// withdrawal a withdrawal rather than a deletion of the mask.
	v, ok := g.GetNodeProperty("n", "v")
	if !ok {
		t.Fatal("the property vanished with the aborted transaction's version")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Errorf("v = %d after an aborted write, want the pre-transaction 0", got)
	}
}
