package cypher_test

// constraint_set_label_test.go — regression tests for rmp #2352, an ACID
// CONSISTENCY defect: `SET n:Label` bypassed UNIQUE constraint enforcement
// entirely, and `REMOVE n:Label` never released what the label had reserved.
//
// # The defect
//
// A UNIQUE constraint binds a (label, property) PAIR, so a node joins it by
// acquiring EITHER half. Every reservation call site was a property-set path, and
// none of them is reached by a label write, so the value-set never learned the
// node had joined the constrained label:
//
//	CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE
//	CREATE (a:Person {k:'a', email:'dup@example.com'})
//	CREATE (b:Plain  {k:'b', email:'dup@example.com'})   -- not a :Person yet
//	MATCH (b:Plain {k:'b'}) SET b:Person                 -- returned nil, COMMITTED
//
// leaving TWO :Person nodes on one supposedly unique email. No concurrency is
// involved: it is a plain single-threaded Consistency violation.
//
// # What these tests pin, and why each one is here
//
// The negative alone would be satisfied by a fix that rejected every label write,
// and the release direction is the half that is easy to forget, so the battery is
// deliberately two-sided at every point:
//
//   - the duplicate is REFUSED, and the label does not stick (atomicity);
//   - a UNIQUE value is still ACCEPTED (the control that gives the refusal
//     meaning);
//   - REMOVE gives the value back, so a later legitimate write is not refused by
//     a phantom (#1342's failure mode);
//   - a REMOVE of a label the node never carried gives NOTHING back — releasing
//     on its behalf would hand away another node's live reservation;
//   - re-adding a label the node already carries is not rejected as a duplicate
//     of ITSELF;
//   - a rolled-back transaction releases exactly what it reserved;
//   - a node with no value for the constrained property is outside the
//     constraint, so any number of them may carry the label.
//
// # Where the null rule comes from
//
// A UNIQUE constraint does not constrain null. That is not read off this
// implementation: it is the rule Neo4j states for property-uniqueness constraints
// and the one this engine already applies on every other path —
// [exec.propertyValueToString] reports no value-set key for the zero
// PropertyValue, and ConstraintRegistry.SeedUniqueValues skips nulls when a
// constraint is created over existing data. Requiring the property is the
// separate job of a NOT NULL constraint (cypher/constraint_check.go). The
// openCypher TCK does not cover constraints at all, so nothing here can be — or
// is — claimed as TCK coverage.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newLabelConstraintEngine builds an in-memory engine and runs the supplied
// setup statements, each in its own transaction.
func newLabelConstraintEngine(t *testing.T, setup ...string) (*cypher.Engine, context.Context) {
	t.Helper()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	ctx := context.Background()
	for _, q := range setup {
		mustRunTx(t, ctx, eng, q)
	}
	return eng, ctx
}

// runLabelConstraintTx runs q in its own transaction, DRAINS the result, and
// returns the first error from either channel.
//
// Draining is not optional and the reason is worth stating: a write-operator
// error — every UNIQUE violation, on the property path as much as on the label
// path — is reported on [cypher.Result.Err] and NOT by RunInTx, which hands back
// a nil error. Asserting only on RunInTx's return therefore reads every
// violation as success. The first draft of these tests did exactly that and
// scored the working fix as a failure.
func runLabelConstraintTx(ctx context.Context, eng *cypher.Engine, q string) error {
	res, err := eng.RunInTx(ctx, q, nil)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // draining is the point
	}
	if rerr := res.Err(); rerr != nil {
		_ = res.Close()
		return rerr
	}
	return res.Close()
}

