package cypher_test

// constraint_label_conflict_test.go — regression tests for rmp #2354, an ACID
// ATOMICITY defect: a committed `SET n:Label` (or `REMOVE n:Label`) could write
// NOTHING, because the node-label store's write-write conflict test sat INSIDE the
// already-has-the-label guard.
//
// # The defect
//
// graph/lpg/lpg.go read the label bag out of the shard map BY VALUE and then
// guarded both the conflict test and the delta on it:
//
//	if g.labelDeltasEnabled() && !bag.has(lid) { ...conflict test...; ...delta... }
//
// The bag is the RAW stored bag, so it already carries other in-flight
// transactions' eager writes. A peer's UNCOMMITTED add of the same label therefore
// made has() true, skipped the whole block, and left bag.add as a no-op — so the
// transaction committed having recorded nothing:
//
//	T1: SET n:Acct   (eager write puts :Acct in the raw bag), uncommitted
//	T2: SET n:Acct   → has() true ⇒ no conflict, no delta, no write
//	T2: COMMIT       → nil
//	T1: ROLLBACK     → its undo strips :Acct
//	final: n carries NO :Acct, yet T2 was told it committed.
//
// This is the IDENTICAL defect rmp #2324 fixed for the node-PROPERTY store, where
// the same reasoning ("only a write that RECORDS a version can conflict") was
// measured producing 216 from 400 concurrent increments. The label store kept the
// guard; these tests pin that it no longer does.
//
// The REMOVE direction had the mirror of it, and one case worse: when the peer's
// eager removal took the node's LAST label the bag entry was deleted outright, so
// not even the has() guard was reached.
//
// # What these tests pin, and why each is here
//
// A fix that simply conflicted on every label write would satisfy every negative
// below, so each is paired with a positive control:
//
//   - both orderings of add/add are refused rather than silently lost;
//   - remove/remove is refused, in BOTH the label-among-others and the
//     only-label (bag-entry-deleted) shapes;
//   - a transaction re-asserting a label it added ITSELF is still ACCEPTED — this
//     is MERGE's ON MATCH shape and the reason the guard existed at all;
//   - two transactions labelling DIFFERENT nodes both commit, so the fix did not
//     turn the label store into a global lock;
//   - the refusal is attributed to the "node labels" store, not to some other one.
//
// The openCypher TCK does not cover concurrent transactions, so nothing here is —
// or is claimed as — TCK coverage.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// newLabelConflictGraph builds an in-memory engine over a graph the caller keeps a
// handle on — needed only by the tests that read [lpg.Graph.MVCCStats] — and runs
// the setup statements, each in its own transaction.
func newLabelConflictGraph(t *testing.T, setup ...string) (*lpg.Graph[string, float64], *cypher.Engine, context.Context) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, q := range setup {
		mustRunTx(t, ctx, eng, q)
	}
	return g, eng, ctx
}

// newLabelConflictEngine builds an in-memory engine and runs the setup statements,
// each in its own transaction.
func newLabelConflictEngine(t *testing.T, setup ...string) (*cypher.Engine, context.Context) {
	t.Helper()
	_, eng, ctx := newLabelConflictGraph(t, setup...)
	return eng, ctx
}

// execInTx runs q inside tx and drains the result, returning the first error from
// either channel. Draining matters: a write-operator error is reported on
// Result.Err and not by Exec, so asserting only on Exec's return reads every
// refusal as success.
func execInTx(tx *cypher.ExplicitTx, q string) error {
	r, err := tx.Exec(q, nil)
	if err != nil {
		return err
	}
	for r.Next() { // draining is the point
	}
	if rerr := r.Err(); rerr != nil {
		_ = r.Close()
		return rerr
	}
	return r.Close()
}

// beginTwo opens two overlapping explicit transactions and registers cleanup that
// rolls back whatever the test leaves open.
func beginTwo(t *testing.T, ctx context.Context, eng *cypher.Engine) (t1, t2 *cypher.ExplicitTx) { //nolint:revive // t first
	t.Helper()
	t1, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx (T1): %v", err)
	}
	t2, err = eng.BeginTx(ctx)
	if err != nil {
		_ = t1.Rollback()
		t.Fatalf("BeginTx (T2): %v", err)
	}
	t.Cleanup(func() {
		_ = t1.Rollback()
		_ = t2.Rollback()
	})
	return t1, t2
}

