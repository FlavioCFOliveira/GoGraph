package cypher_test

// unique_constraint_selfset_readback_test.go — regression tests for the
// production-readiness audit finding [A1] (rmp #2032).
//
// Whole-entity SET (SET n = {…} / SET n += {…}) under an active UNIQUE
// constraint used to SILENTLY SKIP a constraint-violating property write
// (cypher/exec/set_all.go writeOne did a bare return on the CheckSetProperty
// error). The consequences were an ACID Atomicity/Consistency breach:
//   - a cross-node conflict left the statement PARTIALLY committed (the
//     non-violating siblings were written, the violating one dropped, no error);
//   - a self-preserving replace SET i = {pk: <same>, …} silently dropped the
//     constrained property to NULL because the node's own reservation was not
//     released before the check;
//   - clearTarget removed properties without releasing their reservations,
//     desyncing the constraint registry.
//
// The pre-existing unique_constraint_selfset_test.go asserted only err==nil and
// "value still blocked", which the bug satisfied — so it masked the data loss.
// These tests READ THE STORED VALUE BACK. Each must FAIL on the pre-fix code
// (wrong stored value / missing error) and PASS after: a violation now returns
// the error so the whole statement rolls back atomically, and a self-preserving
// replace succeeds with the constrained value intact.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// wantString fails unless row[col] is exactly the expected string value.
func wantString(t *testing.T, row map[string]interface{}, col, want string) {
	t.Helper()
	got, ok := row[col].(expr.StringValue)
	if !ok {
		t.Fatalf("%s: want StringValue(%q), got %T (%v)", col, want, row[col], row[col])
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", col, string(got), want)
	}
}

// wantAbsent fails unless row[col] is null/absent (never written).
func wantAbsent(t *testing.T, row map[string]interface{}, col string) {
	t.Helper()
	raw := row[col]
	if raw == nil {
		return // absent
	}
	v, ok := raw.(expr.Value)
	if !ok {
		t.Fatalf("%s: want null/absent, got %T (%v)", col, raw, raw)
	}
	if !expr.IsNull(v) {
		t.Fatalf("%s: want null/absent, got %T (%v)", col, v, v)
	}
}

// TestUniqueConstraint_SelfSetReplace_PreservesValue is the self-preserving
// replace case (single node, no cross-node conflict at all). Pre-fix: `code`
// was silently dropped to NULL; post-fix it survives with qty updated.
func TestUniqueConstraint_SelfSetReplace_PreservesValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Item", "code")
	constraintMustWrite(t, eng, `CREATE (:Item {code: "C-42", qty: 1})`)

	if err := tryWrite(eng, `MATCH (i:Item {code: "C-42"}) SET i = {code: "C-42", qty: 2}`); err != nil {
		t.Fatalf("self-preserving replace must succeed, got: %v", err)
	}

	row := singleRow(t, eng, `MATCH (i:Item {code: "C-42"}) RETURN i.code AS code, i.qty AS qty`)
	wantString(t, row, "code", "C-42") // pre-fix: NULL (silently dropped)
	if qty, ok := row["qty"].(expr.IntegerValue); !ok || int64(qty) != 2 {
		t.Fatalf("qty = %v, want 2", row["qty"])
	}
}

// TestUniqueConstraint_SelfSetReplace_CrossNodeRollsBack: SET n = {…} where a
// constrained value is owned by ANOTHER node must error and roll back the WHOLE
// statement (no partial commit, no silent NULL). Pre-fix: no error, the
// violating email was skipped but the sibling name was written and the node's
// own email was dropped.
func TestUniqueConstraint_SelfSetReplace_CrossNodeRollsBack(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")
	constraintMustWrite(t, eng, `CREATE (:Person {email: "a@example.com"})`)
	constraintMustWrite(t, eng, `CREATE (:Person {email: "b@example.com"})`)

	err := tryWrite(eng, `MATCH (n:Person {email: "b@example.com"}) SET n = {email: "a@example.com", name: "X"}`)
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("cross-node conflicting whole-entity SET must return ErrConstraintViolation, got: %v", err)
	}

	// The whole statement rolled back: b's email is intact and no name leaked.
	row := singleRow(t, eng, `MATCH (n:Person {email: "b@example.com"}) RETURN n.email AS email, n.name AS name`)
	wantString(t, row, "email", "b@example.com")
	wantAbsent(t, row, "name")
}

// TestUniqueConstraint_SelfSetAppend_CrossNodeRollsBack: SET n += {…} variant.
// Pre-fix: the conflicting email was skipped but nick was still written (partial
// commit) with no error.
func TestUniqueConstraint_SelfSetAppend_CrossNodeRollsBack(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")
	constraintMustWrite(t, eng, `CREATE (:Person {email: "a@example.com"})`)
	constraintMustWrite(t, eng, `CREATE (:Person {email: "b@example.com"})`)

	err := tryWrite(eng, `MATCH (n:Person {email: "b@example.com"}) SET n += {email: "a@example.com", nick: "Bobby"}`)
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("cross-node conflicting SET += must return ErrConstraintViolation, got: %v", err)
	}

	// No partial commit: nick must not have been written, email intact.
	row := singleRow(t, eng, `MATCH (n:Person {email: "b@example.com"}) RETURN n.email AS email, n.nick AS nick`)
	wantString(t, row, "email", "b@example.com")
	wantAbsent(t, row, "nick")
}

// TestUniqueConstraint_SelfSetReplace_RegistryConsistentAfterRollback verifies
// the registry is not desynced by the rolled-back statement: the conflicting
// value is still uniquely held (a new duplicate is rejected) and the node's own
// value remains reserved.
func TestUniqueConstraint_SelfSetReplace_RegistryConsistentAfterRollback(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")
	constraintMustWrite(t, eng, `CREATE (:Person {email: "a@example.com"})`)
	constraintMustWrite(t, eng, `CREATE (:Person {email: "b@example.com"})`)

	_ = tryWrite(eng, `MATCH (n:Person {email: "b@example.com"}) SET n = {email: "a@example.com"}`)

	if e := tryWrite(eng, `CREATE (:Person {email: "a@example.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("a@example.com must still be blocked after rollback, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:Person {email: "b@example.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("b@example.com must still be blocked after rollback, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:Person {email: "c@example.com"})`); e != nil {
		t.Fatalf("a distinct value must be insertable after rollback, got: %v", e)
	}
}
