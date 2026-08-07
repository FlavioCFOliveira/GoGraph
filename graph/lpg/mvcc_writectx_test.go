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
	// record() allocates each transaction's shared record on first use, so
	// asking both for one is what proves they are separate records rather than
	// two nil placeholders.
	if a.record() == b.record() {
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

// TestWriteCtx_ReassertingAVisibleValueDoesNotConflict is the requirement that
// keeps MERGE working, stated correctly: its MATCH branch re-asserts a property it
// just READ, so the version it re-asserts over is VISIBLE to its own transaction and
// must not be refused.
//
// # It replaced a test that asserted the rmp #2324 defect
//
// The previous form had transaction A write v=7 and stay IN FLIGHT, then had B write
// v=7 too, and asserted B was not refused — on the reasoning that a write recording
// no version has nothing to conflict over. That reasoning is what lost 46% of
// concurrent increments.
//
// B cannot have READ the 7 it wrote: A is uncommitted, so A's value is invisible to
// B. B therefore computed 7 independently, from a value A has since displaced, and
// accepting it discards B's update — or, if A aborts, leaves B's "successful" write
// with no trace at all. Refusing B is the correct answer, and the equality of the two
// values is a coincidence rather than idempotence.
//
// What MERGE actually needs is below, and it is unaffected by the fix.
func TestWriteCtx_ReassertingAVisibleValueDoesNotConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// Commit v=7 so it is visible to any transaction beginning afterwards.
	txSeed := g.beginLabelTx()
	if err := txSeed.setNodeProperty("a", "v", Int64Value(7)); err != nil {
		t.Fatalf("seed setNodeProperty: %v", err)
	}
	mustCommit(t, txSeed)

	// MERGE's shape: a transaction reads the committed 7 and re-asserts it.
	tx := g.beginLabelTx()
	if err := tx.setNodeProperty("a", "v", Int64Value(7)); err != nil {
		t.Fatalf("re-asserting a VISIBLE value was refused: %v — MERGE's MATCH branch "+
			"re-asserts properties on every match, and the version it re-asserts over is "+
			"visible to its own snapshot, so it must not conflict", err)
	}
	mustCommit(t, tx)
}

