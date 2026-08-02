package lpg

// mvcc_writectx_test.go — per-transaction write state (rmp #2301), and the
// write-write conflict detection it makes sound (rmp #2300).
//
// The headline test is TestWriteCtx_DisjointDirectWritersDoNotConflict. It is
// the failure that forced the #2300 revert, reduced to its cause: detection
// that reads the writer's snapshot from a PER-GRAPH field attributes one
// transaction's snapshot to another goroutine's write, and reports a conflict
// between writers that never touched the same object.

import (
	"errors"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// mustCommit commits a transaction that is expected to succeed and returns its
// commit timestamp.
//
// It exists because commit REFUSES a transaction that hit a serialization
// conflict (rmp #2300), so every commit now has two outcomes and a test that
// meant to exercise the successful one has to say so.
func mustCommit[N comparable, W any](t *testing.T, tx *labelTx[N, W]) uint64 {
	t.Helper()
	ts, err := tx.commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ts
}

// TestWriteCtx_DisjointDirectWritersDoNotConflict is the regression gate for
// the defect that reverted rmp #2300's first wiring.
//
// 64 goroutines write disjoint nodes through the direct Go API while
// reclamation opens its own write bracket underneath them
// (reclaimAfterDirectWrite → ApplyAtomically, graph/lpg/mvcc_gc.go). With the
// snapshot read from a per-graph field, those goroutines were tested against
// the sweep's transaction and reported serialization conflicts against each
// other. With the snapshot travelling in the writeCtx, a direct write carries
// no transaction at all and cannot be tested against one.
//
// The workload is deliberately the same shape as TestGraph_Concurrent, which is
// what caught it, and heavy enough to cross the reclamation threshold so the
// bracket really does open mid-flight.
func TestWriteCtx_DisjointDirectWritersDoNotConflict(t *testing.T) {
	t.Parallel()
	g := New[int, int64](adjlist.Config{Directed: true, Multigraph: false})

	const (
		goroutines = 64
		perWorker  = 128
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				n := w*perWorker + i
				if err := g.SetNodeLabel(n, "Person"); err != nil {
					t.Errorf("SetNodeLabel(%d): %v — a direct write carries no transaction, "+
						"so it must never report a serialization conflict", n, err)
					return
				}
				if err := g.SetNodeProperty(n, "v", Int64Value(int64(i))); err != nil {
					t.Errorf("SetNodeProperty(%d): %v", n, err)
					return
				}
				// Rewrite the SAME node, which is where a per-graph snapshot
				// would find a head it could not see.
				if err := g.SetNodeLabel(n, "Active"); err != nil {
					t.Errorf("SetNodeLabel(%d, Active): %v", n, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestWriteCtx_TwoTransactionsHaveDistinctState is the property the whole task
// exists for: two overlapping write transactions hold two distinct contexts,
// and neither can observe the other's.
func TestWriteCtx_TwoTransactionsHaveDistinctState(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})

	a := g.beginWriteCtx()
	b := g.beginWriteCtx()

	if a.txID == b.txID {
		t.Fatalf("two write contexts share the transaction id %d", a.txID)
	}
	if a.info == b.info {
		t.Fatal("two write contexts share ONE commit record: publishing either would publish both")
	}
	if b.txID <= a.txID {
		t.Fatalf("transaction ids are not monotone: %d then %d", a.txID, b.txID)
	}
	// Each sees its own uncommitted work and not the other's — which is what
	// makes them separable at all.
	if !mvcc.Visible(a.txID, a.startTS, a.txID) {
		t.Fatal("a transaction cannot see its own uncommitted version")
	}
	if mvcc.Visible(b.txID, a.startTS, a.txID) {
		t.Fatal("a transaction can see another in-flight transaction's version")
	}
}

// TestWriteCtx_ConflictIsScopedToTheWritingTransaction is rmp #2300's rule, now
// sound: the conflict is decided from the writer's OWN snapshot.
func TestWriteCtx_ConflictIsScopedToTheWritingTransaction(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// A writes and does not finish.
	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}

	// B, a genuinely separate transaction, tries the same node.
	txB := g.beginLabelTx()
	err := txB.setNodeLabel("a", "FromB")
	if err == nil {
		t.Fatal("B overwrote a node that writer A is still writing, with no error: A's label " +
			"is lost the moment A commits, and nothing anywhere reports it")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("B got %v, want a serialization conflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("the error does not carry a *mvcc.Conflict: %v", err)
	}
	if c.Store != "node labels" {
		t.Fatalf("Conflict.Store = %q, want %q", c.Store, "node labels")
	}
	if !c.ConcurrentWriter() {
		t.Fatal("the blocking version belongs to an in-flight writer, so this is " +
			"first-updater-wins and must report as such")
	}
	if c.TxID != txB.ctx.txID {
		t.Fatalf("the conflict is attributed to transaction %d, but B is %d: the losing "+
			"transaction must be the one that was refused", c.TxID, txB.ctx.txID)
	}

	// A's write survives, which is the point of refusing B.
	tsA := mustCommit(t, txA)
	if !g.ReadAt(&Snapshot{startTS: tsA}).HasNodeLabel("a", "FromA") {
		t.Fatal("A's label did not survive despite B being refused")
	}
	if g.ReadAt(&Snapshot{startTS: tsA}).HasNodeLabel("a", "FromB") {
		t.Fatal("B's refused write was applied anyway")
	}
}

// TestWriteCtx_DisjointTransactionsDoNotConflict is what distinguishes conflict
// detection from a global lock wearing a new name.
func TestWriteCtx_DisjointTransactionsDoNotConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	txA := g.beginLabelTx()
	txB := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}
	if err := txB.setNodeLabel("b", "FromB"); err != nil {
		t.Fatalf("B was refused a write to a disjoint object: %v — conflict detection is "+
			"behaving as a global lock", err)
	}
	if err := txA.setNodeProperty("a", "v", Int64Value(1)); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}
	if err := txB.setNodeProperty("b", "v", Int64Value(2)); err != nil {
		t.Fatalf("B setNodeProperty on a disjoint node: %v", err)
	}
	if mustCommit(t, txA) == 0 || mustCommit(t, txB) == 0 {
		t.Fatal("a disjoint transaction failed to commit")
	}
}

