package cypher_test

// mvcc_explicit_tx_conflict_test.go — rmp #2305 AC3.
//
// Two explicit write transactions can now be open at the same time (rmp #2305
// retired the transaction-lifetime barrier hold), so for the first time two of them
// can collide on the same object. Before, the exclusive hold made that
// unrepresentable: the second transaction could not even begin.
//
// This file establishes WHERE the collision surfaces and asserts it, because the
// answer is part of the engine's contract and a caller writing a retry loop needs
// to know it.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestExplicitTx_ConflictSurfacesAtTheConflictingStatement pins the point at which a
// write-write collision between two open explicit transactions is reported.
//
// # The answer, and why it is the right one
//
// GoGraph reports it AT THE CONFLICTING STATEMENT — the second transaction's Exec
// fails — not at COMMIT. That follows from the mechanism rather than from a
// preference: conflict detection is FIRST-UPDATER-WINS on the version chain, so the
// loser is identified at the moment it tries to install a version over one an
// in-flight transaction already installed. There is nothing to defer to commit,
// because the decision is already made.
//
// The alternative — validating a write set at COMMIT — belongs to optimistic engines
// that BUFFER their writes and therefore cannot know about a collision until they
// try to install them. GoGraph applies eagerly, so it has the information at the
// statement, and reporting it there is strictly more useful: the client learns WHICH
// statement failed rather than only that the transaction did.
//
// Measured, not assumed: the error observed here is
// mvcc.ErrSerializationConflict raised from B's Exec, logged by this test.
//
// # What the caller must do
//
// Roll the transaction back and retry it. The error is retriable and identifies
// itself as such through [mvcc.ErrSerializationConflict]; over Bolt it maps to a
// TransientError so the official driver's managed transactions retry it
// automatically.
func TestExplicitTx_ConflictSurfacesAtTheConflictingStatement(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	// Seed the contested node in its own committed transaction, so both
	// transactions below start from a state where it exists.
	if err := runWrite(t, eng, "CREATE (:Acct {id:'x', bal:0})"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txA, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("A BeginTx: %v", err)
	}
	defer func() { _ = txA.Rollback() }()

	txB, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("B BeginTx: %v", err)
	}
	defer func() { _ = txB.Rollback() }()

	// A updates the contested property first and stays open, so its version sits at
	// the head of the chain, uncommitted.
	resA, err := txA.Exec("MATCH (a:Acct {id:'x'}) SET a.bal = 1", nil)
	if err != nil {
		t.Fatalf("A Exec: %v", err)
	}
	if derr := drainExec(t, resA); derr != nil {
		t.Fatalf("A drain: %v", derr)
	}

	// B updates the SAME property. First-updater-wins makes B the loser.
	resB, bErr := txB.Exec("MATCH (a:Acct {id:'x'}) SET a.bal = 2", nil)
	if bErr == nil && resB != nil {
		bErr = drainExec(t, resB)
	}

	if bErr == nil {
		t.Fatalf("B's conflicting statement succeeded while A held an uncommitted " +
			"version of the same property.\nFirst-updater-wins must refuse the second " +
			"writer: two transactions silently overwriting one another is a lost update, " +
			"which snapshot isolation forbids.")
	}
	if !errors.Is(bErr, mvcc.ErrSerializationConflict) {
		t.Fatalf("B's conflicting statement failed with %v, which is not "+
			"errors.Is(mvcc.ErrSerializationConflict).\nA caller cannot tell a retriable "+
			"conflict from a permanent failure, so a managed transaction would either "+
			"give up on a retriable error or retry a hopeless one.", bErr)
	}
	t.Logf("the conflict surfaced at B's statement: %v", bErr)

	// A is unaffected by B's refusal and commits normally: the loser's failure must
	// not doom the winner.
	if cerr := txA.Commit(); cerr != nil {
		t.Fatalf("A Commit after B was refused: %v.\nThe winner of a conflict must be "+
			"able to commit; only the loser is refused.", cerr)
	}
	if got := countLabelledNodes(t, eng, "Acct"); got != 1 {
		t.Fatalf("after the conflict the graph holds %d :Acct nodes, want 1", got)
	}
}

// TestExplicitTx_DisjointTransactionsBothCommit is the companion that stops the
// conflict predicate from being satisfied by refusing everything: two open
// transactions writing DIFFERENT objects must BOTH commit.
//
// Without this, an implementation that reported a conflict for every concurrent
// pair would pass the test above.
func TestExplicitTx_DisjointTransactionsBothCommit(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	txA, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("A BeginTx: %v", err)
	}
	txB, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("B BeginTx: %v", err)
	}

	// Interleaved statements on disjoint keys, so the two transactions are genuinely
	// concurrent rather than merely overlapping in wall-clock time.
	for _, step := range []struct {
		tx    *cypher.ExplicitTx
		name  string
		query string
	}{
		{txA, "A", "CREATE (:Acct {id:'a'})"},
		{txB, "B", "CREATE (:Acct {id:'b'})"},
		{txA, "A", "MATCH (n:Acct {id:'a'}) SET n.bal = 1"},
		{txB, "B", "MATCH (n:Acct {id:'b'}) SET n.bal = 2"},
	} {
		res, execErr := step.tx.Exec(step.query, nil)
		if execErr != nil {
			t.Fatalf("%s Exec(%q): %v", step.name, step.query, execErr)
		}
		if derr := drainExec(t, res); derr != nil {
			t.Fatalf("%s drain(%q): %v", step.name, step.query, derr)
		}
	}

	if err := txA.Commit(); err != nil {
		t.Fatalf("A Commit: %v.\nTwo transactions on disjoint objects must both commit; "+
			"refusing one would make the conflict predicate useless.", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("B Commit: %v", err)
	}
	if got := countLabelledNodes(t, eng, "Acct"); got != 2 {
		t.Fatalf("after two disjoint committed transactions the graph holds %d :Acct "+
			"nodes, want 2", got)
	}
}

