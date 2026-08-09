package cypher_test

// constraint_writeskew_test.go — regression tests for rmp #2353, an ACID
// CONSISTENCY defect: two transactions writing through DIFFERENT substores of the
// same node jointly commit a state that violates a declared NOT NULL constraint,
// though neither transaction violates it on its own snapshot.
//
// # The anomaly
//
//	CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL
//	CREATE (n:Plain {k:'n1', email:'x'})   -- has the property, NOT the label
//	T1: REMOVE n.email    -- property substore
//	T2: SET n:Acct        -- label substore
//	T1: COMMIT -> nil
//	T2: COMMIT -> nil
//	final: 1 node carrying :Acct with NO email.
//
// Neither transaction is wrong on its own view, which is why both are admitted:
// T1 removed the property and sees no constrained label on the node, so it has
// nothing to check; T2 added the label and its snapshot predates T1's commit, so it
// still sees email='x'. The violation exists only in the MERGED state.
//
// Conflict detection does not catch it either, because conflicts are per SUBSTORE:
// node labels and node properties carry separate delta chains and separate head
// stamps (mvcc.StoreNodeLabels vs mvcc.StoreNodeProperties). Two transactions
// touching the same node through different substores never collide.
//
// This is the classic write-skew anomaly, which plain Snapshot Isolation permits by
// definition — two adjacent anti-dependencies. It is nevertheless a breach of this
// project's ACID CONSISTENCY mandate, which requires every committed transaction to
// leave the graph satisfying every declared invariant.
//
// The openCypher TCK does not cover concurrent transactions, so nothing here is —
// or is claimed as — TCK coverage.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// notNullSetup is the fixture both directions of the skew start from: a constraint
// on :Acct.email, and a node that carries the property but not yet the label.
var notNullSetup = []string{
	`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`,
	`CREATE (n:Plain {k:'n1', email:'x'})`,
}

const (
	dropProperty = `MATCH (n {k:'n1'}) REMOVE n.email`
	addLabel     = `MATCH (n:Plain {k:'n1'}) SET n:Acct`
)

// TestConstraint_NotNullViolationInOneTransactionIsRejected is the ORACLE CONTROL,
// and it must be read before any of the skew assertions mean anything: it proves the
// commit-time existence check can see this violation AT ALL.
//
// Without it, a skew test that observed "no violation committed" would be satisfied
// by a checker that never fires.
func TestConstraint_NotNullViolationInOneTransactionIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{name: "drop property then add label", stmts: []string{dropProperty, addLabel}},
		{name: "add label then drop property", stmts: []string{addLabel, dropProperty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, ctx := newLabelConflictEngine(t, notNullSetup...)

			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, q := range tc.stmts {
				if err := execInTx(tx, q); err != nil {
					t.Fatalf("%s: the check belongs at COMMIT, not at the statement: %v", q, err)
				}
			}
			if err := tx.Commit(); err == nil {
				t.Fatal("ORACLE BROKEN: one transaction committed :Acct with no email; " +
					"every write-skew assertion in this file is worthless until this passes")
			}

			if got := countQ(t, ctx, eng,
				`MATCH (n:Acct) WHERE n.email IS NULL RETURN count(n) AS c`); got != 0 {
				t.Fatalf("%d node(s) committed carrying :Acct with no email", got)
			}
		})
	}
}