// mustRunTx runs q in its own transaction and fails the test if it is rejected.
func mustRunTx(t *testing.T, ctx context.Context, eng *cypher.Engine, q string) { //nolint:revive // t first, ctx follows
	t.Helper()
	if err := runLabelConstraintTx(ctx, eng, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// expectUniqueViolation asserts q is rejected with a typed UNIQUE
// *exec.ConstraintViolationError wrapping [exec.ErrConstraintViolation].
func expectUniqueViolation(t *testing.T, ctx context.Context, eng *cypher.Engine, q string) { //nolint:revive // t first, ctx follows
	t.Helper()
	err := runLabelConstraintTx(ctx, eng, q)
	if err == nil {
		t.Fatalf("%s: expected a UNIQUE constraint violation, got nil", q)
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("%s: expected ErrConstraintViolation, got %v", q, err)
	}
	var cve *exec.ConstraintViolationError
	if !errors.As(err, &cve) {
		t.Fatalf("%s: expected a typed *exec.ConstraintViolationError, got %T: %v", q, err, err)
	}
	if cve.Kind != "UNIQUE" {
		t.Fatalf("%s: violation Kind = %q, want %q (err=%v)", q, cve.Kind, "UNIQUE", err)
	}
}

// TestSetLabel_DuplicateUnderUniqueConstraint_Refused is the defect itself:
// giving a node a constrained label whose property duplicates an existing
// member's value must be refused, and the label must not stick.
func TestSetLabel_DuplicateUnderUniqueConstraint_Refused(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'dup@example.com'})`,
		// b carries the SAME email but is not a :Person, so nothing binds it yet.
		`CREATE (b:Plain {k:'b', email:'dup@example.com'})`,
	)

	expectUniqueViolation(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)

	// Consistency: exactly one :Person may hold that email.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'dup@example.com' RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Person nodes holding 'dup@example.com' = %d, want 1", got)
	}
	// Atomicity: the rejected statement's eager label write was rolled back, so b
	// is not a :Person at all.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.k = 'b' RETURN count(n) AS c`); got != 0 {
		t.Fatalf("the label STUCK on the rejected node: b is :Person (count=%d, want 0)", got)
	}
	// The node itself must survive intact — the rollback undoes the label, not the node.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Plain) WHERE n.k = 'b' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("rollback damaged the node: :Plain k='b' count=%d, want 1", got)
	}
}

// TestSetLabel_UniqueValue_Accepted is the positive control. Without it, a fix
// that refused EVERY label write would satisfy the test above.
func TestSetLabel_UniqueValue_Accepted(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'taken@example.com'})`,
		`CREATE (b:Plain {k:'b', email:'free@example.com'})`,
	)

	mustRunTx(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)

	if got := countQ(t, ctx, eng, `MATCH (n:Person) RETURN count(n) AS c`); got != 2 {
		t.Fatalf(":Person count = %d, want 2 (the unique-valued label add was refused)", got)
	}

	// The accepted add must have RESERVED the value, not merely passed the check:
	// a third node claiming it is now a duplicate and must be refused.
	mustRunTx(t, ctx, eng, `CREATE (c:Plain {k:'c', email:'free@example.com'})`)
	expectUniqueViolation(t, ctx, eng, `MATCH (c:Plain {k:'c'}) SET c:Person`)
}

// TestRemoveLabel_ReleasesUniqueReservation: taking a node out of the constraint
// frees its value, so a later legitimate claim is not refused by a phantom.
func TestRemoveLabel_ReleasesUniqueReservation(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'shared@example.com'})`,
		`CREATE (b:Plain {k:'b', email:'shared@example.com'})`,
	)

	// While a is a :Person the value is taken.
	expectUniqueViolation(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)

	// a leaves the constraint; the value must come back.
	mustRunTx(t, ctx, eng, `MATCH (a {k:'a'}) REMOVE a:Person`)
	mustRunTx(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)

	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'shared@example.com' RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Person nodes holding the value = %d, want 1 (b only)", got)
	}
}

