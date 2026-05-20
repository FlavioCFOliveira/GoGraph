package exec_test

// constraints_test.go — unit tests for ConstraintRegistry (task-296).

import (
	"errors"
	"testing"

	"go.uber.org/goleak"

	"gograph/cypher/exec"
	"gograph/graph/index"
	indexhash "gograph/graph/index/hash"
	"gograph/graph/lpg"
)

func TestConstraintRegistry_RegisterUnique_LooksUpName(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")
	name, ok := reg.UniqueIndexName("Person", "email")
	if !ok {
		t.Fatal("expected unique constraint to be registered")
	}
	if name != "__uniq__Person.email" {
		t.Errorf("unexpected index name %q", name)
	}
}

func TestConstraintRegistry_UnregisterUnique(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "idx")
	reg.UnregisterUnique("Person", "email")
	_, ok := reg.UniqueIndexName("Person", "email")
	if ok {
		t.Fatal("expected unique constraint to be absent after unregister")
	}
}

func TestConstraintRegistry_RegisterNotNull(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	if !reg.HasNotNull("Person", "name") {
		t.Fatal("expected not-null constraint to be registered")
	}
}

func TestConstraintRegistry_UnregisterNotNull(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	reg.UnregisterNotNull("Person", "name")
	if reg.HasNotNull("Person", "name") {
		t.Fatal("expected not-null constraint to be absent after unregister")
	}
}

func TestConstraintRegistry_CheckSetProperty_NoConstraint_OK(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	// No constraints: any value must pass.
	err := reg.CheckSetProperty([]string{"Person"}, "email", lpg.StringValue("a@b.com"), nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestConstraintRegistry_CheckSetProperty_NotNull_Null_Violation(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	// Zero PropertyValue (Kind == 0) is null.
	var null lpg.PropertyValue
	err := reg.CheckSetProperty([]string{"Person"}, "name", null, nil)
	if err == nil {
		t.Fatal("expected constraint violation, got nil")
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Errorf("expected ErrConstraintViolation, got %v", err)
	}
}

func TestConstraintRegistry_CheckSetProperty_NotNull_NonNull_OK(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	err := reg.CheckSetProperty([]string{"Person"}, "name", lpg.StringValue("Alice"), nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestConstraintRegistry_CheckSetProperty_Unique_Violation(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	idx := indexhash.New[string]()
	// Pre-populate the index with the value that will conflict.
	idx.Insert("alice@example.com", 1)
	if err := mgr.CreateIndex("__uniq__Person.email", idx); err != nil {
		t.Fatal(err)
	}

	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")

	err := reg.CheckSetProperty([]string{"Person"}, "email", lpg.StringValue("alice@example.com"), mgr)
	if err == nil {
		t.Fatal("expected unique constraint violation, got nil")
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Errorf("expected ErrConstraintViolation, got %v", err)
	}
}

func TestConstraintRegistry_CheckSetProperty_Unique_NoViolation(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	idx := indexhash.New[string]()
	if err := mgr.CreateIndex("__uniq__Person.email", idx); err != nil {
		t.Fatal(err)
	}

	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")

	// Index is empty, so no violation.
	err := reg.CheckSetProperty([]string{"Person"}, "email", lpg.StringValue("new@example.com"), mgr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestConstraintRegistry_CheckSetProperty_MultiLabel_Violation(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	// NOT NULL constraint on the second label in the slice.
	reg.RegisterNotNull("Employee", "empID")
	var null lpg.PropertyValue
	// Node has both Person and Employee labels.
	err := reg.CheckSetProperty([]string{"Person", "Employee"}, "empID", null, nil)
	if err == nil {
		t.Fatal("expected not-null violation via Employee label")
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Errorf("expected ErrConstraintViolation, got %v", err)
	}
}

func TestConstraintViolationError_Wraps(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	var null lpg.PropertyValue
	err := reg.CheckSetProperty([]string{"Person"}, "name", null, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// Must satisfy errors.Is for the sentinel.
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Errorf("errors.Is(err, ErrConstraintViolation) = false")
	}
	// Must be unwrappable to *ConstraintViolationError.
	var cve *exec.ConstraintViolationError
	if !errors.As(err, &cve) {
		t.Errorf("errors.As to *ConstraintViolationError = false")
	} else if cve.Label != "Person" || cve.Property != "name" || cve.Kind != "NOT NULL" {
		t.Errorf("unexpected ConstraintViolationError fields: %+v", cve)
	}
}
