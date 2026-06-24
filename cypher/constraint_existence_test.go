package cypher_test

// constraint_existence_test.go — end-to-end tests for commit-time enforcement of
// NOT NULL (property-existence) constraints (#1754, ACID Consistency).
//
// The constraint is a commit-time, how-agnostic invariant: at commit, every node
// carrying the constrained label in its final committed state must have the
// property present and non-null. These tests drive the full Engine pipeline to
// prove every way a node can end up violating the invariant is rejected
// atomically, while a node that legitimately does not carry the label, or has the
// property, is accepted — and that an unrelated unconstrained write is unaffected.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newExistenceEngine builds an in-memory engine with a NOT NULL(Acct.email)
// constraint installed.
func newExistenceEngine(t *testing.T) (*cypher.Engine, context.Context) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	res, err := eng.Run(ctx, `CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`, nil)
	if err != nil {
		t.Fatalf("create constraint error: %v", err)
	}
	_ = res.Close()
	return eng, ctx
}

// expectViolation asserts that running query rejects with ErrConstraintViolation
// and leaves the live node count unchanged at wantNodes (atomicity).
func expectViolation(t *testing.T, eng *cypher.Engine, ctx context.Context, query string, wantNodes int64) { //nolint:revive // t first, ctx follows
	t.Helper()
	before := countNodes(t, ctx, eng)
	if _, err := eng.RunInTx(ctx, query, nil); err == nil {
		t.Fatalf("%s: expected NOT NULL constraint violation, got nil", query)
	} else if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("%s: expected ErrConstraintViolation, got %v", query, err)
	}
	if got := countNodes(t, ctx, eng); got != wantNodes {
		t.Fatalf("%s: atomicity breach — node count %d, want %d (rejected tx left partial state)", query, got, wantNodes)
	}
	_ = before
}

// TestExistence_CreateOmittingProp_Rejected: CREATE adding the constrained label
// without the property is rejected atomically.
func TestExistence_CreateOmittingProp_Rejected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	expectViolation(t, eng, ctx, `CREATE (n:Acct {id:1})`, 0)
}

// TestExistence_CreateWithProp_Accepted: CREATE that includes the property
// succeeds.
func TestExistence_CreateWithProp_Accepted(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct {id:1, email:'a@x'})`, nil); err != nil {
		t.Fatalf("valid CREATE rejected: %v", err)
	}
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("valid CREATE did not persist: node count %d, want 1", got)
	}
}

// TestExistence_SetLabelOnNodeLackingProp_Rejected: SET n:Acct adding the
// constrained label to a node that lacks the property is rejected atomically; the
// node survives (the SET is undone), the label is not attached.
func TestExistence_SetLabelOnNodeLackingProp_Rejected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	// Seed an unrelated node that lacks email.
	if _, err := eng.RunInTx(ctx, `CREATE (n:Person {id:7})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	// Adding :Acct to it must be rejected (node would carry :Acct without email).
	if _, err := eng.RunInTx(ctx, `MATCH (n:Person {id:7}) SET n:Acct`, nil); err == nil {
		t.Fatal("expected violation for SET n:Acct on node lacking email, got nil")
	} else if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation, got %v", err)
	}
	// Atomicity: the seed node survives and did NOT gain the :Acct label.
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("node count %d, want 1 (seed must survive)", got)
	}
	res, err := eng.Run(ctx, `MATCH (n:Acct) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("verify query error: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("verify query produced no rows")
	}
	if c := asInt(t, res.Record()["c"]); c != 0 {
		t.Fatalf(":Acct label leaked onto node after rejected SET; count=%d want 0", c)
	}
}

// TestExistence_SetLabelOnNodeWithProp_Accepted: SET n:Acct on a node that
// already has the property succeeds.
func TestExistence_SetLabelOnNodeWithProp_Accepted(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Person {id:7, email:'p@x'})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	if _, err := eng.RunInTx(ctx, `MATCH (n:Person {id:7}) SET n:Acct`, nil); err != nil {
		t.Fatalf("valid SET n:Acct rejected: %v", err)
	}
}

// TestExistence_RemovePropOnConstrainedNode_Rejected: REMOVE n.email on a node
// carrying :Acct is rejected atomically; the property survives.
func TestExistence_RemovePropOnConstrainedNode_Rejected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct {id:1, email:'a@x'})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	if _, err := eng.RunInTx(ctx, `MATCH (n:Acct {id:1}) REMOVE n.email`, nil); err == nil {
		t.Fatal("expected violation for REMOVE n.email on :Acct node, got nil")
	} else if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation, got %v", err)
	}
	// Atomicity: email must still be present.
	res, err := eng.Run(ctx, `MATCH (n:Acct {id:1}) RETURN n.email AS e`, nil)
	if err != nil {
		t.Fatalf("verify query error: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("node disappeared after rejected REMOVE")
	}
	if res.Record()["e"] == nil {
		t.Fatal("email was removed despite the rejected transaction (atomicity breach)")
	}
}

// TestExistence_SetPropNullOnConstrainedNode_Rejected: SET n.email = null
// (a removal in the Cypher data model) on a :Acct node is rejected. This is the
// existing SET-to-null enforcement, preserved.
func TestExistence_SetPropNullOnConstrainedNode_Rejected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct {id:1, email:'a@x'})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	if _, err := eng.RunInTx(ctx, `MATCH (n:Acct {id:1}) SET n.email = null`, nil); err == nil {
		t.Fatal("expected violation for SET n.email = null on :Acct node, got nil")
	} else if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation, got %v", err)
	}
}

// TestExistence_DeleteConstrainedNode_Accepted: DELETE / DETACH DELETE of a node
// needs no check — the node is absent from the final committed state.
func TestExistence_DeleteConstrainedNode_Accepted(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct {id:1, email:'a@x'})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	if _, err := eng.RunInTx(ctx, `MATCH (n:Acct {id:1}) DELETE n`, nil); err != nil {
		t.Fatalf("DELETE of constrained node wrongly rejected: %v", err)
	}
	if got := countNodes(t, ctx, eng); got != 0 {
		t.Fatalf("node count %d, want 0 after DELETE", got)
	}
}

// TestExistence_CreateThenSetWithinTx_Accepted: a single statement that creates
// the node and sets the property is valid — the invariant is final-state, not
// per-clause. (CREATE (n:Acct) SET n.email = 'x'.)
func TestExistence_CreateThenSetWithinTx_Accepted(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct {id:1}) SET n.email = 'a@x'`, nil); err != nil {
		t.Fatalf("CREATE-then-SET within one statement wrongly rejected: %v", err)
	}
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("valid CREATE-then-SET did not persist: count %d want 1", got)
	}
}

