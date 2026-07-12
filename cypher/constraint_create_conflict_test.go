package cypher_test

// constraint_create_conflict_test.go — regression for #1907 (name uniqueness /
// deterministic drop) and #1908 (consistent already-exists, no backing-index
// name leak).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newPlainEngine(t *testing.T) (*cypher.Engine, context.Context) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g), context.Background()
}

// runDDL runs a DDL statement and returns its error.
func runDDL(eng *cypher.Engine, ctx context.Context, q string) error {
	_, err := eng.Run(ctx, q, nil)
	return err
}

func TestCreateConstraint_DuplicateNameRejected(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)

	if err := runDDL(eng, ctx, `CREATE CONSTRAINT dup FOR (n:A) REQUIRE n.x IS UNIQUE`); err != nil {
		t.Fatalf("first CREATE: %v", err)
	}
	// Same name, different (label, property): must be a name conflict.
	err := runDDL(eng, ctx, `CREATE CONSTRAINT dup FOR (n:B) REQUIRE n.y IS UNIQUE`)
	if !errors.Is(err, exec.ErrConstraintNameConflict) {
		t.Fatalf("expected ErrConstraintNameConflict, got: %v", err)
	}
	// Same name, different kind: also a conflict.
	err = runDDL(eng, ctx, `CREATE CONSTRAINT dup FOR (n:C) REQUIRE n.z IS NOT NULL`)
	if !errors.Is(err, exec.ErrConstraintNameConflict) {
		t.Fatalf("expected ErrConstraintNameConflict across kinds, got: %v", err)
	}
}

func TestCreateConstraint_EquivalentAlreadyExists(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)

	// UNIQUE: re-declaring the same (label, property) under a different name is
	// an equivalent-already-exists error that must not leak the __uniq__ index.
	if err := runDDL(eng, ctx, `CREATE CONSTRAINT c1 FOR (n:A) REQUIRE n.email IS UNIQUE`); err != nil {
		t.Fatalf("first UNIQUE CREATE: %v", err)
	}
	err := runDDL(eng, ctx, `CREATE CONSTRAINT c1b FOR (n:A) REQUIRE n.email IS UNIQUE`)
	if !errors.Is(err, exec.ErrConstraintAlreadyExists) {
		t.Fatalf("expected ErrConstraintAlreadyExists for UNIQUE, got: %v", err)
	}
	if strings.Contains(err.Error(), "__uniq__") {
		t.Fatalf("error leaks the internal backing-index name: %q", err.Error())
	}

	// NOT NULL: re-declaring must ALSO be a hard error (previously silently
	// succeeded and overwrote the stored name).
	if err := runDDL(eng, ctx, `CREATE CONSTRAINT n1 FOR (n:B) REQUIRE n.title IS NOT NULL`); err != nil {
		t.Fatalf("first NOT NULL CREATE: %v", err)
	}
	err = runDDL(eng, ctx, `CREATE CONSTRAINT n1 FOR (n:B) REQUIRE n.title IS NOT NULL`)
	if !errors.Is(err, exec.ErrConstraintAlreadyExists) {
		t.Fatalf("expected ErrConstraintAlreadyExists for NOT NULL, got: %v", err)
	}
}

func TestCreateConstraint_IfNotExistsAbsorbsEquivalent(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)

	if err := runDDL(eng, ctx, `CREATE CONSTRAINT c FOR (n:A) REQUIRE n.x IS UNIQUE`); err != nil {
		t.Fatalf("first CREATE: %v", err)
	}
	if err := runDDL(eng, ctx, `CREATE CONSTRAINT c IF NOT EXISTS FOR (n:A) REQUIRE n.x IS UNIQUE`); err != nil {
		t.Fatalf("IF NOT EXISTS must be a silent no-op, got: %v", err)
	}
}

// TestDropConstraint_ByNameDeterministic verifies that after names are unique,
// DROP CONSTRAINT <name> removes exactly the named constraint and leaves the
// other in force.
func TestDropConstraint_ByNameDeterministic(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)

	if err := runDDL(eng, ctx, `CREATE CONSTRAINT c_a FOR (n:A) REQUIRE n.x IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_a: %v", err)
	}
	if err := runDDL(eng, ctx, `CREATE CONSTRAINT c_b FOR (n:B) REQUIRE n.y IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_b: %v", err)
	}
	if err := runDDL(eng, ctx, `DROP CONSTRAINT c_a`); err != nil {
		t.Fatalf("DROP c_a: %v", err)
	}

	// c_a is gone: its (A, x) UNIQUE no longer bites — duplicates are accepted.
	if e := tryWrite(eng, `CREATE (:A {x: "v"})`); e != nil {
		t.Fatalf("insert A: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:A {x: "v"})`); e != nil {
		t.Fatalf("A duplicate must be accepted after DROP c_a, got: %v", e)
	}
	// c_b remains: its (B, y) UNIQUE still bites.
	if e := tryWrite(eng, `CREATE (:B {y: "w"})`); e != nil {
		t.Fatalf("insert B: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:B {y: "w"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("c_b must still bite, expected violation, got: %v", e)
	}
}
