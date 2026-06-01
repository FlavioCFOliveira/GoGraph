package cypher_test

// create_constraint_test.go — T864
//
// Additive tests for CREATE CONSTRAINT. The basic DDL dispatch and violation
// tests already live in write_engine_test.go; this file adds:
//   - TestCreateConstraint_IfNotExists_Idempotent: IF NOT EXISTS succeeds twice.
//   - TestCreateConstraint_UniqueViolationViaCypher: two CREATE statements
//     sharing the same unique property value must fail.
//   - TestCreateConstraint_BackingIndexName: the backing index for a UNIQUE
//     constraint follows the "__uniq__Label.prop" naming convention.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCreateConstraint_IfNotExists_Idempotent verifies that
// CREATE CONSTRAINT … IF NOT EXISTS is idempotent: issuing it a second time
// does not error even though the constraint already exists.
func TestCreateConstraint_IfNotExists_Idempotent(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// The parser expects: CREATE CONSTRAINT [name] ON ... ASSERT ... IS UNIQUE [IF NOT EXISTS]
	// or:                CREATE CONSTRAINT [name] IF NOT EXISTS ON ... ASSERT ... IS UNIQUE
	// (name comes before IF NOT EXISTS per the DDL parser grammar).
	const q = `CREATE CONSTRAINT c_idem ON (n:User) ASSERT n.login IS UNIQUE IF NOT EXISTS`

	// First call must succeed.
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("first CREATE CONSTRAINT IF NOT EXISTS: %v", err)
	}
	drainResult(t, res)

	// Second call with the same name and IF NOT EXISTS must also succeed.
	res2, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("second CREATE CONSTRAINT IF NOT EXISTS: %v", err)
	}
	drainResult(t, res2)

	// Backing index must still be present (not double-registered, not dropped).
	mgr := g.IndexManager()
	if mgr == nil {
		t.Fatal("IndexManager must be non-nil")
	}
	if _, err := mgr.GetIndex("__uniq__User.login"); err != nil {
		t.Fatalf("backing unique index missing after idempotent CREATE CONSTRAINT: %v", err)
	}
}

// TestCreateConstraint_UniqueViolationViaCypher confirms that, after a UNIQUE
// constraint is established, a Cypher CREATE that produces a duplicate value
// is rejected during pipeline execution.
func TestCreateConstraint_UniqueViolationViaCypher(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g

	// Establish constraint.
	drainResult(t, mustRun(t, ctx, eng,
		`CREATE CONSTRAINT email_uniq ON (n:Member) ASSERT n.email IS UNIQUE`))

	// Insert first node — must succeed.
	res, err := eng.RunInTx(ctx, `CREATE (n:Member {email: "a@example.com"})`, nil)
	if err != nil {
		t.Fatalf("first CREATE: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("first CREATE iteration error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("first CREATE close: %v", err)
	}

	// Insert second node with the same email — must fail.
	res2, err := eng.RunInTx(ctx, `CREATE (n:Member {email: "a@example.com"})`, nil)
	if err != nil {
		// Build-time error is also acceptable.
		return
	}
	for res2.Next() {
	}
	iterErr := res2.Err()
	_ = res2.Close()
	if iterErr == nil {
		t.Fatal("expected unique constraint violation, got nil")
	}
}

// TestCreateConstraint_BackingIndexName verifies that the backing index for a
// UNIQUE constraint is named "__uniq__<Label>.<property>" by convention.
// This name is the lookup key used by the constraint enforcement path.
func TestCreateConstraint_BackingIndexName(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainResult(t, mustRun(t, ctx, eng,
		`CREATE CONSTRAINT order_ref ON (n:Order) ASSERT n.ref IS UNIQUE`))

	mgr := g.IndexManager()
	if mgr == nil {
		t.Fatal("IndexManager must be non-nil")
	}
	sub, err := mgr.GetIndex("__uniq__Order.ref")
	if err != nil {
		t.Fatalf("backing index __uniq__Order.ref not found: %v", err)
	}
	if sub.Kind() != "hash" {
		t.Errorf("backing index kind = %q, want \"hash\"", sub.Kind())
	}
}