// TestExistence_MultipleConstrainedLabels: a node carrying two constrained labels
// must satisfy BOTH; missing either property rejects.
func TestExistence_MultipleConstrainedLabels(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE CONSTRAINT a_nn ON (n:A) ASSERT n.a IS NOT NULL`,
		`CREATE CONSTRAINT b_nn ON (n:B) ASSERT n.b IS NOT NULL`,
	} {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("create constraint %q error: %v", q, err)
		}
		_ = res.Close()
	}
	// Has :A,:B and a but not b → rejected (b missing).
	expectViolation(t, eng, ctx, `CREATE (n:A:B {a:1})`, 0)
	// Has both a and b → accepted.
	if _, err := eng.RunInTx(ctx, `CREATE (n:A:B {a:1, b:2})`, nil); err != nil {
		t.Fatalf("node satisfying both constraints wrongly rejected: %v", err)
	}
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("count %d want 1", got)
	}
}

// TestExistence_UnrelatedCreate_Unaffected: a CREATE that touches no constrained
// label is unaffected by the existence constraint (and pays nothing for it).
func TestExistence_UnrelatedCreate_Unaffected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	if _, err := eng.RunInTx(ctx, `CREATE (n:Widget {id:1})`, nil); err != nil {
		t.Fatalf("unrelated CREATE rejected: %v", err)
	}
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("unrelated CREATE did not persist: count %d want 1", got)
	}
}

// TestExistence_RemoveLabelExemptsNode: removing the constrained label from a
// node that lacks the property is valid — the node no longer carries the label
// in its final state, so the property may legitimately be absent.
func TestExistence_RemoveLabelExemptsNode(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	// Seed a valid :Acct node BEFORE the constraint exists, carrying email, then
	// add a second label so REMOVE :Acct leaves a live node.
	if _, err := eng.RunInTx(ctx, `CREATE (n:Acct:Tmp {id:1, email:'a@x'})`, nil); err != nil {
		t.Fatalf("seed CREATE error: %v", err)
	}
	res, err := eng.Run(ctx, `CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`, nil)
	if err != nil {
		t.Fatalf("create constraint error: %v", err)
	}
	_ = res.Close()
	// Remove the email AND the :Acct label together: final state carries :Tmp
	// only, so the constraint does not apply. Must be accepted.
	if _, err := eng.RunInTx(ctx, `MATCH (n:Acct {id:1}) REMOVE n.email REMOVE n:Acct`, nil); err != nil {
		t.Fatalf("REMOVE label + prop together wrongly rejected: %v", err)
	}
}

// TestExistence_ExplicitTx_CreateOmittingProp_Rejected: the same enforcement must
// hold on the explicit-transaction (Bolt BEGIN/RUN/COMMIT) path, checked at
// Commit, leaving no partial state.
func TestExistence_ExplicitTx_CreateOmittingProp_Rejected(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx error: %v", err)
	}
	if _, err := tx.Exec(`CREATE (n:Acct {id:1})`, nil); err != nil {
		t.Fatalf("Exec error (statement itself should not fail; check is at Commit): %v", err)
	}
	if cErr := tx.Commit(); cErr == nil {
		t.Fatal("expected Commit to reject the NOT NULL violation, got nil")
	} else if !errors.Is(cErr, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation from Commit, got %v", cErr)
	}
	// Atomicity: nothing committed.
	if got := countNodes(t, ctx, eng); got != 0 {
		t.Fatalf("rejected explicit tx leaked %d node(s); want 0", got)
	}
}

// TestExistence_ExplicitTx_CreateThenSet_Accepted: across two statements of one
// explicit transaction, CREATE then SET the property is valid (final-state check
// at Commit).
func TestExistence_ExplicitTx_CreateThenSet_Accepted(t *testing.T) {
	eng, ctx := newExistenceEngine(t)
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx error: %v", err)
	}
	if _, err := tx.Exec(`CREATE (n:Acct {id:1})`, nil); err != nil {
		t.Fatalf("Exec CREATE error: %v", err)
	}
	if _, err := tx.Exec(`MATCH (n:Acct {id:1}) SET n.email = 'a@x'`, nil); err != nil {
		t.Fatalf("Exec SET error: %v", err)
	}
	if cErr := tx.Commit(); cErr != nil {
		t.Fatalf("valid explicit tx wrongly rejected at Commit: %v", cErr)
	}
	if got := countNodes(t, ctx, eng); got != 1 {
		t.Fatalf("valid explicit tx did not persist: count %d want 1", got)
	}
}

// asInt extracts an int64 from a count() result column value (an
// expr.IntegerValue).
func asInt(t *testing.T, v interface{}) int64 {
	t.Helper()
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("expected IntegerValue, got %T", v)
	}
	return int64(iv)
}
