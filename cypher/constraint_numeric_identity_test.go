package cypher_test

// constraint_numeric_identity_test.go — end-to-end regression for #1910: under a
// UNIQUE constraint an integer and a numerically-equal float are the same value
// (aligned with openCypher = and MERGE), while distinct numbers and distinct
// kinds are not.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestUniqueConstraint_IntFloatCollide(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "P", "v")

	// int 1, then float 1.0 — must be rejected as a duplicate.
	constraintMustWrite(t, eng, `CREATE (:P {v: 1})`)
	if err := tryWrite(eng, `CREATE (:P {v: 1.0})`); !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("float 1.0 must violate UNIQUE against int 1, got: %v", err)
	}
}

func TestUniqueConstraint_DistinctNumbersAndKindsPass(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "P", "v")

	constraintMustWrite(t, eng, `CREATE (:P {v: 2})`)
	// A distinct float value must pass.
	if err := tryWrite(eng, `CREATE (:P {v: 3.5})`); err != nil {
		t.Fatalf("distinct float 3.5 must pass: %v", err)
	}
	// A string "2" is a different kind — not a duplicate of int 2.
	if err := tryWrite(eng, `CREATE (:P {v: "2"})`); err != nil {
		t.Fatalf("string \"2\" must not collide with int 2: %v", err)
	}
}
