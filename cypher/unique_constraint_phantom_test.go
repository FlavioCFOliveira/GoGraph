package cypher_test

// unique_constraint_phantom_test.go — regression tests for task #1342.
//
// Before the fix, the ConstraintRegistry value-set was append-only: DELETE,
// REMOVE, SET (old value), and rollback never released reservations, so a
// legitimate write was permanently rejected with "already exists" for the
// process lifetime.
//
// Each test must FAIL on the unmodified (pre-fix) code and PASS after.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newConstraintEngine(t *testing.T, label, prop string) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	q := `CREATE CONSTRAINT c_phantom ON (n:` + label + `) ASSERT n.` + prop + ` IS UNIQUE`
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	drainResult(t, res)
	return eng
}

// constraintMustWrite executes a write-only Cypher statement via RunInTx and
// fatals on any error.
func constraintMustWrite(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	ctx := context.Background()
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		t.Fatalf("RunInTx %q: %v", query, err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", query, err)
	}
}

// tryWrite executes a write-only Cypher statement via RunInTx and returns its
// error (RunInTx error or drain error, whichever comes first).
func tryWrite(eng *cypher.Engine, query string) error {
	ctx := context.Background()
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	return res.Err()
}

// TestUniqueConstraint_DeleteThenRecreate verifies that after a node with a
// unique property is deleted, a new node can be created with the same value.
// Pre-fix: the second CREATE returned ErrConstraintViolation because the
// value was never released from the registry.
func TestUniqueConstraint_DeleteThenRecreate(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")

	constraintMustWrite(t, eng, `CREATE (:Person {email: "alice@example.com"})`)
	constraintMustWrite(t, eng, `MATCH (n:Person {email: "alice@example.com"}) DETACH DELETE n`)

	// Recreate with the same value — must succeed after fix.
	err := tryWrite(eng, `CREATE (:Person {email: "alice@example.com"})`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("phantom reservation: CREATE after DELETE rejected with constraint violation (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on recreate: %v", err)
	}
}

// TestUniqueConstraint_RollbackThenCreate verifies that after a CREATE is
// rolled back inside an explicit transaction, the value is available again.
// Pre-fix: the registry still contained the value after rollback so the next
// CREATE was rejected.
func TestUniqueConstraint_RollbackThenCreate(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Account", "username")

	ctx := context.Background()

	// Open an explicit transaction, create a node, then roll back.
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	_, err = tx.Exec(`CREATE (:Account {username: "bob"})`, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec inside tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// After rollback, the value must be free; this CREATE must succeed.
	err = tryWrite(eng, `CREATE (:Account {username: "bob"})`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("phantom reservation: CREATE after Rollback rejected with constraint violation (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error after rollback: %v", err)
	}
}

// TestUniqueConstraint_SetReleasesOldValue verifies that after SET changes a
// node's unique property to a new value, the old value is available again.
// Pre-fix: RecordPropertySet added the new value but never released the old
// one, so a CREATE with the old value was rejected.
func TestUniqueConstraint_SetReleasesOldValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Product", "sku")

	constraintMustWrite(t, eng, `CREATE (:Product {sku: "SKU-001"})`)
	constraintMustWrite(t, eng, `MATCH (p:Product {sku: "SKU-001"}) SET p.sku = "SKU-002"`)

	// "SKU-001" must now be free — create another node with it.
	err := tryWrite(eng, `CREATE (:Product {sku: "SKU-001"})`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("phantom reservation: CREATE with old value rejected after SET (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error after SET: %v", err)
	}

	// "SKU-002" must still be in use — attempting to create a duplicate must fail.
	err = tryWrite(eng, `CREATE (:Product {sku: "SKU-002"})`)
	if err == nil {
		t.Fatal("expected constraint violation for SKU-002 duplicate, got nil")
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation for duplicate SKU-002, got: %v", err)
	}
}

// TestUniqueConstraint_RemovePropertyReleasesValue verifies that REMOVE n.prop
// frees the unique-constraint slot so a subsequent CREATE with the same value
// succeeds.
func TestUniqueConstraint_RemovePropertyReleasesValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Item", "code")

	constraintMustWrite(t, eng, `CREATE (:Item {code: "C-42"})`)
	constraintMustWrite(t, eng, `MATCH (i:Item {code: "C-42"}) REMOVE i.code`)

	// "C-42" must now be free.
	err := tryWrite(eng, `CREATE (:Item {code: "C-42"})`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("phantom reservation: CREATE after REMOVE rejected with constraint violation (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error after REMOVE: %v", err)
	}
}

// TestUniqueConstraint_AutocommitRollbackReleasesValue verifies that when an
// autocommit write fails because it violates the constraint (an internal
// rollback fires), previously recorded values from within that statement are
// cleaned up so the registry is left consistent with the rolled-back graph.
//
// Specifically: after a failed duplicate CREATE, the existing seed values
// must still be blocked (the constraint is alive) and new distinct values
// must be insertable (no phantom contamination).
func TestUniqueConstraint_AutocommitRollbackReleasesValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "User", "uid")

	constraintMustWrite(t, eng, `CREATE (:User {uid: "u-1"})`)
	constraintMustWrite(t, eng, `CREATE (:User {uid: "u-2"})`)

	// Attempt to create a duplicate — must fail.
	err := tryWrite(eng, `CREATE (:User {uid: "u-1"})`)
	if err == nil {
		t.Fatal("expected constraint violation, got nil")
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation, got: %v", err)
	}

	// u-1 and u-2 must still be blocked.
	for _, uid := range []string{"u-1", "u-2"} {
		if e := tryWrite(eng, `CREATE (:User {uid: "`+uid+`"})`); !errors.Is(e, exec.ErrConstraintViolation) {
			t.Fatalf("expected constraint violation for %s (seed node must still block), got: %v", uid, e)
		}
	}

	// A new, distinct value must be insertable — no phantom contamination.
	if err := tryWrite(eng, `CREATE (:User {uid: "u-3"})`); err != nil {
		t.Fatalf("CREATE u-3 after failed statement: %v", err)
	}

	// u-3 must now be in use.
	if e := tryWrite(eng, `CREATE (:User {uid: "u-3"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for u-3 dup, got: %v", e)
	}
}