// assertLabelStoreConflict asserts err is a serialization conflict attributed to
// the node-label store. An empty attribution would still satisfy errors.Is, so the
// store name is checked explicitly.
func assertLabelStoreConflict(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a serialization conflict, got nil", what)
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("%s: expected mvcc.ErrSerializationConflict, got %v", what, err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("%s: expected a typed *mvcc.Conflict, got %T: %v", what, err, err)
	}
	if c.Store != mvcc.StoreNodeLabels {
		t.Fatalf("%s: conflict attributed to store %q, want %q", what, c.Store, mvcc.StoreNodeLabels)
	}
	// A refusal whose ROLLBACK failed is an atomicity violation even when the final
	// state happens to come out right, and it is reported by wrapping the conflict
	// rather than by replacing it — so every check above would still pass. Asserted
	// here because that is exactly how a wrong `hadLabel` shows up: the undo journals
	// an inverse for a write that never happened, the replay is refused, and only
	// this sentinel says so. See
	// TestLabelStore_DoomedRemoveDoesNotResurrectACommittedRemoval.
	if errors.Is(err, cypher.ErrUndoFailed) {
		t.Fatalf("%s: the rollback of the refused transaction FAILED: %v", what, err)
	}
}

// TestLabelStore_ConflictIsAttributedInMVCCStats pins the OBSERVABILITY half: an
// operator who sees the workload contending must be able to read WHICH structure it
// contends on. The typed error already carries the store, but the error reaches only
// the caller who tripped it; the counter series is what a running deployment
// exports.
//
// Both halves are asserted, because either alone is satisfiable by an accident:
// a total with no attribution says the engine contends but not on what, and an
// attribution without the total would not have counted a transaction at all.
//
// The counter counts DOOMED TRANSACTIONS, not detections — the increment sits on the
// winning CAS in writeCtx.conflictErr — so exactly one is expected here however many
// writes the doomed transaction went on to attempt.
func TestLabelStore_ConflictIsAttributedInMVCCStats(t *testing.T) {
	g, eng, ctx := newLabelConflictGraph(t, `CREATE (n:Plain {k:'n1'})`)
	labelIdx := mvcc.ConflictStoreIndex(mvcc.StoreNodeLabels)

	before := g.MVCCStats().Write
	t1, t2 := beginTwo(t, ctx, eng)

	const set = `MATCH (n:Plain {k:'n1'}) SET n:Acct`
	if err := execInTx(t1, set); err != nil {
		t.Fatalf("T1 SET: %v", err)
	}
	assertLabelStoreConflict(t, execInTx(t2, set), "T2 SET n:Acct")

	after := g.MVCCStats().Write
	if got := after.Conflicts - before.Conflicts; got != 1 {
		t.Errorf("total doomed transactions: got %d, want 1", got)
	}
	if got := after.ByStore[labelIdx] - before.ByStore[labelIdx]; got != 1 {
		t.Errorf("conflicts attributed to %q: got %d, want 1", mvcc.StoreNodeLabels, got)
	}
	// And the attribution is EXCLUSIVE: a fix that recorded the conflict against
	// several stores, or against the catch-all, would still satisfy the two checks
	// above.
	for i := range after.ByStore {
		if i == labelIdx {
			continue
		}
		if d := after.ByStore[i] - before.ByStore[i]; d != 0 {
			t.Errorf("store %q also counted %d conflict(s); the attribution must be exclusive",
				mvcc.ConflictStoreName(i), d)
		}
	}
}

