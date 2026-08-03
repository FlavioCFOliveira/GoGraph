package lpg

// writer_identity_test.go — a writer has a transaction id and reads through it
// (rmp #2299).
//
// Until this task the write path read the PRESENT: [Graph.ReadAt] with a nil
// snapshot, documented as "the current stored value, no version walk". That is
// correct only while a barrier guarantees no other writer's uncommitted work
// can be in the present. [mvcc.Clock.NextTxID] had zero non-test callers, so
// the ts == txID branch of [mvcc.Visible] — the read-your-own-writes machinery —
// was fully built and entirely dead: a writer saw its own uncommitted work only
// because it was the only writer in the graph.
//
// These tests cover what replaces that: writer identity, the branch it wakes,
// isolation from another in-flight writer, and the horizon registration that
// stops reclamation freeing what the writer still needs.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestWriter_HasADistinctTransactionIdentity is acceptance criterion 1: every
// write transaction gets a distinct, monotone, never-reused id, carried on a
// snapshot the write path reads through.
func TestWriter_HasADistinctTransactionIdentity(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})

	seen := make(map[uint64]bool)
	var prev uint64
	for i := 0; i < 50; i++ {
		var got *Snapshot
		if err := g.ApplyAtomically(func() error {
			got = g.writerSnapshot()
			return nil
		}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if got == nil {
			t.Fatal("no writer snapshot inside the apply bracket")
		}
		if got.TxID() == 0 {
			t.Fatal("writer snapshot carries txID 0: the write path would read the present, " +
				"and the ts == txID branch of mvcc.Visible would stay dead")
		}
		if got.TxID() < mvcc.TxIDBase {
			t.Fatalf("txID %d is below TxIDBase %d: it could be mistaken for a commit timestamp",
				got.TxID(), mvcc.TxIDBase)
		}
		if seen[got.TxID()] {
			t.Fatalf("transaction id %d reused", got.TxID())
		}
		if got.TxID() <= prev {
			t.Fatalf("transaction id %d is not monotone after %d", got.TxID(), prev)
		}
		seen[got.TxID()], prev = true, got.TxID()
	}

	// And it is released when the bracket closes: a snapshot left behind would
	// make every later read resolve as of a stale writer.
	if g.writerSnapshot() != nil {
		t.Fatal("writer snapshot still published after the apply bracket closed")
	}
}

// TestWriter_ReadsItsOwnUncommittedWork is acceptance criterion 2: the
// ts == txID branch of [mvcc.Visible] is LIVE. Delete that branch and this test
// fails — the writer stops seeing what it just wrote.
func TestWriter_ReadsItsOwnUncommittedWork(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	tx := g.beginLabelTx()
	if err := tx.setNodeLabel("a", "Marked"); err != nil {
		t.Fatalf("setNodeLabel: %v", err)
	}

	// The writer's own snapshot: its start instant, plus its own id.
	own := &Snapshot{startTS: tx.ctx.startTS, txID: tx.ctx.txID}
	if !g.ReadAt(own).HasNodeLabel("a", "Marked") {
		t.Fatal("a writer cannot see the label it just wrote through its own snapshot: " +
			"the ts == txID branch of mvcc.Visible is not being reached")
	}

	// A reader at the same instant WITHOUT the writer's id sees nothing: the
	// label is uncommitted, and that is what makes the branch the only way in.
	plain := &Snapshot{startTS: tx.ctx.startTS}
	if g.ReadAt(plain).HasNodeLabel("a", "Marked") {
		t.Fatal("an uncommitted label is visible to a reader that is not the writer — a dirty read")
	}

	ts := mustCommit(t, tx)
	after := &Snapshot{startTS: ts}
	if !g.ReadAt(after).HasNodeLabel("a", "Marked") {
		t.Fatal("the label is not visible after commit")
	}
}

