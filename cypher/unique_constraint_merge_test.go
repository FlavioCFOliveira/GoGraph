package cypher_test

// unique_constraint_merge_test.go — regression tests for H-B (rmp #1904).
//
// MERGE ... ON MATCH SET / ON CREATE SET overwrote a UNIQUE-constrained
// property without releasing the replaced value, so the old value leaked as a
// permanent phantom reservation (no live node held it, yet it was blocked for
// the process lifetime), and an idempotent MERGE self-set was rejected as its
// own duplicate. The MERGE property-set paths now release the old value before
// the check, mirroring the plain-SET fix (#1905).
//
// Each test must FAIL on the pre-fix code and PASS after.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestUniqueConstraint_MergeOnMatchSetReleasesOldValue verifies that after
// MERGE ... ON MATCH SET overwrites a UNIQUE value, the old value is free.
func TestUniqueConstraint_MergeOnMatchSetReleasesOldValue(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "User", "email")

	constraintMustWrite(t, eng, `CREATE (:User {email: "old@b.com"})`)
	// MERGE matches the existing node and rewrites its email.
	constraintMustWrite(t, eng, `MERGE (n:User {email: "old@b.com"}) ON MATCH SET n.email = "new@b.com"`)

	// "old@b.com" is no longer held by any live node — it must be reusable.
	err := tryWrite(eng, `CREATE (:User {email: "old@b.com"})`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("phantom reservation: value not released by MERGE ON MATCH SET (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error reusing released value: %v", err)
	}

	// "new@b.com" must still be uniquely held.
	if e := tryWrite(eng, `CREATE (:User {email: "new@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation for new@b.com duplicate, got: %v", e)
	}
}

// TestUniqueConstraint_MergeOnMatchSelfSet verifies that MERGE ... ON MATCH SET
// to the node's own current value succeeds (idempotent upsert).
func TestUniqueConstraint_MergeOnMatchSelfSet(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "User", "email")

	constraintMustWrite(t, eng, `CREATE (:User {email: "x@b.com"})`)
	err := tryWrite(eng, `MERGE (n:User {email: "x@b.com"}) ON MATCH SET n.email = "x@b.com"`)
	if errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatal("MERGE idempotent self-set rejected as its own duplicate (pre-fix behaviour)")
	}
	if err != nil {
		t.Fatalf("unexpected error on MERGE self-set: %v", err)
	}

	// The value must still be uniquely held.
	if e := tryWrite(eng, `CREATE (:User {email: "x@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected constraint violation after MERGE self-set, got: %v", e)
	}
}

// TestUniqueConstraint_MergeOnMatchCrossNodeDuplicate is the control: MERGE ...
// ON MATCH SET to a value another live node already holds must still be
// rejected (release-before-check must not weaken cross-node enforcement).
func TestUniqueConstraint_MergeOnMatchCrossNodeDuplicate(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "User", "email")

	constraintMustWrite(t, eng, `CREATE (:User {email: "a@b.com"})`)
	constraintMustWrite(t, eng, `CREATE (:User {email: "c@b.com"})`)

	// MERGE matches the c@b.com node, then tries to set its email to a@b.com,
	// which another live node holds — must fail.
	err := tryWrite(eng, `MERGE (n:User {email: "c@b.com"}) ON MATCH SET n.email = "a@b.com"`)
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("expected ErrConstraintViolation for MERGE cross-node duplicate, got: %v", err)
	}

	// Registry must remain consistent after the failed statement.
	if e := tryWrite(eng, `CREATE (:User {email: "a@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected a@b.com still blocked after failed MERGE, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:User {email: "c@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected c@b.com still blocked after failed MERGE, got: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:User {email: "z@b.com"})`); e != nil {
		t.Fatalf("distinct value must be insertable after failed MERGE, got: %v", e)
	}
}