// TestLabelStore_DoomedRemoveDoesNotResurrectACommittedRemoval pins the atomicity
// of a REFUSED transaction's rollback against a peer that COMMITTED in between:
//
//	T1: REMOVE n:Acct     -- eager removal, uncommitted
//	T2: REMOVE n:Acct     -- doomed by the conflict; the write is SKIPPED
//	T1: COMMIT            -- the removal is now committed and legitimate
//	T2: COMMIT            -> refused, and its rollback must undo only what IT did
//
// T1's committed removal must stand, and T2's unrelated labels must be untouched.
//
// # Why this test exists here, and the negative finding it carries
//
// rmp #2354's technical requirements also asked for `hadLabel` — the adapter's
// pre-call check at cypher/api.go SetNodeLabel/RemoveNodeLabel — to be resolved
// through the committing transaction's view (HasNodeLabelInTx) rather than the raw
// graph, so a peer's uncommitted label add could not suppress this transaction's
// NOT NULL touch.
//
// That change was implemented and MEASURED, and it is NEITHER needed NOR harmful —
// it is unreachable. The raw/tx-view divergence is real (probed here: for T2 the tx
// view says the label is present while the raw bag says absent), but any such
// divergence means node n's delta chain carries a head that does not belong to this
// transaction, and the now-unconditional conflict test dooms the transaction for
// exactly that reason. A doomed transaction never reaches the NOT NULL check, so the
// suppression the requirement aimed at cannot occur. `hadLabel` was therefore left
// as the raw read: it is the read the PHYSICAL undo journal wants, since the journal
// un-applies eager mutations on the shared structure and so must describe what this
// transaction actually did to it.
//
// One thing that measurement did surface, and it is why the ErrUndoFailed assertion
// exists in assertLabelStoreConflict: with the tx-visible read the journal records
// an inverse for a write that never happened, that inverse is then REFUSED at replay
// — and cypher/undo_record.go discards its error (`_ = m.wv.SetNodeLabel(...)`), so
// the rollback reports success either way. The final state came out right by
// accident, not by construction. Under the raw read the journal can only ever hold
// inverses of writes this transaction really made over a head it still owns, so the
// replay cannot be refused and the discarded error is unreachable. That is a
// property of the raw read, not of the undo layer, and it is the second reason to
// keep it.
func TestLabelStore_DoomedRemoveDoesNotResurrectACommittedRemoval(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, `CREATE (n:Acct:Extra {k:'n1'})`)
	t1, t2 := beginTwo(t, ctx, eng)

	const remove = `MATCH (n {k:'n1'}) REMOVE n:Acct`
	if err := execInTx(t1, remove); err != nil {
		t.Fatalf("T1 REMOVE: %v", err)
	}
	// Void primitive: the conflict is recorded, not returned. See
	// TestLabelStore_ConcurrentRemove.
	_ = execInTx(t2, remove)

	// T1 commits FIRST, so its removal is committed and legitimate.
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 COMMIT must succeed: %v", err)
	}
	// T2 is refused, and its rollback must not undo a write it never made.
	assertLabelStoreConflict(t, t2.Commit(), "T2 COMMIT after a colliding REMOVE")

	if got := countQ(t, ctx, eng, `MATCH (n:Acct) WHERE n.k='n1' RETURN count(n) AS c`); got != 0 {
		t.Fatalf("T1's COMMITTED removal was undone by T2's rollback: :Acct count=%d, want 0", got)
	}
	// The node itself, and its other label, are untouched.
	if got := countQ(t, ctx, eng, `MATCH (n:Extra) WHERE n.k='n1' RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Extra count=%d, want 1 — the rollback damaged an unrelated label", got)
	}
}

// TestLabelStore_ConcurrentAddThenPeerRollback is the defect itself: a peer's
// UNCOMMITTED add of the same label must not swallow this transaction's write.
//
// The assertion is deliberately "refused OR survived", not "refused": either
// outcome is correct MVCC. What is forbidden is the third one the defect produced
// — a nil commit whose write is absent from the committed state.
func TestLabelStore_ConcurrentAddThenPeerRollback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		firstWriter string // which transaction issues its SET first
	}{
		{name: "T1 writes first", firstWriter: "T1"},
		{name: "T2 writes first", firstWriter: "T2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, ctx := newLabelConflictEngine(t, `CREATE (n:Plain {k:'n1'})`)
			t1, t2 := beginTwo(t, ctx, eng)

			const set = `MATCH (n:Plain {k:'n1'}) SET n:Acct`
			first, second := t1, t2
			if tc.firstWriter == "T2" {
				first, second = t2, t1
			}

			if err := execInTx(first, set); err != nil {
				t.Fatalf("the FIRST writer must succeed, got %v", err)
			}
			secondWrite := execInTx(second, set)

			// The second writer must be REFUSED, and with the right attribution.
			assertLabelStoreConflict(t, secondWrite, "second writer's SET n:Acct")

			// The first writer rolls back; nothing of it may remain...
			if err := first.Rollback(); err != nil {
				t.Fatalf("first.Rollback: %v", err)
			}
			// ...and the refused transaction must not be able to commit as if its
			// write had landed. That is the lost update, and it is what the defect
			// allowed: a nil commit with an empty effect.
			if err := second.Commit(); err == nil {
				acct := countQ(t, ctx, eng, `MATCH (n:Acct) WHERE n.k='n1' RETURN count(n) AS c`)
				if acct == 0 {
					t.Fatal("LOST UPDATE: the refused transaction committed with nil and its " +
						"label is absent from the committed state")
				}
			}
		})
	}
}

// TestLabelStore_ConcurrentRemove covers the REMOVE direction, in both shapes.
//
// The only-label shape is the one that was worse: the peer's eager removal took
// the node's last label, which DELETED the shard's bag entry, so the conflict test
// was not reached even by the guard that existed.
func TestLabelStore_ConcurrentRemove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
	}{
		{
			name:  "label among others",
			setup: `CREATE (n:Acct:Extra {k:'n1'})`,
		},
		{
			name:  "only label, so the peer's removal deletes the bag entry",
			setup: `CREATE (n:Acct {k:'n1'})`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, ctx := newLabelConflictEngine(t, tc.setup)
			t1, t2 := beginTwo(t, ctx, eng)

			const remove = `MATCH (n {k:'n1'}) REMOVE n:Acct`
			if err := execInTx(t1, remove); err != nil {
				t.Fatalf("T1 REMOVE must succeed, got %v", err)
			}

			// REMOVE reaches the graph through a VOID primitive
			// (Graph.removeNodeLabelInfo returns nothing), so the conflict is
			// RECORDED on the transaction rather than surfaced by the statement, and
			// the statement legitimately returns nil. That is the documented contract
			// — see graph/lpg/lpg.go removeNodeLabelInfo and
			// TestWriteCtx_VoidPrimitiveConflictDoomsTheTransaction — and the reason
			// the recording exists at all: without it the removal would be dropped
			// and the transaction would commit as if it had happened.
			//
			// So COMMIT is the assertion point here, not the write. Asserting on the
			// write instead is how this test first read the working fix as a failure.
			t2Write := execInTx(t2, remove)
			t.Logf("T2 REMOVE statement err = %v (nil is correct: void primitive)", t2Write)

			commitErr := t2.Commit()
			if commitErr == nil {
				t.Fatal("LOST UPDATE: T2's COMMIT succeeded although its REMOVE collided " +
					"with a peer's uncommitted removal of the same label")
			}
			// Not merely non-nil: it must be the TYPED conflict, attributed to the
			// node-label store. A commit that failed for some unrelated reason would
			// satisfy a bare non-nil check and leave the defect unpinned.
			assertLabelStoreConflict(t, commitErr, "T2 COMMIT after a colliding REMOVE")
			t.Logf("T2 COMMIT err = %v", commitErr)

			// T1 rolls back, so the label must still be there: neither transaction
			// removed it.
			if err := t1.Rollback(); err != nil {
				t.Fatalf("t1.Rollback: %v", err)
			}
			if got := countQ(t, ctx, eng, `MATCH (n:Acct) WHERE n.k='n1' RETURN count(n) AS c`); got != 1 {
				t.Fatalf("both transactions failed, so :Acct must survive: count=%d, want 1", got)
			}
		})
	}
}

// TestLabelStore_AutocommitRemoveIsRefused covers the OTHER half of rmp #2354's
// backstop, which every test above leaves unexercised.
//
// The conflict backstop exists in two places, because two code paths drive the write
// bracket themselves: cypher/exectx.go for ExplicitTx.Commit, and cypher/api.go
// commitUnderBarrier for the autocommit RunInTx path. The tests above all go through
// ExplicitTx, so without this one half the fix is unpinned — and it is the half the
// common caller uses.
//
// Reachability was not assumed. An ExplicitTx looked as though it might hold the
// write barrier exclusively, which would make the autocommit half dead code;
// measurement says otherwise — the autocommit statement below runs to a verdict while
// T1 is still open, so the two genuinely interleave.
//
// REMOVE is the operation that needs the backstop: it reaches the graph through a
// VOID primitive, so the conflict is recorded rather than returned and nothing else
// would report it. A SET would surface at the statement.
func TestLabelStore_AutocommitRemoveIsRefused(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, `CREATE (n:Acct:Extra {k:'n1'})`)

	t1, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx (T1): %v", err)
	}
	t.Cleanup(func() { _ = t1.Rollback() })

	const remove = `MATCH (n {k:'n1'}) REMOVE n:Acct`
	if err := execInTx(t1, remove); err != nil {
		t.Fatalf("T1 REMOVE must succeed: %v", err)
	}

	// The autocommit statement collides with T1's uncommitted removal. It must be
	// refused; a nil verdict here is the lost update, since its write was skipped.
	res, runErr := eng.RunInTx(ctx, remove, nil)
	verdict := runErr
	if res != nil {
		if verdict == nil {
			verdict = res.Err()
		}
		_ = res.Close()
	}
	assertLabelStoreConflict(t, verdict, "autocommit REMOVE over a peer's uncommitted removal")

	// T1 rolls back, so neither transaction removed the label and it must survive.
	if err := t1.Rollback(); err != nil {
		t.Fatalf("t1.Rollback: %v", err)
	}
	if got := countQ(t, ctx, eng, `MATCH (n:Acct) WHERE n.k='n1' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("both transactions failed, so :Acct must survive: count=%d, want 1", got)
	}
}