// TestRemoveLabel_NonMember_ReleasesNothing pins the guard that keeps the release
// direction from destroying the constraint. The value-set is keyed by (label,
// value) and not by node, so releasing on behalf of a node that never carried the
// label would hand away the reservation of whichever node genuinely holds it.
func TestRemoveLabel_NonMember_ReleasesNothing(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'held@example.com'})`,
		// b holds the same value but is NOT a :Person, so it reserved nothing.
		`CREATE (b:Plain {k:'b', email:'held@example.com'})`,
	)

	// A no-op REMOVE: b never carried :Person.
	mustRunTx(t, ctx, eng, `MATCH (b:Plain {k:'b'}) REMOVE b:Person`)

	// a's reservation must be untouched, so b still cannot take the value.
	expectUniqueViolation(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'held@example.com' RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Person nodes holding the value = %d, want 1", got)
	}
}

// TestSetLabel_AlreadyCarried_NotSelfDuplicate: re-adding a label the node
// already has must not reject the node as a duplicate of itself. Its value is
// already in the value-set — reserved when the property was written — so a fix
// that reserved unconditionally would refuse this.
func TestSetLabel_AlreadyCarried_NotSelfDuplicate(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'self@example.com'})`,
	)

	mustRunTx(t, ctx, eng, `MATCH (a:Person {k:'a'}) SET a:Person`)
	// Repeating the same label WITHIN one statement is the same hazard: the first
	// write must be visible to the second guard.
	mustRunTx(t, ctx, eng, `MATCH (a:Person {k:'a'}) SET a:Person:Person`)

	if got := countQ(t, ctx, eng, `MATCH (n:Person) RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Person count = %d, want 1", got)
	}
	// The idempotent re-add must not have released anything either: the value is
	// still taken.
	mustRunTx(t, ctx, eng, `CREATE (b:Plain {k:'b', email:'self@example.com'})`)
	expectUniqueViolation(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)
}

// TestSetLabel_RolledBackTransaction_ReleasesReservation: a transaction that
// reserved a value by adding a label and then rolled back must give it back, or
// the value is reserved for ever by a transaction that never happened.
func TestSetLabel_RolledBackTransaction_ReleasesReservation(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (b:Plain {k:'b', email:'rollback@example.com'})`,
		`CREATE (c:Plain {k:'c', email:'rollback@example.com'})`,
	)

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	r, err := tx.Exec(`MATCH (b:Plain {k:'b'}) SET b:Person`, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("SET b:Person: %v", err)
	}
	for r.Next() { //nolint:revive // drained so the write actually runs; see runLabelConstraintTx
	}
	if err := r.Err(); err != nil {
		_ = r.Close()
		_ = tx.Rollback()
		t.Fatalf("SET b:Person: %v", err)
	}
	if err := r.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SET b:Person: close: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// b never became a :Person, so the value is free and c may take it.
	if got := countQ(t, ctx, eng, `MATCH (n:Person) RETURN count(n) AS c`); got != 0 {
		t.Fatalf("after rollback :Person count = %d, want 0", got)
	}
	mustRunTx(t, ctx, eng, `MATCH (c:Plain {k:'c'}) SET c:Person`)
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.k = 'c' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("c did not become a :Person after the rollback freed the value (count=%d)", got)
	}
}

// TestSetLabel_NullProperty_Unconstrained: a node with NO value for the
// constrained property is outside the constraint, so any number of such nodes may
// carry the label together. See the file doc for the source of this rule.
func TestSetLabel_NullProperty_Unconstrained(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Plain {k:'a'})`,
		`CREATE (b:Plain {k:'b'})`,
	)

	mustRunTx(t, ctx, eng, `MATCH (a:Plain {k:'a'}) SET a:Person`)
	mustRunTx(t, ctx, eng, `MATCH (b:Plain {k:'b'}) SET b:Person`)

	if got := countQ(t, ctx, eng, `MATCH (n:Person) RETURN count(n) AS c`); got != 2 {
		t.Fatalf(":Person count = %d, want 2 (a property-less node is not constrained)", got)
	}
	// Nothing was reserved on their behalf, so a real value is still free, and the
	// label add did not silently reserve a "null" slot.
	mustRunTx(t, ctx, eng, `MATCH (a:Person {k:'a'}) SET a.email = 'later@example.com'`)
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'later@example.com' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("count after giving a property-less member a value = %d, want 1", got)
	}
}

// TestSetLabel_MultipleConstrainedLabels covers a single statement adding several
// labels of which more than one is constrained: every one of them must be
// enforced, and the second must be able to reject after the first has passed.
func TestSetLabel_MultipleConstrainedLabels(t *testing.T) {
	setup := []string{
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE CONSTRAINT staff_badge_uq ON (n:Staff) ASSERT n.badge IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'taken@example.com'})`,
		`CREATE (s:Staff {k:'s', badge:'B-1'})`,
	}

	t.Run("second label violates", func(t *testing.T) {
		// n's email is free but its badge duplicates s's: the statement must be
		// refused, and NEITHER label may stick.
		eng, ctx := newLabelConstraintEngine(t, append(append([]string{}, setup...),
			`CREATE (n:Plain {k:'n', email:'free@example.com', badge:'B-1'})`)...)

		expectUniqueViolation(t, ctx, eng, `MATCH (n:Plain {k:'n'}) SET n:Person:Staff`)

		if got := countQ(t, ctx, eng, `MATCH (n:Person) WHERE n.k = 'n' RETURN count(n) AS c`); got != 0 {
			t.Fatalf("the FIRST label stuck despite the second violating: :Person count=%d, want 0", got)
		}
		if got := countQ(t, ctx, eng, `MATCH (n:Staff) WHERE n.k = 'n' RETURN count(n) AS c`); got != 0 {
			t.Fatalf("the violating label stuck: :Staff count=%d, want 0", got)
		}
		// The rejected statement must not have leaked a reservation of the email it
		// checked first: another node may still legitimately take it.
		mustRunTx(t, ctx, eng, `CREATE (m:Plain {k:'m', email:'free@example.com'})`)
		mustRunTx(t, ctx, eng, `MATCH (m:Plain {k:'m'}) SET m:Person`)
	})

	t.Run("first label violates", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, append(append([]string{}, setup...),
			`CREATE (n:Plain {k:'n', email:'taken@example.com', badge:'B-9'})`)...)

		expectUniqueViolation(t, ctx, eng, `MATCH (n:Plain {k:'n'}) SET n:Person:Staff`)

		if got := countQ(t, ctx, eng, `MATCH (n:Staff) WHERE n.k = 'n' RETURN count(n) AS c`); got != 0 {
			t.Fatalf("the second label stuck although the first violated: :Staff count=%d, want 0", got)
		}
		// B-9 was never reserved by the rejected statement.
		mustRunTx(t, ctx, eng, `CREATE (m:Plain {k:'m', badge:'B-9'})`)
		mustRunTx(t, ctx, eng, `MATCH (m:Plain {k:'m'}) SET m:Staff`)
	})

	t.Run("both unique: accepted and both reserved", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, append(append([]string{}, setup...),
			`CREATE (n:Plain {k:'n', email:'free@example.com', badge:'B-9'})`)...)

		mustRunTx(t, ctx, eng, `MATCH (n:Plain {k:'n'}) SET n:Person:Staff`)

		if got := countQ(t, ctx, eng, `MATCH (n:Person) WHERE n.k = 'n' RETURN count(n) AS c`); got != 1 {
			t.Fatalf("n is not a :Person after an accepted add (count=%d)", got)
		}
		if got := countQ(t, ctx, eng, `MATCH (n:Staff) WHERE n.k = 'n' RETURN count(n) AS c`); got != 1 {
			t.Fatalf("n is not :Staff after an accepted add (count=%d)", got)
		}
		// BOTH values must now be reserved, not just the one that happened to be
		// checked last.
		mustRunTx(t, ctx, eng, `CREATE (p:Plain {k:'p', email:'free@example.com'})`)
		expectUniqueViolation(t, ctx, eng, `MATCH (p:Plain {k:'p'}) SET p:Person`)
		mustRunTx(t, ctx, eng, `CREATE (q:Plain {k:'q', badge:'B-9'})`)
		expectUniqueViolation(t, ctx, eng, `MATCH (q:Plain {k:'q'}) SET q:Staff`)
	})
}