// TestExplicitTx_PhysicalUndoIsSoundUnderConcurrency verifies the reasoning that
// lets Rollback keep the PHYSICAL undo log now that two transactions can be open at
// once — rather than assuming it, which is what rmp #2305 asked for.
//
// # The reasoning, and why it needs checking
//
// Rollback restores the stored value IN PLACE from an inverse recorded at write
// time. That is only sound if no other transaction can have built on the value being
// withdrawn. Under a transaction-lifetime exclusive lock that was trivially true —
// there was no other writer. It is no longer trivial, so the argument has to stand on
// conflict detection instead:
//
//   - another transaction cannot have WRITTEN the same object, because
//     first-updater-wins refuses it while the first transaction's version is
//     uncommitted (TestExplicitTx_ConflictSurfacesAtTheConflictingStatement);
//   - another transaction cannot have READ the withdrawn value, because it reads
//     through its OWN snapshot, taken at its start, which never includes another
//     transaction's uncommitted version.
//
// So the physical restore touches a value only its own transaction could observe.
// This test exercises exactly that: B reads the contested property while A holds an
// uncommitted write to it, A rolls back, and then B writes it successfully — which
// would be impossible if A's rollback had left the object in a state B's conflict
// check misread, and which proves A's refusal of B was not permanent.
func TestExplicitTx_PhysicalUndoIsSoundUnderConcurrency(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	if err := runWrite(t, eng, "CREATE (:Acct {id:'y', bal:7})"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txA, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("A BeginTx: %v", err)
	}
	resA, err := txA.Exec("MATCH (a:Acct {id:'y'}) SET a.bal = 99", nil)
	if err != nil {
		t.Fatalf("A Exec: %v", err)
	}
	if derr := drainExec(t, resA); derr != nil {
		t.Fatalf("A drain: %v", derr)
	}

	// B READS the contested property while A's write is uncommitted. It must observe
	// the seeded value, never A's 99: B's snapshot excludes A's unpublished version.
	if got := balanceOf(t, eng, "y"); got != 7 {
		t.Fatalf("a concurrent read observed bal=%d while A's write was uncommitted, "+
			"want the seeded 7. An uncommitted version must not be readable, or the "+
			"physical undo below would be withdrawing a value another transaction acted "+
			"on.", got)
	}

	// A rolls back: the physical undo restores the stored value in place.
	if rerr := txA.Rollback(); rerr != nil {
		t.Fatalf("A Rollback: %v", rerr)
	}
	if got := balanceOf(t, eng, "y"); got != 7 {
		t.Fatalf("after A rolled back, bal=%d, want the seeded 7 — the rollback left a "+
			"trace", got)
	}

	// B can now write it. This is the half that proves A's rollback genuinely cleared
	// the head of the version chain rather than leaving a version that keeps refusing
	// every later writer — the liveness bug the abort path had to be fixed for once
	// before (rmp #2320's aborted-head exemption).
	txB, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("B BeginTx: %v", err)
	}
	resB, err := txB.Exec("MATCH (a:Acct {id:'y'}) SET a.bal = 42", nil)
	if err != nil {
		t.Fatalf("B Exec after A rolled back: %v.\nA rolled-back transaction must not "+
			"leave the object permanently unwritable.", err)
	}
	if derr := drainExec(t, resB); derr != nil {
		t.Fatalf("B drain: %v", derr)
	}
	if cerr := txB.Commit(); cerr != nil {
		t.Fatalf("B Commit: %v", cerr)
	}
	if got := balanceOf(t, eng, "y"); got != 42 {
		t.Fatalf("after B committed, bal=%d, want 42", got)
	}
}

// balanceOf reads the bal property of the :Acct node with the given id through the
// engine's ordinary concurrent read path.
func balanceOf(t *testing.T, eng *cypher.Engine, id string) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(),
		"MATCH (a:Acct) WHERE a.id = '"+id+"' RETURN a.bal AS b", nil)
	if err != nil {
		t.Fatalf("read bal: %v", err)
	}
	var got int64
	for res.Next() {
		if v, ok := res.Record()["b"]; ok {
			if n, isInt := v.(expr.IntegerValue); isInt {
				got = int64(n)
			}
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("read bal drain: %v", err)
	}
	_ = res.Close()
	return got
}
