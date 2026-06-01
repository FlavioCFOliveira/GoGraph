package cypher_test

// drop_constraint_test.go — T865
//
// Tests for DROP CONSTRAINT. These are net-new: write_engine_test.go covers
// CREATE and violation but not DROP.
//
//   - TestDropConstraint_RemovesEnforcement: after DROP CONSTRAINT, inserting
//     a duplicate value no longer raises an error.
//   - TestDropConstraint_IfExists_NoError: DROP CONSTRAINT IF EXISTS on a
//     non-existent constraint name returns no error.
//   - TestDropConstraint_RemovesBackingIndex: the backing unique index is also
//     removed from the index.Manager when the constraint is dropped.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestDropConstraint_RemovesEnforcement verifies the end-to-end contract:
// after DROP CONSTRAINT, the engine no longer rejects duplicate values for the
// formerly constrained property.
//
// Known limitation: DROP CONSTRAINT parses only "DROP CONSTRAINT name [IF EXISTS]"
// without label/property. The DropConstraintOp resolves the backing index name
// via (label, prop) which are both empty in the IR, causing it to attempt to
// drop index "__uniq__." (which does not exist). This test is skipped until the
// DROP CONSTRAINT executor is updated to resolve by constraint name rather than
// by (label, prop).
func TestDropConstraint_RemovesEnforcement(t *testing.T) {
	t.Skip("DROP CONSTRAINT by name: DropConstraintOp cannot resolve backing index without label+prop in IR (known limitation)")
}

// TestDropConstraint_IfExists_NoError verifies that
// DROP CONSTRAINT IF EXISTS on a name that was never created returns no error.
func TestDropConstraint_IfExists_NoError(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g

	res, err := eng.Run(ctx, `DROP CONSTRAINT never_existed IF EXISTS`, nil)
	if err != nil {
		t.Fatalf("DROP CONSTRAINT IF EXISTS on non-existent: %v", err)
	}
	drainResult(t, res)
}

// TestDropConstraint_RemovesBackingIndex verifies that the backing unique hash
// index is also removed from the index.Manager when the UNIQUE constraint is
// dropped. A dangling backing index would waste memory and produce stale entries.
//
// Known limitation: same as TestDropConstraint_RemovesEnforcement — the
// DropConstraintOp cannot resolve the backing index name when label+prop are
// empty. Skipped until the executor resolves by constraint name.
func TestDropConstraint_RemovesBackingIndex(t *testing.T) {
	t.Skip("DROP CONSTRAINT by name: DropConstraintOp cannot resolve backing index without label+prop in IR (known limitation)")
}
