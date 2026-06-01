package schema

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestSchema_Required(t *testing.T) {
	t.Parallel()

	newSchema := func() *Schema {
		s := New(nil, nil)
		if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
			t.Fatalf("RegisterProperty name: %v", err)
		}
		if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
			t.Fatalf("RegisterProperty age: %v", err)
		}
		s.RequireProperty("Person", "name")
		return s
	}

	t.Run("present required property passes", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		props := map[string]lpg.PropertyValue{
			"name": lpg.StringValue("Alice"),
		}
		if err := s.ValidateNode([]string{"Person"}, props); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("missing required property returns ErrMissingRequired", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		err := s.ValidateNode([]string{"Person"}, map[string]lpg.PropertyValue{})
		if !errors.Is(err, ErrMissingRequired) {
			t.Fatalf("expected ErrMissingRequired, got %v", err)
		}
		if !strings.Contains(err.Error(), "Person") {
			t.Errorf("error message missing label name: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "name") {
			t.Errorf("error message missing property key: %q", err.Error())
		}
	})

	t.Run("label without requirements passes with empty props", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		if err := s.ValidateNode([]string{"Account"}, map[string]lpg.PropertyValue{}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("multiple labels one missing required returns error", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		// "Person" requires "name"; props only has "email" (unregistered, ignored).
		props := map[string]lpg.PropertyValue{
			"email": lpg.StringValue("a@b.com"),
		}
		err := s.ValidateNode([]string{"Account", "Person"}, props)
		if !errors.Is(err, ErrMissingRequired) {
			t.Fatalf("expected ErrMissingRequired, got %v", err)
		}
	})

	t.Run("RequireProperty is idempotent", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		// Register same requirement twice.
		s.RequireProperty("Person", "name")
		s.RequireProperty("Person", "name")
		// Should still behave identically — exactly one entry.
		s.mu.RLock()
		count := len(s.required["Person"])
		s.mu.RUnlock()
		if count != 1 {
			t.Fatalf("expected 1 required entry, got %d", count)
		}
		// Validation still works correctly.
		props := map[string]lpg.PropertyValue{"name": lpg.StringValue("Bob")}
		if err := s.ValidateNode([]string{"Person"}, props); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("unregistered label requirement is stored and checked", func(t *testing.T) {
		t.Parallel()
		s := New(nil, nil)
		if _, err := s.RegisterProperty("email", lpg.PropString); err != nil {
			t.Fatalf("RegisterProperty email: %v", err)
		}
		// "Org" label never registered via RegisterLabel, but requirement still works.
		s.RequireProperty("Org", "email")
		err := s.ValidateNode([]string{"Org"}, map[string]lpg.PropertyValue{})
		if !errors.Is(err, ErrMissingRequired) {
			t.Fatalf("expected ErrMissingRequired, got %v", err)
		}
	})

	t.Run("ValidateNode also catches type mismatch on present props", func(t *testing.T) {
		t.Parallel()
		s := newSchema()
		// "name" is present but with wrong kind (Int64 instead of String).
		props := map[string]lpg.PropertyValue{
			"name": lpg.Int64Value(42),
		}
		err := s.ValidateNode([]string{"Person"}, props)
		if !errors.Is(err, ErrTypeMismatch) {
			t.Fatalf("expected ErrTypeMismatch, got %v", err)
		}
	})
}
