package schema

import (
	"testing"

	"gograph/graph/lpg"
)

// TestSchema_PropertyKind covers the introspection helper that
// surfaces the declared kind for a registered property name.
func TestSchema_PropertyKind(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	if _, ok := s.PropertyKind("missing"); ok {
		t.Fatalf("PropertyKind of unregistered key returned ok=true")
	}
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	kind, ok := s.PropertyKind("age")
	if !ok {
		t.Fatal("PropertyKind: ok=false for registered key")
	}
	if kind != lpg.PropInt64 {
		t.Fatalf("PropertyKind = %d, want PropInt64", kind)
	}
}

// TestSchema_LabelRegistry verifies that the schema exposes the
// underlying label registry it was constructed with, so callers can
// reuse the same registry across an [lpg.Graph] and its schema.
func TestSchema_LabelRegistry(t *testing.T) {
	t.Parallel()
	reg := lpg.NewLabelRegistry()
	s := New(reg, nil)
	if s.LabelRegistry() != reg {
		t.Fatal("LabelRegistry() did not return the registry passed to New")
	}
	// Default-constructed schema returns a non-nil internal registry.
	def := New(nil, nil)
	if def.LabelRegistry() == nil {
		t.Fatal("default schema returned nil LabelRegistry")
	}
}

// TestSchema_PropertyKeyRegistry verifies that the schema exposes
// the underlying property-key registry.
func TestSchema_PropertyKeyRegistry(t *testing.T) {
	t.Parallel()
	reg := lpg.NewPropertyKeyRegistry()
	s := New(nil, reg)
	if s.PropertyKeyRegistry() != reg {
		t.Fatal("PropertyKeyRegistry() did not return the registry passed to New")
	}
	// Cross-check: RegisterProperty must mint an ID using the
	// caller-provided registry.
	id, err := s.RegisterProperty("age", lpg.PropInt64)
	if err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	if got, ok := reg.Lookup("age"); !ok || got != id {
		t.Fatalf("registry lookup mismatch: got (%d, %v), want (%d, true)", got, ok, id)
	}
}

// TestSchema_RegisterLabel_DoubleCheck deterministically exercises
// the post-write-lock double-check branch of RegisterLabel.
func TestSchema_RegisterLabel_DoubleCheck(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	a := s.RegisterLabel("Foo")
	b := s.RegisterLabel("Foo")
	if a != b {
		t.Fatalf("RegisterLabel not idempotent: %d != %d", a, b)
	}
}
