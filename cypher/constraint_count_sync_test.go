package cypher_test

// constraint_count_sync_test.go — regression for #1917: Graph.HasConstraints
// must reflect a just-created constraint. The count is now flipped inside the
// ApplyAtomically barrier (atomically with registration) rather than only on a
// deferred call after the writer lock is released, closing the window in which
// a concurrent checkpoint could see the constraint registered but the count
// stale. The crash-window itself is exercised by the checkpoint crash tests;
// this guards the end-state.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestCreateConstraint_HasConstraintsSynced(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if g.HasConstraints() {
		t.Fatal("fresh graph must report no constraints")
	}
	if _, err := eng.Run(ctx, `CREATE CONSTRAINT u FOR (n:User) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if !g.HasConstraints() {
		t.Fatal("Graph.HasConstraints must be true after CREATE CONSTRAINT")
	}

	if err := runDDL(eng, ctx, `DROP CONSTRAINT u`); err != nil {
		t.Fatalf("DROP CONSTRAINT: %v", err)
	}
	if g.HasConstraints() {
		t.Fatal("Graph.HasConstraints must be false after the last constraint is dropped")
	}
}