// TestSetLabel_SameTransactionCreateThenLabel: a node created and given the
// constrained label in ONE transaction is judged against the transaction's own
// uncommitted property write, which only a read through that transaction's view
// can see.
func TestSetLabel_SameTransactionCreateThenLabel(t *testing.T) {
	eng, ctx := newLabelConstraintEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'intx@example.com'})`,
	)

	expectUniqueViolation(t, ctx, eng,
		`CREATE (b:Plain {k:'b', email:'intx@example.com'}) WITH b MATCH (n:Plain {k:'b'}) SET n:Person`)

	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'intx@example.com' RETURN count(n) AS c`); got != 1 {
		t.Fatalf(":Person nodes holding the value = %d, want 1", got)
	}
	if got := countQ(t, ctx, eng, `MATCH (n) WHERE n.k = 'b' RETURN count(n) AS c`); got != 0 {
		t.Fatalf("atomicity breach: the rejected transaction left node b behind (count=%d)", got)
	}
}

// TestMergeAction_SetLabel_EnforcesUnique covers MERGE's label-set action, which
// reaches the graph through a DIFFERENT operator than `SET n:Label`.
//
// # Why these exist as separate tests
//
// Enforcing on the SetLabels operator alone left the identical duplicate
// committing through MERGE. A UNIQUE constraint is a property of the committed
// state, so it has to hold no matter which syntax reached that state, and GoGraph
// has THREE distinct label-write sites: the SetLabels operator
// (cypher/exec/set.go), the single-node MERGE action (cypher/exec/merge.go), and
// the pattern MERGE action (cypher/exec/merge_pattern.go). Each was verified to
// admit the duplicate before the fix and to refuse it after; a test per site is
// the only thing that keeps a future change from regressing one of them in
// isolation.
//
// Each subtest carries its own positive control, because "refuse everything"
// would otherwise pass.
func TestMergeAction_SetLabel_EnforcesUnique(t *testing.T) {
	const constraint = `CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`

	t.Run("ON MATCH violates", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, constraint,
			`CREATE (a:Person {k:'a', email:'dup@example.com'})`,
			`CREATE (b:Plain {k:'b', email:'dup@example.com'})`)

		expectUniqueViolation(t, ctx, eng, `MERGE (b:Plain {k:'b'}) ON MATCH SET b:Person`)

		if got := countQ(t, ctx, eng,
			`MATCH (n:Person) WHERE n.email = 'dup@example.com' RETURN count(n) AS c`); got != 1 {
			t.Fatalf(":Person nodes sharing the unique email = %d, want 1", got)
		}
	})

	t.Run("ON MATCH accepted", func(t *testing.T) {
		// The positive control: a free value must still pass through MERGE.
		eng, ctx := newLabelConstraintEngine(t, constraint,
			`CREATE (a:Person {k:'a', email:'taken@example.com'})`,
			`CREATE (b:Plain {k:'b', email:'free@example.com'})`)

		mustRunTx(t, ctx, eng, `MERGE (b:Plain {k:'b'}) ON MATCH SET b:Person`)

		if got := countQ(t, ctx, eng, `MATCH (n:Person) WHERE n.k = 'b' RETURN count(n) AS c`); got != 1 {
			t.Fatalf("b did not become a :Person through MERGE (count=%d, want 1)", got)
		}
		// The label write must also have RESERVED the value, or the enforcement is
		// only half present: a second node must now be refused it.
		mustRunTx(t, ctx, eng, `CREATE (c:Plain {k:'c', email:'free@example.com'})`)
		expectUniqueViolation(t, ctx, eng, `MATCH (c:Plain {k:'c'}) SET c:Person`)
	})

	t.Run("ON CREATE violates", func(t *testing.T) {
		// MERGE creates the node (with the duplicate email) and the action then
		// brings it under the constraint.
		eng, ctx := newLabelConstraintEngine(t, constraint,
			`CREATE (a:Person {k:'a', email:'dup@example.com'})`)

		expectUniqueViolation(t, ctx, eng,
			`MERGE (b:Plain {k:'b', email:'dup@example.com'}) ON CREATE SET b:Person`)

		if got := countQ(t, ctx, eng,
			`MATCH (n:Person) WHERE n.email = 'dup@example.com' RETURN count(n) AS c`); got != 1 {
			t.Fatalf(":Person nodes sharing the unique email = %d, want 1", got)
		}
	})

	t.Run("ON CREATE accepted", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, constraint,
			`CREATE (a:Person {k:'a', email:'taken@example.com'})`)

		mustRunTx(t, ctx, eng,
			`MERGE (b:Plain {k:'b', email:'free@example.com'}) ON CREATE SET b:Person`)

		if got := countQ(t, ctx, eng, `MATCH (n:Person) WHERE n.k = 'b' RETURN count(n) AS c`); got != 1 {
			t.Fatalf("b did not become a :Person through MERGE ON CREATE (count=%d, want 1)", got)
		}
	})
}