// TestConstraint_NotNullWriteSkewAcrossSubstores is the defect: the same violation,
// split across two transactions and two substores.
//
// Both orderings of the COMMITS are covered, because the anomaly is symmetric —
// whichever transaction commits second is the one whose snapshot is stale.
func TestConstraint_NotNullWriteSkewAcrossSubstores(t *testing.T) {
	for _, tc := range []struct {
		name        string
		firstCommit string // which transaction commits first
	}{
		{name: "property-remover commits first", firstCommit: "T1"},
		{name: "label-adder commits first", firstCommit: "T2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, eng, ctx := newLabelConflictGraph(t, notNullSetup...)
			before := g.MVCCStats().Write
			t1, t2 := beginTwo(t, ctx, eng)

			// T1 works the PROPERTY substore, T2 the LABEL substore. Neither
			// statement may fail: the constraint is a commit-time check.
			if err := execInTx(t1, dropProperty); err != nil {
				t.Fatalf("T1 REMOVE n.email: %v", err)
			}
			if err := execInTx(t2, addLabel); err != nil {
				t.Fatalf("T2 SET n:Acct: %v", err)
			}

			first, second := t1, t2
			if tc.firstCommit == "T2" {
				first, second = t2, t1
			}
			firstErr := first.Commit()
			secondErr := second.Commit()

			// EXACTLY ONE must commit. Which one is not specified — either
			// serialisation order is legal — but they cannot both succeed.
			committed := 0
			if firstErr == nil {
				committed++
			}
			if secondErr == nil {
				committed++
			}
			if committed == 2 {
				t.Errorf("WRITE SKEW: both transactions committed (first=%v second=%v)", firstErr, secondErr)
			}
			if committed == 0 {
				t.Errorf("both transactions were refused; one of them must be able to make progress "+
					"(first=%v second=%v)", firstErr, secondErr)
			}

			// THE COMMITTED STATE SATISFIES THE INVARIANT. Asserted FIRST and with
			// Errorf, deliberately: this is the ACID CONSISTENCY property the task is
			// about, and the oracle control above proves the query can observe its
			// violation. Assert it after a Fatalf-style check and a failure elsewhere
			// hides whether the graph was actually corrupted — which is exactly what
			// happened on the first run of this test.
			//
			// THIS ASSERTION ALSO PINS A SECOND FIX, and is the only test that does:
			// the undo of the refused transaction's label add must leave no stale
			// label-index candidate, or this label scan yields a node whose labels do
			// not include :Acct. See deferLabelIndexRemoval's `undoing` exemption in
			// graph/lpg/mvcc_index.go and the control
			// TestLabelIndex_NotNullRefusalLeavesNoStaleCandidate below. Verified by
			// reverting that exemption alone, which fails exactly here.
			if got := countQ(t, ctx, eng,
				`MATCH (n:Acct) WHERE n.email IS NULL RETURN count(n) AS c`); got != 0 {
				t.Errorf("CONSISTENCY: %d node(s) committed carrying :Acct with no email", got)
			}

			// A real conflict, not a lucky interleaving: the counter must have moved.
			after := g.MVCCStats().Write
			if d := after.Conflicts - before.Conflicts; d != 1 {
				t.Errorf("doomed transactions counted: got %d, want 1", d)
			}

			// The refusal must be the module's own write-write conflict, so a caller
			// retries through the path it already has rather than meeting a new
			// failure mode. A constraint violation reported to the WRONG transaction
			// would also leave the graph consistent, so this is asserted separately.
			refusal := firstErr
			if refusal == nil {
				refusal = secondErr
			}
			assertSerializationConflict(t, refusal, "the refused transaction")
		})
	}
}

// TestConstraint_DisjointNodesUnderConstraintDoNotConflict is the control against
// the lazy fix: making constrained nodes conflict must not make EVERY write to a
// constrained label conflict.
//
// Two transactions doing the very same work on DIFFERENT nodes must both commit.
func TestConstraint_DisjointNodesUnderConstraintDoNotConflict(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t,
		`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`,
		`CREATE (a:Plain {k:'a', email:'x'})`,
		`CREATE (b:Plain {k:'b', email:'y'})`)
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, `MATCH (n:Plain {k:'a'}) SET n:Acct`); err != nil {
		t.Fatalf("T1 on node a: %v", err)
	}
	if err := execInTx(t2, `MATCH (n:Plain {k:'b'}) SET n:Acct`); err != nil {
		t.Fatalf("T2 on node b: %v", err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 commit (node a, email present): %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("T2 commit (node b, email present) must not conflict with T1 on node a: %v", err)
	}
	if got := countQ(t, ctx, eng, `MATCH (n:Acct) RETURN count(n) AS c`); got != 2 {
		t.Fatalf("both disjoint transactions should have committed: :Acct count=%d, want 2", got)
	}
}

