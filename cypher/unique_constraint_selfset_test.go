package cypher_test

// unique_constraint_selfset_test.go — regression tests for H-C (rmp #1905).
//
// An idempotent self-set of a UNIQUE property — setting the property to the
// value the node already holds (SET n.k = n.k, SET n.k = <same literal>,
// SET n += {k: <same>}, SET n = {k: <same>}) — must succeed: the graph already
// satisfies the constraint, so rejecting the state it already accepts is a
// self-contradiction. Before the fix, CheckSetProperty ran before the node's
// own value was released, so the write saw its own reservation and was rejected
// with a UNIQUE violation.
//
// Each test must FAIL on the pre-fix code and PASS after. The final assertion
// in each guards against over-releasing: a genuine cross-node duplicate must
// still be rejected.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestUniqueConstraint_SelfSetLiteralSameValue: SET n.k = <its current value>.
func TestUniqueConstraint_SelfSetLiteralSameValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")

	constraintMustWrite(t, eng, `CREATE (:Person {email: "alice@example.com"})`)

	// Set the property to the value it already holds — must succeed.
	err := tryWrite(eng, `MATCH (n:Person {email: "alice@example.com"}) SET n.email = "alice@example.com"`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("idempotent self-set rejected as its own duplicate (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on self-set: %v", err)
	}

	// The value must still be uniquely held: a second node with it must fail.
	if e := tryWrite(eng, `CREATE (:Person {email: "alice@example.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for duplicate after self-set, got: %v", e)
	}
}

// TestUniqueConstraint_SelfSetPropertyReference: SET n.k = n.k.
func TestUniqueConstraint_SelfSetPropertyReference(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Account", "username")

	constraintMustWrite(t, eng, `CREATE (:Account {username: "bob"})`)

	err := tryWrite(eng, `MATCH (n:Account {username: "bob"}) SET n.username = n.username`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("SET n.k = n.k rejected as its own duplicate (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on SET n.k = n.k: %v", err)
	}

	if e := tryWrite(eng, `CREATE (:Account {username: "bob"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for duplicate after self-set, got: %v", e)
	}
}

// TestUniqueConstraint_SelfSetMapMerge: SET n += {k: <same value>} (+= merge),
// with an additional unchanged non-constrained field.
func TestUniqueConstraint_SelfSetMapMerge(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Product", "sku")

	constraintMustWrite(t, eng, `CREATE (:Product {sku: "SKU-001", name: "Widget"})`)

	err := tryWrite(eng, `MATCH (p:Product {sku: "SKU-001"}) SET p += {sku: "SKU-001", name: "Widget v2"}`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("SET n += {unchanged unique key} rejected as its own duplicate (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on += self-set: %v", err)
	}

	if e := tryWrite(eng, `CREATE (:Product {sku: "SKU-001"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for duplicate after += self-set, got: %v", e)
	}
}

// TestUniqueConstraint_SelfSetMapReplace: SET n = {k: <same value>} (replace
// all), keeping the constrained value unchanged.
func TestUniqueConstraint_SelfSetMapReplace(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Item", "code")

	constraintMustWrite(t, eng, `CREATE (:Item {code: "C-42", qty: 1})`)

	err := tryWrite(eng, `MATCH (i:Item {code: "C-42"}) SET i = {code: "C-42", qty: 2}`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("SET n = {unchanged unique key} rejected as its own duplicate (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on = self-set: %v", err)
	}

	if e := tryWrite(eng, `CREATE (:Item {code: "C-42"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for duplicate after = self-set, got: %v", e)
	}
}

// TestUniqueConstraint_CrossNodeDuplicateStillRejected is the control: the
// release-before-check fix must NOT weaken cross-node enforcement — setting one
// node's UNIQUE property to a value another live node already holds must fail.
func TestUniqueConstraint_CrossNodeDuplicateStillRejected(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "User", "uid")

	constraintMustWrite(t, eng, `CREATE (:User {uid: "u-1"})`)
	constraintMustWrite(t, eng, `CREATE (:User {uid: "u-2"})`)

	// SET u-2's uid to u-1's value — a genuine cross-node duplicate, must fail.
	err := tryWrite(eng, `MATCH (n:User {uid: "u-2"}) SET n.uid = "u-1"`)
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation for cross-node duplicate SET, got: %v", err)
	}

	// After the failed statement the registry must be consistent: u-2 still free
	// to nobody else, u-1 still blocked, and a new distinct value insertable.
	if e := tryWrite(eng, `CREATE (:User {uid: "u-1"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected u-1 still blocked after failed cross-node SET, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:User {uid: "u-2"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected u-2 still blocked after failed cross-node SET, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:User {uid: "u-9"})`); e != nil {
		t.Fatalf("distinct value must be insertable after failed cross-node SET, got: %v", e)
	}
}
