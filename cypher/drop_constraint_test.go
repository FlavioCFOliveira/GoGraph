package cypher_test

// drop_constraint_test.go — T865 / #1556
//
// Tests for DROP CONSTRAINT <name> [IF EXISTS]. Since #1556 the by-name drop
// resolves (kind, label, property) from the constraint registry, so it removes
// both the constraint and its UNIQUE backing index atomically:
//
//   - TestDropConstraint_RemovesEnforcement: after DROP CONSTRAINT, inserting
//     a duplicate value no longer raises an error.
//   - TestDropConstraint_IfExists_NoError: DROP CONSTRAINT IF EXISTS on a
//     non-existent constraint name returns no error (clean no-op).
//   - TestDropConstraint_RemovesBackingIndex: the backing unique index is also
//     removed from the index.Manager when the constraint is dropped.
//   - TestDropConstraint_Missing_NoIfExists_Errors: DROP CONSTRAINT on an
//     unknown name WITHOUT IF EXISTS fails with a typed error (never silent).
//   - TestDropConstraint_ThenRecreate: after dropping, the same UNIQUE
//     constraint can be re-created and enforces again.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runWriteRejected runs a write statement and reports whether it was rejected,
// either at build time (RunInTx returns an error) or during materialisation
// (the constraint violation surfaces via Result.Err on iteration).
func runWriteRejected(t *testing.T, ctx context.Context, eng *cypher.Engine, q string) bool { //nolint:revive // t first by testing convention
	t.Helper()
	res, err := eng.RunInTx(ctx, q, nil)
	if err != nil {
		return true
	}
	for res.Next() {
	}
	iterErr := res.Err()
	_ = res.Close()
	return iterErr != nil
}

// TestDropConstraint_RemovesEnforcement verifies the end-to-end contract:
// after DROP CONSTRAINT <name>, the engine no longer rejects duplicate values
// for the formerly constrained property.
func TestDropConstraint_RemovesEnforcement(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainResult(t, mustRun(t, ctx, eng,
		`CREATE CONSTRAINT acct_email ON (n:Acct) ASSERT n.email IS UNIQUE`))
	if runWriteRejected(t, ctx, eng, `CREATE (n:Acct {email:"a@x"})`) {
		t.Fatal("first insert was rejected")
	}
	// While the constraint is live the duplicate must be rejected.
	if !runWriteRejected(t, ctx, eng, `CREATE (n:Acct {email:"a@x"})`) {
		t.Fatal("duplicate accepted while UNIQUE constraint is live")
	}

	// Drop by name, then the duplicate must be accepted.
	drainResult(t, mustRun(t, ctx, eng, `DROP CONSTRAINT acct_email`))
	if runWriteRejected(t, ctx, eng, `CREATE (n:Acct {email:"a@x"})`) {
		t.Fatal("duplicate still rejected after DROP CONSTRAINT")
	}
}

// TestDropConstraint_IfExists_NoError verifies that DROP CONSTRAINT IF EXISTS
// on a name that was never created returns no error (clean no-op).
func TestDropConstraint_IfExists_NoError(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.Run(ctx, `DROP CONSTRAINT never_existed IF EXISTS`, nil)
	if err != nil {
		t.Fatalf("DROP CONSTRAINT IF EXISTS on non-existent: %v", err)
	}
	drainResult(t, res)
}

// TestDropConstraint_Missing_NoIfExists_Errors verifies that DROP CONSTRAINT on
// an unknown name WITHOUT IF EXISTS fails with a typed error wrapping
// exec.ErrConstraintNotFound — never a fail-silent success.
func TestDropConstraint_Missing_NoIfExists_Errors(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	_, err := eng.Run(ctx, `DROP CONSTRAINT never_existed`, nil)
	if err == nil {
		t.Fatal("DROP CONSTRAINT on a missing name without IF EXISTS must error, not succeed")
	}
	if !errors.Is(err, exec.ErrConstraintNotFound) {
		t.Fatalf("expected ErrConstraintNotFound, got %v", err)
	}
}

// TestDropConstraint_RemovesBackingIndex verifies that the backing unique hash
// index is also removed from the index.Manager when the UNIQUE constraint is
// dropped. A dangling backing index would block re-creation of the same
// constraint and produce stale entries.
func TestDropConstraint_RemovesBackingIndex(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainResult(t, mustRun(t, ctx, eng,
		`CREATE CONSTRAINT user_login ON (n:User) ASSERT n.login IS UNIQUE`))

	mgr := g.IndexManager()
	if mgr == nil {
		t.Fatal("IndexManager must be non-nil")
	}
	if _, err := mgr.GetIndex(exec.UniqueIndexName("User", "login")); err != nil {
		t.Fatalf("backing index missing before drop: %v", err)
	}

	drainResult(t, mustRun(t, ctx, eng, `DROP CONSTRAINT user_login`))

	if _, err := mgr.GetIndex(exec.UniqueIndexName("User", "login")); !errors.Is(err, index.ErrIndexNotFound) {
		t.Fatalf("backing index still present after DROP CONSTRAINT (GetIndex err: %v)", err)
	}
}

// TestDropConstraint_ThenRecreate verifies the re-creation contract: after a
// successful drop, the same UNIQUE constraint can be re-created cleanly and
// enforces again. This is the consequence the orphaned backing index used to
// permanently block.
func TestDropConstraint_ThenRecreate(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	const create = `CREATE CONSTRAINT dup_c ON (n:Thing) ASSERT n.key IS UNIQUE`
	drainResult(t, mustRun(t, ctx, eng, create))
	drainResult(t, mustRun(t, ctx, eng, `DROP CONSTRAINT dup_c`))

	// Re-create must succeed (no residual constraint, no residual index).
	res, err := eng.Run(ctx, create, nil)
	if err != nil {
		t.Fatalf("re-CREATE CONSTRAINT after drop: %v", err)
	}
	drainResult(t, res)

	// And it enforces again.
	if runWriteRejected(t, ctx, eng, `CREATE (n:Thing {key:"k"})`) {
		t.Fatal("first insert after re-create was rejected")
	}
	if !runWriteRejected(t, ctx, eng, `CREATE (n:Thing {key:"k"})`) {
		t.Fatal("re-created UNIQUE constraint does not enforce")
	}
}
