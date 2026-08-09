package cypher_test

// constraint_uncommitted_read_test.go — regression test for rmp #2350, an ACID
// CONSISTENCY defect: the commit-time NOT NULL check read the PRESENT, so it could
// decide a constraint on another transaction's UNCOMMITTED state.
//
// # The defect
//
// cypher/constraint_check.go asserted that its commit-time scan "runs under the
// barrier, so it observes a quiescent graph (no concurrent writer, no in-flight
// View)". Both halves became false — the barrier has been SHARED since rmp #2320, and
// rmp #2344 removed Graph.View — and the code rested on it:
// checkNotNullConstraints read through the RAW graph's present-time, unversioned
// accessors rather than through the committing transaction's own view. GoGraph
// updates a stored value IN PLACE and keeps the inverse in the version chain, so an
// accessor that resolves no version reads the NEWEST value — another transaction's
// uncommitted work included.
//
// # Why write-write conflict detection does not save it
//
// Conflicts are detected PER SUBSTORE against that object's version-chain head:
// labels in graph/lpg/lpg.go, properties in graph/lpg/property.go, existence in
// mvcc_life.go. Two transactions touching the SAME node through DIFFERENT substores
// therefore never conflict — and that is exactly the pair this test builds: one adds
// the constrained LABEL, the other writes the constrained PROPERTY.
//
// # The direction that matters
//
// FALSE ACCEPT is the unrecoverable one and is what this pins: the committing
// transaction sees a property value that only an uncommitted transaction has written,
// accepts, and commits a node carrying the constrained label with no property. When
// the other transaction then rolls back, the durable state violates the constraint —
// a Consistency violation the engine reported as success.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countQ runs a counting query and returns its single integer column.
//
// Assertions here go through CYPHER, not through the raw graph by key: a node
// created by CREATE (n:Acct {k:'n1'}) is NOT interned under the key "n1" — `k` is a
// PROPERTY. The first version of this test asserted g.HasNodeLabel("n1", "Acct") and
// duly reported "the accepted transaction did not commit its label" against a
// perfectly good engine. The lookup was wrong, not the engine.
func countQ(t *testing.T, ctx context.Context, eng *cypher.Engine, q string) int64 { //nolint:revive // t first by convention, ctx follows
	t.Helper()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("%s: no rows", q)
	}
	v, ok := res.Record()["c"]
	if !ok {
		t.Fatalf("%s: missing column c", q)
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("%s: column c is %T, want integer", q, v)
	}
	return int64(iv)
}

// TestConstraint_NotNullCheck_IgnoresUncommittedWrites builds the conflict-free
// same-node / different-substore interleaving and asserts the committing
// transaction is judged against COMMITTED state only.
//
// Sequence, with T2 deliberately parked mid-transaction:
//
//	seed : CREATE (n:Plain {k:'n1'})            — no :Acct label, no email
//	T2   : BEGIN; SET n.email = 'x'             — eager, UNCOMMITTED, never committed
//	T1   : autocommit  MATCH (n) SET n:Acct     — its NOT NULL check runs here
//	T2   : ROLLBACK                             — 'x' never existed in any committed state
//
// T1 must be REJECTED: in committed state the node has no email, so labelling it
// :Acct violates NOT NULL(Acct.email). Before the fix T1 saw T2's uncommitted 'x',
// accepted, and left a durable violation behind after T2's rollback.
func TestConstraint_NotNullCheck_IgnoresUncommittedWrites(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	mustRun := func(q string) {
		t.Helper()
		res, err := eng.RunInTx(ctx, q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if err := res.Err(); err != nil {
			_ = res.Close()
			t.Fatalf("%s: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("%s: close: %v", q, err)
		}
	}

	mustRun(`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`)
	mustRun(`CREATE (n:Plain {k:'n1'})`)

	// T2 writes the constrained property and PARKS, uncommitted.
	t2, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx (T2): %v", err)
	}
	r2, err := t2.Exec(`MATCH (n:Plain {k:'n1'}) SET n.email = 'uncommitted@example.com'`, nil)
	if err != nil {
		_ = t2.Rollback()
		t.Fatalf("T2 SET: %v", err)
	}
	if err := r2.Err(); err != nil {
		_ = r2.Close()
		_ = t2.Rollback()
		t.Fatalf("T2 SET: %v", err)
	}
	_ = r2.Close()
	// From here until the rollback below, 'uncommitted@example.com' exists ONLY in
	// T2's unpublished work. No committed state has ever carried it.

	// T1 adds the constrained LABEL — a different substore from T2's property write,
	// so no write-write conflict fires and both transactions are legal.
	_, t1Err := eng.RunInTx(ctx, `MATCH (n:Plain {k:'n1'}) SET n:Acct`, nil)

	if rbErr := t2.Rollback(); rbErr != nil {
		t.Fatalf("T2 Rollback: %v", rbErr)
	}

	// Committed state after both transactions have finished.
	acct := countQ(t, ctx, eng, `MATCH (n:Acct) RETURN count(n) AS c`)
	withEmail := countQ(t, ctx, eng, `MATCH (n) WHERE n.email IS NOT NULL RETURN count(n) AS c`)

	if withEmail != 0 {
		t.Fatalf("T2's rolled-back property survived on %d node(s); the fixture is not "+
			"exercising uncommitted state", withEmail)
	}
	if t1Err == nil {
		t.Fatalf("ACID CONSISTENCY VIOLATED: labelling the node :Acct was ACCEPTED because the "+
			"commit-time NOT NULL check read the PRESENT and saw a value only T2 had written, "+
			"uncommitted. After T2's rollback %d node(s) carry :Acct and %d carry an email — a "+
			"node with the constrained label and no email, committed as a success. The check "+
			"must read through the committing transaction's own view",
			acct, withEmail)
	}
	// Rejected, which is correct. Atomicity: the label must not have stuck either.
	if acct != 0 {
		t.Fatalf("the rejected transaction left the :Acct label on %d node(s) — a commit-time "+
			"rejection must roll its eager mutations back", acct)
	}
}

// TestConstraint_NotNullCheck_SeesItsOwnWrites is the positive control, and without
// it the test above would pass against a check that rejected everything.
//
// The committing transaction sets the property AND the label in one statement. Its
// own eager write is not committed either at the moment the check runs, so a fix that
// simply ignored unpublished versions would break this — the check must resolve
// through the transaction's OWN id, seeing its own writes and nobody else's.
func TestConstraint_NotNullCheck_SeesItsOwnWrites(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`, nil)
	if err != nil {
		t.Fatalf("create constraint: %v", err)
	}
	_ = res.Close()

	// One transaction, both writes: the label and the property it requires.
	r, err := eng.RunInTx(ctx, `CREATE (n:Acct {k:'own', email:'self@example.com'})`, nil)
	if err != nil {
		t.Fatalf("a transaction that supplies the constrained property in the SAME statement "+
			"was rejected: %v. The check must see the committing transaction's OWN "+
			"uncommitted writes — resolving through its transaction id — while excluding "+
			"every other transaction's", err)
	}
	if err := r.Err(); err != nil {
		_ = r.Close()
		t.Fatalf("result error: %v", err)
	}
	_ = r.Close()

	if n := countQ(t, ctx, eng, `MATCH (n:Acct) RETURN count(n) AS c`); n != 1 {
		t.Fatalf("%d node(s) carry :Acct after an accepted transaction, want 1", n)
	}
	if n := countQ(t, ctx, eng, `MATCH (n:Acct {email:'self@example.com'}) RETURN count(n) AS c`); n != 1 {
		t.Fatalf("%d node(s) carry :Acct with the committed email, want 1", n)
	}
}