// TestLabelStore_SelfReassertIsAccepted is the positive control that gives every
// refusal above its meaning, and it guards the very case the removed guard
// existed for: MERGE's ON MATCH branch re-asserts labels on every match.
//
// A transaction re-asserting a label over its OWN head must not be refused as its
// own conflict.
func TestLabelStore_SelfReassertIsAccepted(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, `CREATE (n:Plain {k:'n1'})`)

	// Same label twice in one statement, then again in a second statement of the
	// same transaction, then a MERGE that re-asserts on match.
	mustRunTx(t, ctx, eng, `MATCH (n:Plain {k:'n1'}) SET n:Acct:Acct`)
	mustRunTx(t, ctx, eng, `MATCH (n:Plain {k:'n1'}) SET n:Acct SET n:Acct`)
	mustRunTx(t, ctx, eng, `MERGE (n:Plain {k:'n1'}) ON MATCH SET n:Acct`)

	if got := countQ(t, ctx, eng, `MATCH (n:Acct) WHERE n.k='n1' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("self re-assert left :Acct on %d nodes, want 1", got)
	}

	// And inside ONE explicit transaction, across statements.
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for _, q := range []string{
		`MATCH (n:Plain {k:'n1'}) SET n:Acct`,
		`MATCH (n:Plain {k:'n1'}) SET n:More`,
		`MATCH (n:Plain {k:'n1'}) SET n:Acct`,
	} {
		if err := execInTx(tx, q); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s: a transaction must never conflict with itself: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit of a self-consistent transaction: %v", err)
	}
}

// TestLabelStore_DifferentNodesDoNotConflict is the control against the other way
// a fix could be wrong: making the conflict test unconditional must not make the
// label store behave like a global lock.
func TestLabelStore_DifferentNodesDoNotConflict(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t,
		`CREATE (a:Plain {k:'a'})`,
		`CREATE (b:Plain {k:'b'})`)
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, `MATCH (n:Plain {k:'a'}) SET n:Acct`); err != nil {
		t.Fatalf("T1 on node a: %v", err)
	}
	if err := execInTx(t2, `MATCH (n:Plain {k:'b'}) SET n:Acct`); err != nil {
		t.Fatalf("T2 on node b must NOT conflict with T1 on node a: %v", err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("T2 commit: %v", err)
	}
	if got := countQ(t, ctx, eng, `MATCH (n:Acct) RETURN count(n) AS c`); got != 2 {
		t.Fatalf("both disjoint transactions should have committed: :Acct count=%d, want 2", got)
	}
}

// TestLabelStore_PeerUncommittedLabelDoesNotSuppressNotNullCheck closes the second
// consequence of the same raw read (audit GAP-2).
//
// The adapter's pre-check `hadLabel := a.g.HasNodeLabel(n, label)` is a raw
// present-time read, and cypher/undo_record.go recordSetNodeLabel returns EARLY
// when it is true — which would remove the node from this transaction's
// commit-time NOT NULL check. A peer's UNCOMMITTED add of the constrained label
// could therefore let a violating state through.
//
// With the conflict test now unconditional the interleaving is refused before that
// matters, which is what this pins: the committed state must never carry the
// constrained label without the required property.
func TestLabelStore_PeerUncommittedLabelDoesNotSuppressNotNullCheck(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t,
		`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`,
		`CREATE (n:Plain {k:'n1'})`) // NO email property
	t1, t2 := beginTwo(t, ctx, eng)

	const set = `MATCH (n:Plain {k:'n1'}) SET n:Acct`
	// T1's own commit-time check will reject this (no email), but its EAGER write
	// lands in the raw bag first, which is what T2 would have read.
	if err := execInTx(t1, set); err != nil {
		t.Fatalf("T1 SET label (eager write should succeed; the check is at commit): %v", err)
	}
	t2Write := execInTx(t2, set)
	assertLabelStoreConflict(t, t2Write, "T2 SET n:Acct over a peer's uncommitted add")

	_ = t1.Commit() // must fail its NOT NULL check
	_ = t2.Commit()

	violating := countQ(t, ctx, eng,
		`MATCH (n:Acct) WHERE n.email IS NULL RETURN count(n) AS c`)
	if violating != 0 {
		t.Fatalf("CONSISTENCY: %d node(s) committed carrying :Acct with no email", violating)
	}
}
