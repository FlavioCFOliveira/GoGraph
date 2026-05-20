package exec_test

// create_constraint_test.go — unit tests for CreateConstraintOp (task-296).

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"gograph/cypher/exec"
	"gograph/graph/index"
)

func TestCreateConstraintOp_Unique_CreatesIndex(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()
	op := exec.NewCreateConstraintOp("person_email_unique", "Person", "email",
		exec.ConstraintUnique, false, mgr, reg)

	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if ok {
		t.Fatal("DDL operator should emit no rows")
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}

	// Backing index must now exist in the manager.
	sub, lookupErr := mgr.GetIndex("__uniq__Person.email")
	if lookupErr != nil {
		t.Fatalf("backing index not found: %v", lookupErr)
	}
	if sub.Kind() != "hash" {
		t.Errorf("expected hash backing index, got %q", sub.Kind())
	}

	// Registry must know the unique constraint.
	name, registered := reg.UniqueIndexName("Person", "email")
	if !registered {
		t.Fatal("unique constraint not registered")
	}
	if name != "__uniq__Person.email" {
		t.Errorf("unexpected backing index name %q", name)
	}
}

func TestCreateConstraintOp_NotNull_RegistersOnly(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()
	op := exec.NewCreateConstraintOp("person_name_notnull", "Person", "name",
		exec.ConstraintNotNull, false, mgr, reg)

	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	_ = op.Close()

	// No backing index should be created.
	names := mgr.ListIndexes()
	if len(names) > 0 {
		t.Errorf("expected no indexes for NOT NULL constraint, got %v", names)
	}

	// Registry must know the not-null constraint.
	if !reg.HasNotNull("Person", "name") {
		t.Fatal("not-null constraint not registered")
	}
}

func TestCreateConstraintOp_Unique_Duplicate_Errors(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	createOne := func() {
		op := exec.NewCreateConstraintOp("c", "Person", "email",
			exec.ConstraintUnique, false, mgr, reg)
		_ = op.Init(context.Background())
		var row exec.Row
		_, _ = op.Next(&row)
		_ = op.Close()
	}
	createOne()

	// Second create without IF NOT EXISTS must error.
	op := exec.NewCreateConstraintOp("c", "Person", "email",
		exec.ConstraintUnique, false, mgr, reg)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err == nil {
		t.Fatal("expected error for duplicate unique constraint")
	}
}

func TestCreateConstraintOp_Unique_IfNotExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	first := exec.NewCreateConstraintOp("c", "Person", "email",
		exec.ConstraintUnique, false, mgr, reg)
	_ = first.Init(context.Background())
	var row exec.Row
	_, _ = first.Next(&row)
	_ = first.Close()

	// Second create with IF NOT EXISTS must succeed silently.
	second := exec.NewCreateConstraintOp("c", "Person", "email",
		exec.ConstraintUnique, true, mgr, reg)
	_ = second.Init(context.Background())
	_, err := second.Next(&row)
	if err != nil {
		t.Fatalf("IF NOT EXISTS should not error: %v", err)
	}
}

func TestCreateConstraintOp_NotNull_IfNotExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	first := exec.NewCreateConstraintOp("c", "Person", "name",
		exec.ConstraintNotNull, false, mgr, reg)
	_ = first.Init(context.Background())
	var row exec.Row
	_, _ = first.Next(&row)
	_ = first.Close()

	// Second create with IF NOT EXISTS must succeed silently.
	second := exec.NewCreateConstraintOp("c", "Person", "name",
		exec.ConstraintNotNull, true, mgr, reg)
	_ = second.Init(context.Background())
	_, err := second.Next(&row)
	if err != nil {
		t.Fatalf("IF NOT EXISTS should not error: %v", err)
	}
}
