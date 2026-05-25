package schema_test

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/graph/lpg/schema"
)

// TestSchema_EnforceWrites verifies that a Graph with an installed SchemaValidator
// rejects property writes that violate the declared schema, while allowing
// conforming writes through unchanged.
func TestSchema_EnforceWrites(t *testing.T) {
	t.Parallel()

	g := lpg.New[int, int64](adjlist.Config{Directed: true})

	// Add nodes up-front; each subtest uses its own node ID to avoid
	// shared-state races between parallel subtests.
	for _, id := range []int{1, 2, 3, 4, 5} {
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode(%d): %v", id, err)
		}
	}

	// Pre-enablement write on node 1: must survive validator installation and
	// remain readable afterwards (node 1 is used only by this subtest).
	if err := g.SetNodeProperty(1, "age", lpg.Int64Value(99)); err != nil {
		t.Fatalf("pre-validator SetNodeProperty: %v", err)
	}

	s := schema.New(g.Registry(), g.PropertyKeys())
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}

	// Install the validator — *schema.Schema satisfies lpg.SchemaValidator because
	// it implements Validate(string, lpg.PropertyValue) error.
	g.SetValidator(s)

	// Pre-set value must still be visible after validator installation (node 1).
	t.Run("pre_enablement_value_survives", func(t *testing.T) {
		t.Parallel()
		v, ok := g.GetNodeProperty(1, "age")
		if !ok {
			t.Fatal("pre-set property must still be present after SetValidator")
		}
		i, _ := v.Int64()
		if i != 99 {
			t.Fatalf("pre-set value = %d, want 99", i)
		}
	})

	// Conforming write on node 2: correct type, must succeed and store the value.
	t.Run("conforming_write_accepted", func(t *testing.T) {
		t.Parallel()
		if err := g.SetNodeProperty(2, "age", lpg.Int64Value(30)); err != nil {
			t.Fatalf("conforming SetNodeProperty: %v", err)
		}
		v, ok := g.GetNodeProperty(2, "age")
		if !ok {
			t.Fatal("conforming write must store the value")
		}
		i, _ := v.Int64()
		if i != 30 {
			t.Fatalf("stored value = %d, want 30", i)
		}
	})

	// Non-conforming write on node 3: wrong type, must be rejected and graph
	// state must remain at the value established in this subtest.
	t.Run("type_mismatch_rejected", func(t *testing.T) {
		t.Parallel()
		// Establish a known good value on the isolated node.
		if err := g.SetNodeProperty(3, "age", lpg.Int64Value(30)); err != nil {
			t.Fatalf("setup SetNodeProperty: %v", err)
		}
		err := g.SetNodeProperty(3, "age", lpg.StringValue("wrong"))
		if err == nil {
			t.Fatal("expected error for type mismatch, got nil")
		}
		if !errors.Is(err, schema.ErrTypeMismatch) {
			t.Fatalf("expected ErrTypeMismatch, got %v", err)
		}
		// Graph state must be unchanged on this node.
		v, ok := g.GetNodeProperty(3, "age")
		if !ok {
			t.Fatal("property must still exist after rejected write")
		}
		i, _ := v.Int64()
		if i != 30 {
			t.Fatalf("value after rejected write = %d, want 30", i)
		}
	})

	// Unknown property on node 4: must be rejected with ErrUnknownProperty.
	t.Run("unknown_property_rejected", func(t *testing.T) {
		t.Parallel()
		err := g.SetNodeProperty(4, "unknown", lpg.StringValue("x"))
		if err == nil {
			t.Fatal("expected error for unknown property, got nil")
		}
		if !errors.Is(err, schema.ErrUnknownProperty) {
			t.Fatalf("expected ErrUnknownProperty, got %v", err)
		}
		// The unknown key must not have been written.
		if _, ok := g.GetNodeProperty(4, "unknown"); ok {
			t.Fatal("unknown property must not be stored after rejected write")
		}
	})
}

// TestSchema_EnforceEdgeWrites mirrors TestSchema_EnforceWrites for edge
// properties, exercising the SetEdgeProperty → validator path.
func TestSchema_EnforceEdgeWrites(t *testing.T) {
	t.Parallel()

	g := lpg.New[int, int64](adjlist.Config{Directed: true})
	// Add three independent edges, each used by a separate subtest.
	for _, e := range [][2]int{{1, 2}, {3, 4}, {5, 6}} {
		if err := g.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
		}
	}

	s := schema.New(g.Registry(), g.PropertyKeys())
	if _, err := s.RegisterProperty("weight", lpg.PropFloat64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	g.SetValidator(s)

	// Conforming edge-property write on edge (1,2).
	t.Run("conforming_edge_write_accepted", func(t *testing.T) {
		t.Parallel()
		if err := g.SetEdgeProperty(1, 2, "weight", lpg.Float64Value(0.5)); err != nil {
			t.Fatalf("conforming SetEdgeProperty: %v", err)
		}
		v, ok := g.GetEdgeProperty(1, 2, "weight")
		if !ok {
			t.Fatal("conforming edge write must store the value")
		}
		f, _ := v.Float64()
		if f != 0.5 {
			t.Fatalf("stored edge value = %v, want 0.5", f)
		}
	})

	// Type-mismatch edge-property write on edge (3,4).
	t.Run("type_mismatch_edge_rejected", func(t *testing.T) {
		t.Parallel()
		// Establish a known good value on the isolated edge.
		if err := g.SetEdgeProperty(3, 4, "weight", lpg.Float64Value(1.0)); err != nil {
			t.Fatalf("setup SetEdgeProperty: %v", err)
		}
		err := g.SetEdgeProperty(3, 4, "weight", lpg.Int64Value(99))
		if err == nil {
			t.Fatal("expected error for type mismatch on edge property, got nil")
		}
		if !errors.Is(err, schema.ErrTypeMismatch) {
			t.Fatalf("expected ErrTypeMismatch, got %v", err)
		}
		// Graph state must be unchanged on this edge.
		v, ok := g.GetEdgeProperty(3, 4, "weight")
		if !ok {
			t.Fatal("edge property must still exist after rejected write")
		}
		f, _ := v.Float64()
		if f != 1.0 {
			t.Fatalf("edge value after rejected write = %v, want 1.0", f)
		}
	})

	// Unknown edge property on edge (5,6).
	t.Run("unknown_edge_property_rejected", func(t *testing.T) {
		t.Parallel()
		err := g.SetEdgeProperty(5, 6, "no_such", lpg.StringValue("x"))
		if err == nil {
			t.Fatal("expected error for unknown edge property, got nil")
		}
		if !errors.Is(err, schema.ErrUnknownProperty) {
			t.Fatalf("expected ErrUnknownProperty, got %v", err)
		}
		if _, ok := g.GetEdgeProperty(5, 6, "no_such"); ok {
			t.Fatal("unknown edge property must not be stored after rejected write")
		}
	})
}
