package exec_test

// drop_constraint_test.go — unit tests for DropConstraintOp (task-297).

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// setupUniqueConstraint creates a unique constraint on (label, prop) and
// returns the manager and registry ready for a subsequent DropConstraintOp.
func setupUniqueConstraint(t *testing.T, label, prop string) (*index.Manager, *exec.ConstraintRegistry) {
	t.Helper()
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()
	createOp := exec.NewCreateConstraintOp("c", label, prop, exec.ConstraintUnique, false, mgr, reg, nil)
	if err := createOp.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row exec.Row
	if _, err := createOp.Next(&row); err != nil {
		t.Fatal(err)
	}
	_ = createOp.Close()
	return mgr, reg
}

func TestDropConstraintOp_Unique_DropsIndexAndRegistry(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr, reg := setupUniqueConstraint(t, "Person", "email")

	op := exec.NewDropConstraintOp("c", "Person", "email", exec.ConstraintUnique, false, mgr, reg, nil)
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

	// Backing index must be gone.
	if _, lookupErr := mgr.GetIndex("__uniq__Person.email"); !errors.Is(lookupErr, index.ErrIndexNotFound) {
		t.Errorf("expected ErrIndexNotFound after drop, got %v", lookupErr)
	}
	// Registry must no longer know the constraint.
	if _, ok2 := reg.UniqueIndexName("Person", "email"); ok2 {
		t.Fatal("unique constraint still registered after drop")
	}
}

func TestDropConstraintOp_NotNull_UnregistersOnly(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")

	op := exec.NewDropConstraintOp("c", "Person", "name", exec.ConstraintNotNull, false, mgr, reg, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	_ = op.Close()

	if reg.HasNotNull("Person", "name") {
		t.Fatal("not-null constraint still registered after drop")
	}
}

func TestDropConstraintOp_NotNull_NotFound_Errors(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	// Attempt to drop a NOT NULL constraint that was never registered.
	op := exec.NewDropConstraintOp("c", "Person", "name", exec.ConstraintNotNull, false, mgr, reg, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err == nil {
		t.Fatal("expected error when dropping absent NOT NULL constraint")
	}
}

func TestDropConstraintOp_NotNull_IfExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	// IF EXISTS on an absent NOT NULL constraint must succeed silently.
	op := exec.NewDropConstraintOp("c", "Person", "name", exec.ConstraintNotNull, true, mgr, reg, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("IF EXISTS should not error: %v", err)
	}
}

func TestDropConstraintOp_Unique_IfExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	reg := exec.NewConstraintRegistry()

	// IF EXISTS on an absent UNIQUE constraint must succeed silently.
	op := exec.NewDropConstraintOp("c", "Person", "email", exec.ConstraintUnique, true, mgr, reg, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("IF EXISTS should not error: %v", err)
	}
}