// TestWriteCtx_WritingOverAnInFlightVersionConflicts is the other half, and it is
// the rmp #2324 gate at this layer: a transaction that writes the same value another
// IN-FLIGHT transaction has written must be REFUSED, because it cannot have read that
// value and so must have computed it from a version already displaced.
func TestWriteCtx_WritingOverAnInFlightVersionConflicts(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("a", "v", Int64Value(7)); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}

	txB := g.beginLabelTx()
	if err := txB.setNodeProperty("a", "v", Int64Value(7)); err == nil {
		t.Fatal("B was allowed to write the value an in-flight transaction had just " +
			"written. B cannot have READ it — A is uncommitted and invisible to B — so B " +
			"computed it from a version A has displaced, and accepting the write discards " +
			"B's update. This is the rmp #2324 lost update: the conflict test used to be " +
			"skipped whenever the incoming value equalled the STORED one.")
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

// TestWriteCtx_RecycledStateIsNeverSharedAcrossTransactions is the pool's
// correctness gate (rmp #2301).
//
// Per-transaction state and a zero-allocation bracket pull in opposite
// directions, and the pool is what satisfies both — at the price of a state that
// outlives the transaction that used it. This drives many brackets in sequence,
// so the pool really does recycle, and asserts that nothing crosses between
// them: every bracket gets a fresh transaction id, and every bracket's write
// becomes visible at its OWN commit rather than at a neighbour's.
func TestWriteCtx_RecycledStateIsNeverSharedAcrossTransactions(t *testing.T) {
	t.Parallel()
	g := New[int, int64](adjlist.Config{Directed: true})
	// The visibility assertions below read the past through fabricated snapshots,
	// which reclamation owes nothing to. A real reader has to hold the past open;
	// see pinHorizon.
	pinHorizon(t, g)

	const brackets = 64
	ids := make(map[uint64]bool, brackets)
	commits := make([]uint64, brackets)
	for i := 0; i < brackets; i++ {
		var txID uint64
		if err := g.ApplyAtomically(func() error {
			txID = g.writerSnapshot().TxID()
			return g.SetNodeLabel(i, "L")
		}); err != nil {
			t.Fatalf("bracket %d: %v", i, err)
		}
		if txID == 0 {
			t.Fatalf("bracket %d ran with no transaction identity", i)
		}
		if ids[txID] {
			t.Fatalf("bracket %d reused transaction id %d: recycled state kept its "+
				"predecessor's identity, so two transactions publish as one", i, txID)
		}
		ids[txID] = true
		commits[i] = g.mvccClock.ReadTS()
	}

	// Every bracket's label is visible as of its own commit and NOT before it.
	// A shared record would make an earlier reader see a later bracket's work.
	for i := 0; i < brackets; i++ {
		at := g.ReadAt(&Snapshot{startTS: commits[i]})
		if !at.HasNodeLabel(i, "L") {
			t.Fatalf("bracket %d's label is invisible at its own commit %d", i, commits[i])
		}
		if i+1 < brackets && at.HasNodeLabel(i+1, "L") {
			t.Fatalf("a reader at bracket %d's commit sees bracket %d's write: the two "+
				"transactions share a commit record", i, i+1)
		}
	}

	// And the state really was recycled rather than freshly allocated each time,
	// which is the property TestBarrierGuard_ApplyAtomicallyAllocatesNothing
	// asserts from the other side.
	if g.writeTx.Load() != nil {
		t.Fatal("write state still published after the last bracket closed")
	}
}

// TestWriteCtx_ConcurrentTransactionsKeepTheirOwnRecord drives concurrent write
// transactions through the per-transaction state, which is rmp #2301's
// acceptance instrument under -race.
//
// The barrier still serialises ApplyAtomically, so the concurrency has to come
// from the substrate's own transaction type, which takes no barrier at all —
// which is precisely the shape rmp #2304 will generalise. Each transaction
// writes its own nodes, so none of them may conflict, and every one of them must
// be fully visible afterwards.
func TestWriteCtx_ConcurrentTransactionsKeepTheirOwnRecord(t *testing.T) {
	t.Parallel()
	g := New[int, int64](adjlist.Config{Directed: true})

	const (
		writers   = 24
		perWriter = 20
	)
	// Nodes exist up front so the test measures the version chains rather than
	// concurrent node creation, which the mapper serialises on its own.
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			if err := g.AddNode(w*perWriter + i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	errs := make([]error, writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			tx := g.beginLabelTx()
			for i := 0; i < perWriter; i++ {
				n := w*perWriter + i
				if err := tx.setNodeLabel(n, "L"); err != nil {
					errs[w] = err
					tx.abort()
					return
				}
				if err := tx.setNodeProperty(n, "k", StringValue("v")); err != nil {
					errs[w] = err
					tx.abort()
					return
				}
			}
			if _, err := tx.commit(); err != nil {
				errs[w] = err
			}
		}(w)
	}
	wg.Wait()

	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d touched only its own nodes and still failed: %v", w, err)
		}
	}

	// Every transaction's whole work is visible. A lost record would leave its
	// versions stamped with a transaction id no reader can pass, so the label
	// would read as absent forever.
	now := g.ReadAt(&Snapshot{startTS: g.mvccClock.ReadTS()})
	for n := 0; n < writers*perWriter; n++ {
		if !now.HasNodeLabel(n, "L") {
			t.Fatalf("node %d's label was never published: its transaction lost its "+
				"commit record", n)
		}
		v, ok := now.GetNodeProperty(n, "k")
		if !ok {
			t.Fatalf("node %d's property was never published", n)
		}
		if s, isStr := v.String(); !isStr || s != "v" {
			t.Fatalf("node %d's property read back as %v, want the string %q", n, v, "v")
		}
	}
}
