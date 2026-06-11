package lpg

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// ageValidator rejects any value written under the key "age" that is not an
// int64 (i.e. a non-Integer PropertyValue). It is intentionally narrow so
// the test exercises exactly the bypass the fix closes.
type ageValidator struct{}

var errAgeMustBeInt = errors.New("age must be an integer")

func (ageValidator) Validate(key string, value PropertyValue) error {
	if key != "age" {
		return nil
	}
	if _, ok := value.Int64(); !ok {
		return errAgeMustBeInt
	}
	return nil
}

// newValidatedGraph builds a directed, non-multigraph with an ageValidator
// installed and two connected nodes a→b plus one edge handle h.
func newValidatedGraph(t *testing.T) (g *Graph[string, int64], h uint64) {
	t.Helper()
	g = New[string, int64](adjlist.Config{Directed: true})
	g.SetValidator(ageValidator{})

	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}

	// Use AddEdgeH so we have a handle to pass to SetEdgePropertyByHandle.
	cfg := adjlist.Config{Directed: true, Multigraph: true}
	gm := New[string, int64](cfg)
	gm.SetValidator(ageValidator{})
	if err := gm.AddNode("a"); err != nil {
		t.Fatalf("AddNode a (multigraph): %v", err)
	}
	if err := gm.AddNode("b"); err != nil {
		t.Fatalf("AddNode b (multigraph): %v", err)
	}
	handle, err := gm.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	return gm, handle
}

// TestValidatorBypass_SetEdgeProperty_Baseline verifies that the existing
// SetEdgeProperty correctly rejects invalid values (baseline, pre-existing
// behaviour that must not regress).
func TestValidatorBypass_SetEdgeProperty_Baseline(t *testing.T) {
	t.Parallel()
	g, _ := newValidatedGraph(t)

	// Valid write — must succeed.
	if err := g.SetEdgeProperty("a", "b", "age", Int64Value(30)); err != nil {
		t.Fatalf("SetEdgeProperty valid: unexpected error: %v", err)
	}

	// Invalid write — must be rejected.
	err := g.SetEdgeProperty("a", "b", "age", StringValue("oops"))
	if err == nil {
		t.Fatal("SetEdgeProperty: expected error for string age, got nil")
	}
	if !errors.Is(err, errAgeMustBeInt) {
		t.Fatalf("SetEdgeProperty: wrong error: %v", err)
	}

	// Confirm the invalid value was NOT stored.
	props := g.EdgeProperties("a", "b")
	if v, ok := props["age"]; ok {
		if _, isInt := v.Int64(); !isInt {
			t.Fatalf("SetEdgeProperty: stored invalid value %v after rejection", v)
		}
	}
}

// TestValidatorBypass_SetEdgePropertyByHandle verifies that
// SetEdgePropertyByHandle runs the SchemaValidator and rejects invalid values.
func TestValidatorBypass_SetEdgePropertyByHandle(t *testing.T) {
	t.Parallel()
	g, handle := newValidatedGraph(t)

	// Valid write — must succeed.
	if err := g.SetEdgePropertyByHandle("a", "b", handle, "age", Int64Value(25)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle valid: unexpected error: %v", err)
	}

	// Invalid write — must be rejected.
	err := g.SetEdgePropertyByHandle("a", "b", handle, "age", StringValue("oops"))
	if err == nil {
		t.Fatal("SetEdgePropertyByHandle: expected error for string age, got nil")
	}
	if !errors.Is(err, errAgeMustBeInt) {
		t.Fatalf("SetEdgePropertyByHandle: wrong error: %v", err)
	}

	// Confirm the invalid value was NOT stored — per-handle bag should still
	// hold only the previously written int64(25).
	props := g.EdgePropertiesByHandle("a", "b", handle)
	if v, ok := props["age"]; ok {
		if i, isInt := v.Int64(); !isInt || i != 25 {
			t.Fatalf("SetEdgePropertyByHandle: stored invalid value %v after rejection", v)
		}
	}
}

// TestValidatorBypass_SetEdgePropertyAt verifies that SetEdgePropertyAt runs
// the SchemaValidator and rejects invalid values.
func TestValidatorBypass_SetEdgePropertyAt(t *testing.T) {
	t.Parallel()
	g, _ := newValidatedGraph(t)

	const idx = int64(1)

	// Valid write — must succeed.
	if err := g.SetEdgePropertyAt("a", "b", idx, "age", Int64Value(20)); err != nil {
		t.Fatalf("SetEdgePropertyAt valid: unexpected error: %v", err)
	}

	// Invalid write — must be rejected.
	err := g.SetEdgePropertyAt("a", "b", idx, "age", StringValue("oops"))
	if err == nil {
		t.Fatal("SetEdgePropertyAt: expected error for string age, got nil")
	}
	if !errors.Is(err, errAgeMustBeInt) {
		t.Fatalf("SetEdgePropertyAt: wrong error: %v", err)
	}

	// Confirm the invalid value was NOT stored — per-instance bag should
	// still hold only the previously written int64(20).
	props := g.EdgePropertiesAt("a", "b", idx)
	if v, ok := props["age"]; ok {
		if i, isInt := v.Int64(); !isInt || i != 20 {
			t.Fatalf("SetEdgePropertyAt: stored invalid value %v after rejection", v)
		}
	}
}