// TestWriter_DoesNotObserveAnotherInFlightWriter is acceptance criterion 4.
// Two write transactions overlap; neither sees the other's uncommitted work,
// and each sees its own.
//
// They are driven through [Graph.beginLabelTx], which is the substrate's own
// per-transaction type and carries its own commit record and id. The
// engine-level write path cannot yet be made to overlap because the write stamp
// is still a per-GRAPH singleton — moving it to per-transaction state is
// rmp #2301, and until then two overlapping engine writers are not
// representable. What is tested here is the property that matters and the one
// #2301 must preserve: identity-scoped visibility.
func TestWriter_DoesNotObserveAnotherInFlightWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	txA := g.beginLabelTx()
	txB := g.beginLabelTx()
	if txA.ctx.txID == txB.ctx.txID {
		t.Fatalf("two overlapping writers share the transaction id %d", txA.ctx.txID)
	}

	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}
	if err := txB.setNodeLabel("b", "FromB"); err != nil {
		t.Fatalf("B setNodeLabel: %v", err)
	}

	viewA := g.ReadAt(&Snapshot{startTS: txA.ctx.startTS, txID: txA.ctx.txID})
	viewB := g.ReadAt(&Snapshot{startTS: txB.ctx.startTS, txID: txB.ctx.txID})

	if !viewA.HasNodeLabel("a", "FromA") {
		t.Fatal("A cannot see its own uncommitted label")
	}
	if !viewB.HasNodeLabel("b", "FromB") {
		t.Fatal("B cannot see its own uncommitted label")
	}
	if viewA.HasNodeLabel("b", "FromB") {
		t.Fatal("A observes B's UNCOMMITTED label: a dirty read between two in-flight writers")
	}
	if viewB.HasNodeLabel("a", "FromA") {
		t.Fatal("B observes A's UNCOMMITTED label: a dirty read between two in-flight writers")
	}

	// A commits. B, whose instant predates that commit, still must not see it —
	// this is snapshot isolation, not read-committed.
	tsA := mustCommit(t, txA)
	if viewB.HasNodeLabel("a", "FromA") {
		t.Fatalf("B observes A's label committed at %d, after B began at %d", tsA, txB.ctx.startTS)
	}
	// A reader starting now sees A's work and not B's.
	now := g.ReadAt(&Snapshot{startTS: g.readTS()})
	if !now.HasNodeLabel("a", "FromA") {
		t.Fatal("a reader starting after A committed does not see A's label")
	}
	if now.HasNodeLabel("b", "FromB") {
		t.Fatal("a reader sees B's label while B is still in flight")
	}
	tsB := mustCommit(t, txB)
	if tsB <= tsA {
		t.Fatalf("B committed at %d, not after A at %d", tsB, tsA)
	}
}

// TestWriter_RegistersWithTheReclamationHorizon is acceptance criterion 5.
// A writer now READS through a snapshot, so a reclaimer must not free versions
// it can still reach. Until rmp #2299 the horizon covered readers only, which
// was sound precisely because the writer read no snapshot at all (audit finding
// E22).
func TestWriter_RegistersWithTheReclamationHorizon(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	base := g.MVCCStats().ActiveReaders
	var (
		inside     int
		insideMark uint64
		startTS    uint64
	)
	if err := g.ApplyAtomically(func() error {
		s := g.MVCCStats()
		inside = s.ActiveReaders
		insideMark = s.Watermark
		startTS = g.writerSnapshot().StartTS()
		return nil
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if inside != base+1 {
		t.Fatalf("ActiveReaders %d inside a write bracket, want %d: the writer did not register "+
			"with the horizon, so a reclaimer may free versions it is still reading", inside, base+1)
	}
	if insideMark > startTS {
		t.Fatalf("watermark %d inside a write bracket whose snapshot starts at %d: reclamation "+
			"is allowed past the writer's own instant", insideMark, startTS)
	}
	if got := g.MVCCStats().ActiveReaders; got != base {
		t.Fatalf("ActiveReaders %d after the bracket closed, want %d: the writer's horizon slot "+
			"was not returned", got, base)
	}
}

// TestWriter_HoldsReclamationBackWhileItRuns is the behavioural half of
// criterion 5: a version the running writer can still reach is not reclaimed
// out from under it.
func TestWriter_HoldsReclamationBackWhileItRuns(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// Churn so there is history to reclaim.
	for i := 0; i < 200; i++ {
		if err := g.SetNodeLabel("a", "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		g.RemoveNodeLabel("a", "L")
	}

	if err := g.ApplyAtomically(func() error {
		snap := g.writerSnapshot()
		if snap == nil {
			t.Fatal("no writer snapshot")
			return nil
		}
		// A sweep driven from inside the bracket must not pass the writer's own
		// instant, whatever it frees below it.
		g.ReclaimNow()
		if wm := g.MVCCStats().Watermark; wm > snap.StartTS() {
			t.Fatalf("watermark %d after a sweep inside a write bracket starting at %d",
				wm, snap.StartTS())
		}
		// And the writer can still read: whatever was freed, it was not
		// something this snapshot could reach.
		_ = g.ReadAt(snap).HasNodeLabel("a", "L")
		return nil
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}