// TestLabelIndex_NotNullRefusalLeavesNoStaleCandidate is the CONTROL for the
// stale-label-index defect the write-skew fix uncovered. It is NOT the test that
// pins that defect — it passes with and without the fix, and it is here to record
// which shapes are clean, because that boundary was measured and is narrower than
// it first looked.
//
// # The defect, and where it is actually pinned
//
// A label scan returned a node that DOES NOT CARRY THE LABEL:
//
//	MATCH (n {k:'n1'}) RETURN labels(n)   -> ["Plain"]
//	MATCH (n:Acct) RETURN n.k             -> one ROW, k='n1'
//	MATCH (n {k:'n1'}) WHERE n:Acct       -> 0
//
// Not a bad count — an actual row. Graph.setNodeLabelInfo adds to the label bitmap
// IMMEDIATELY, but its undo's removal was DEFERRED, stamped with the aborting
// transaction's record, and then CANCELLED by withdrawAbortedIndexRemovals on the
// reasoning that "a removal the transaction never committed must not fire" — true
// for new work, false for the withdrawal of the transaction's own add.
// deferLabelIndexRemoval now declines to defer while the transaction is unwinding,
// the same exemption writeCtx.undoing already grants the conflict test.
//
// It is pinned by TestConstraint_NotNullWriteSkewAcrossSubstores above, whose
// CONSISTENCY assertion fails against a build carrying the constraint stamp but not
// the exemption. That was verified by reverting the exemption alone.
//
// # What this control records
//
// Only a transaction refused by the SERIALIZATION-CONFLICT backstop leaves the
// residue. Refusal by the commit-time NOT NULL check does not, and that was probed
// rather than assumed: autocommit and explicit transaction, each with and without a
// concurrent open reader, and with a peer committing on the same node and on an
// unrelated one — six shapes, all clean. This test keeps two of them honest, so a
// future change cannot quietly extend the defect to the NOT NULL path.
func TestLabelIndex_NotNullRefusalLeavesNoStaleCandidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder bool
	}{
		{name: "no concurrent reader", holder: false},
		{name: "a concurrent reader holds an older snapshot", holder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, ctx := newLabelConflictEngine(t,
				`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`,
				`CREATE (n:Plain {k:'n1'})`) // NO email: the label add is refused at commit

			if tc.holder {
				h, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatalf("BeginTx (holder): %v", err)
				}
				t.Cleanup(func() { _ = h.Rollback() })
			}

			// The statement succeeds; the commit-time check refuses the transaction,
			// so the eager add is undone inside the barrier.
			if _, err := eng.RunInTx(ctx, addLabel, nil); err == nil {
				t.Fatal("the NOT NULL check should have refused this commit")
			}

			// The label is gone from the store...
			if got := countQ(t, ctx, eng,
				`MATCH (n {k:'n1'}) WHERE n:Acct RETURN count(n) AS c`); got != 0 {
				t.Fatalf("per-row label predicate says the node carries :Acct: count=%d, want 0", got)
			}
			// ...so the label SCAN must not yield it either. Asserted on ROWS and not
			// only on a count: a count can be wrong on its own (rmp #2325/#2326), and a
			// row is the stronger statement — it proves the scan handed a caller a node
			// that does not match the pattern.
			r, err := eng.Run(ctx, `MATCH (n:Acct) RETURN n.k AS k`, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			rows := 0
			for r.Next() {
				rows++
				t.Errorf("label scan yielded k=%v, a node whose labels do not include :Acct", r.Record()["k"])
			}
			if rerr := r.Err(); rerr != nil {
				t.Errorf("rows err: %v", rerr)
			}
			_ = r.Close()
			if got := countQ(t, ctx, eng, `MATCH (n:Acct) RETURN count(n) AS c`); got != 0 {
				t.Errorf("MATCH (n:Acct) count=%d, want 0 (rows seen: %d)", got, rows)
			}
		})
	}
}

// TestConstraintStamp_CostsNothingWithoutAnExistenceConstraint is the NEGATIVE COST
// CONTROL, and it is deliberately structural rather than statistical.
//
// The per-node constraint stamp conflicts at NODE granularity, which is wider than
// the substore granularity everything else here uses, so the claim that it costs an
// unconstrained schema nothing is the claim that has to be defended. A benchmark can
// only bound that cost to within its noise — and on this host, under desktop load,
// that noise reached ±25%. This asserts the underlying fact instead: with no
// existence constraint declared, the stamp store is never written AT ALL.
//
// Both directions, because "always zero" would also satisfy a store that is broken:
// the same workload under a declared constraint must make it non-zero.
func TestConstraintStamp_CostsNothingWithoutAnExistenceConstraint(t *testing.T) {
	const work = 25

	// ARM 1: no existence constraint anywhere in the schema.
	gNone, engNone, ctxNone := newLabelConflictGraph(t)
	for i := 0; i < work; i++ {
		mustRunTx(t, ctxNone, engNone,
			`CREATE (n:Account {id: `+itoa(i)+`, email: 'e'})`)
	}
	for i := 0; i < work; i++ {
		mustRunTx(t, ctxNone, engNone,
			`MATCH (n:Account {id: `+itoa(i)+`}) REMOVE n.email`)
	}
	if got := gNone.MVCCStats().ConstraintStamps; got != 0 {
		t.Errorf("no existence constraint declared, yet %d constraint stamp(s) were written; "+
			"the unconstrained path must not reach the stamp store at all", got)
	}

	// ARM 2: the SAME workload with a constraint declared. The stamp must be used,
	// or arm 1 proves nothing.
	gCon, engCon, ctxCon := newLabelConflictGraph(t,
		`CREATE CONSTRAINT acct_email_nn ON (n:Account) ASSERT n.email IS NOT NULL`)
	for i := 0; i < work; i++ {
		mustRunTx(t, ctxCon, engCon,
			`CREATE (n:Account {id: `+itoa(i)+`, email: 'e'})`)
	}
	if got := gCon.MVCCStats().ConstraintStamps; got == 0 {
		t.Error("a constraint IS declared, yet no constraint stamp was written; " +
			"the zero in arm 1 would then say nothing about the gate")
	}
}

// assertSerializationConflict asserts err is the module's write-write conflict. The
// store attribution is NOT pinned here: which substore reports the collision is an
// implementation choice, unlike rmp #2354 where the label store was the defect's
// location and therefore part of the assertion.
func assertSerializationConflict(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a serialization conflict, got nil", what)
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("%s: expected mvcc.ErrSerializationConflict, got %v", what, err)
	}
}
