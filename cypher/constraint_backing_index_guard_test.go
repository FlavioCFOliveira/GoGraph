package cypher_test

// constraint_backing_index_guard_test.go — regression for #1912 at the engine
// boundary: a user DROP INDEX naming a UNIQUE constraint's __uniq__ backing
// index is rejected, and the constraint remains enforced.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestDropIndex_CannotDropConstraintBackingIndex(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email") // UNIQUE on (Person, email)
	ctx := context.Background()

	// The backing index is named __uniq__Person.email; dropping it directly
	// must be refused.
	if err := runDDL(ctx, eng, `DROP INDEX __uniq__Person.email`); err == nil {
		t.Fatal("expected DROP INDEX of the __uniq__ backing index to be rejected, got nil")
	}

	// The UNIQUE constraint must still be enforced.
	if e := tryWrite(eng, `CREATE (:Person {email: "a@b.com"})`); e != nil {
		t.Fatalf("first insert: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:Person {email: "a@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("constraint must still bite after rejected DROP INDEX, got: %v", e)
	}
}