// TestMergePatternAction_SetLabel_EnforcesUnique covers the multi-element pattern
// MERGE, whose action path (cypher/exec/merge_pattern.go) is a third label-write
// site distinct from both the SetLabels operator and the single-node MERGE.
func TestMergePatternAction_SetLabel_EnforcesUnique(t *testing.T) {
	setup := []string{
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'dup@example.com'})`,
		`CREATE (c:Plain {k:'c'})`,
	}

	t.Run("violates", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, append(append([]string{}, setup...),
			`CREATE (b:Plain {k:'b', email:'dup@example.com'})`,
			`MATCH (b:Plain {k:'b'}), (c:Plain {k:'c'}) CREATE (b)-[:R]->(c)`)...)

		expectUniqueViolation(t, ctx, eng,
			`MERGE (b:Plain {k:'b'})-[:R]->(c:Plain {k:'c'}) ON MATCH SET b:Person`)

		if got := countQ(t, ctx, eng,
			`MATCH (n:Person) WHERE n.email = 'dup@example.com' RETURN count(n) AS c`); got != 1 {
			t.Fatalf(":Person nodes sharing the unique email = %d, want 1", got)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		eng, ctx := newLabelConstraintEngine(t, append(append([]string{}, setup...),
			`CREATE (b:Plain {k:'b', email:'free@example.com'})`,
			`MATCH (b:Plain {k:'b'}), (c:Plain {k:'c'}) CREATE (b)-[:R]->(c)`)...)

		mustRunTx(t, ctx, eng,
			`MERGE (b:Plain {k:'b'})-[:R]->(c:Plain {k:'c'}) ON MATCH SET b:Person`)

		if got := countQ(t, ctx, eng, `MATCH (n:Person) WHERE n.k = 'b' RETURN count(n) AS c`); got != 1 {
			t.Fatalf("b did not become a :Person through pattern MERGE (count=%d, want 1)", got)
		}
	})
}