// TestWriteCtx_OwnSecondWriteIsNotAConflict covers the case Memgraph's
// PrepareForWrite tests first: a transaction must be free to write the same
// object twice.
func TestWriteCtx_OwnSecondWriteIsNotAConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	tx := g.beginLabelTx()
	if err := tx.setNodeLabel("a", "One"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// The head is now this transaction's own version. It must not block it.
	if err := tx.setNodeLabel("a", "Two"); err != nil {
		t.Fatalf("a transaction was refused its own second write to the same node: %v", err)
	}
	if err := tx.setNodeProperty("a", "v", Int64Value(1)); err != nil {
		t.Fatalf("first property write: %v", err)
	}
	if err := tx.setNodeProperty("a", "v", Int64Value(2)); err != nil {
		t.Fatalf("a transaction was refused its own second property write: %v", err)
	}

	ts := mustCommit(t, tx)
	now := g.ReadAt(&Snapshot{startTS: ts})
	if !now.HasNodeLabel("a", "One") || !now.HasNodeLabel("a", "Two") {
		t.Fatal("both labels of the transaction's own two writes should be present")
	}
}

// TestWriteCtx_PropertyWriteThatChangesNothingDoesNotConflict is the guard that
// keeps MERGE working: its MATCH branch re-asserts properties on every match,
// and a write that records no version has nothing to lose and nothing to
// conflict over.
func TestWriteCtx_PropertyWriteThatChangesNothingDoesNotConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("a", "v", Int64Value(7)); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}

	txB := g.beginLabelTx()
	// B writes the value already there. No version is recorded, so there is
	// nothing to conflict over — even though A is still in flight.
	if err := txB.setNodeProperty("a", "v", Int64Value(7)); err != nil {
		t.Fatalf("a write that changes nothing was refused: %v — MERGE's MATCH branch "+
			"re-asserts properties on every match, so this would abort transactions "+
			"that wrote nothing at all", err)
	}
}

// TestWriteCtx_VoidPrimitiveConflictDoomsTheTransaction is the regression gate
// for a LOST UPDATE that the first wiring of rmp #2300 shipped.
//
// Detection records rather than returns, because several push primitives return
// nothing. The first implementation had those primitives SKIP the conflicting
// write and record nothing, on the reasoning that "the caller learns of it from
// the error its next writing call returns, or at commit". Neither was true:
// commit could not fail, so a transaction whose only conflicting write went
// through such a primitive committed successfully having silently dropped it.
//
// Measured against that build, both subtests reported the lost update — B's
// removal of L survived and B's commit returned no error. They are the proof
// that recording on the transaction, and reading it at commit, is load-bearing
// rather than defensive.
func TestWriteCtx_VoidPrimitiveConflictDoomsTheTransaction(t *testing.T) {
	t.Run("label removal", func(t *testing.T) {
		g := New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel("a", "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}

		// A writes the node and stays in flight.
		txA := g.beginLabelTx()
		if err := txA.setNodeLabel("a", "FromA"); err != nil {
			t.Fatalf("A setNodeLabel: %v", err)
		}

		// B removes a label on the same node, through a primitive that cannot
		// return an error.
		txB := g.beginLabelTx()
		txB.removeNodeLabel("a", "L")

		_, err := txB.commit()
		if err == nil {
			t.Fatal("B committed with no error after its removal was refused: the removal " +
				"is silently lost and nothing anywhere reports it")
		}
		if !errors.Is(err, mvcc.ErrSerializationConflict) {
			t.Fatalf("B's commit failed with %v, want a serialization conflict", err)
		}
		var c *mvcc.Conflict
		if !errors.As(err, &c) || c.Store != "node labels" {
			t.Fatalf("the commit error does not identify the store it came from: %v", err)
		}

		// B aborted, so nothing it wrote is visible and A's work is intact.
		tsA := mustCommit(t, txA)
		now := g.ReadAt(&Snapshot{startTS: tsA})
		if !now.HasNodeLabel("a", "L") {
			t.Fatal("B's refused removal was applied anyway")
		}
		if !now.HasNodeLabel("a", "FromA") {
			t.Fatal("A's label did not survive despite B being refused")
		}
	})

	t.Run("property delete", func(t *testing.T) {
		g := New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty("a", "p", Int64Value(1)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}

		txA := g.beginLabelTx()
		if err := txA.setNodeProperty("a", "other", Int64Value(9)); err != nil {
			t.Fatalf("A setNodeProperty: %v", err)
		}

		txB := g.beginLabelTx()
		txB.delNodeProperty("a", "p")

		_, err := txB.commit()
		if err == nil {
			t.Fatal("B committed with no error after its property delete was refused: " +
				"the delete is silently lost")
		}
		var c *mvcc.Conflict
		if !errors.As(err, &c) || c.Store != "node properties" {
			t.Fatalf("the commit error does not identify the store it came from: %v", err)
		}

		tsA := mustCommit(t, txA)
		if _, had := g.ReadAt(&Snapshot{startTS: tsA}).GetNodeProperty("a", "p"); !had {
			t.Fatal("B's refused property delete was applied anyway")
		}
	})
}

// TestWriteCtx_DoomedTransactionRefusesEveryFurtherWrite pins the second half of
// the rule: once a transaction has hit a conflict it may not keep writing.
//
// A doomed transaction is going to abort, and a write it applies in the
// meantime puts a version on a chain whose head belongs to someone else. The
// conflict reported stays the FIRST one — the one that explains the failure —
// rather than whichever object the transaction tripped over on its way out.
func TestWriteCtx_DoomedTransactionRefusesEveryFurtherWrite(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}

	txB := g.beginLabelTx()
	first := txB.setNodeLabel("a", "FromB")
	if first == nil {
		t.Fatal("B was not refused a write to the node A is still writing")
	}

	// "b" is untouched by anyone, so this write does not conflict on its own
	// merits. It must still be refused: B is doomed.
	second := txB.setNodeLabel("b", "FromB")
	if second == nil {
		t.Fatal("a doomed transaction was allowed to keep writing: its versions would " +
			"land on chains it can never publish")
	}

	var c1, c2 *mvcc.Conflict
	if !errors.As(first, &c1) || !errors.As(second, &c2) {
		t.Fatalf("untyped errors: %v / %v", first, second)
	}
	if c1 != c2 {
		t.Fatalf("the second refusal reports a different conflict (%v) from the first (%v): "+
			"the conflict that explains the failure is the first one", c2, c1)
	}

	if _, err := txB.commit(); err == nil {
		t.Fatal("a doomed transaction committed")
	}
	// B's second write must not be on the chain at all.
	tsA := mustCommit(t, txA)
	if g.ReadAt(&Snapshot{startTS: tsA}).HasNodeLabel("b", "FromB") {
		t.Fatal("a doomed transaction's write became visible")
	}
}
