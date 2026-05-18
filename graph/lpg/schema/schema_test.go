package schema

import (
	"errors"
	"sort"
	"sync"
	"testing"

	"gograph/graph/lpg"
)

func TestSchema_RegisterAndValidate(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	if err := s.Validate("age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("Validate age: %v", err)
	}
	if err := s.Validate("name", lpg.StringValue("Alice")); err != nil {
		t.Fatalf("Validate name: %v", err)
	}
	if err := s.Validate("age", lpg.StringValue("nope")); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
	if err := s.Validate("unknown", lpg.Int64Value(1)); !errors.Is(err, ErrUnknownProperty) {
		t.Fatalf("expected ErrUnknownProperty, got %v", err)
	}
}

func TestSchema_RegisterPropertyConflict(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	if _, err := s.RegisterProperty("score", lpg.PropInt64); err != nil {
		t.Fatalf("first RegisterProperty: %v", err)
	}
	if _, err := s.RegisterProperty("score", lpg.PropFloat64); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("conflict should return ErrTypeMismatch, got %v", err)
	}
	// Re-register with the same kind is idempotent.
	if _, err := s.RegisterProperty("score", lpg.PropInt64); err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
}

func TestSchema_Introspection(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	s.RegisterLabel("Person")
	s.RegisterLabel("Account")
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	labels := s.Labels()
	sort.Strings(labels)
	if len(labels) != 2 || labels[0] != "Account" || labels[1] != "Person" {
		t.Fatalf("Labels = %v", labels)
	}
	props := s.Properties()
	if len(props) != 2 || props["age"] != lpg.PropInt64 || props["name"] != lpg.PropString {
		t.Fatalf("Properties = %v", props)
	}
}

func TestSchema_Concurrent(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	const goroutines = 256
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func(w int) {
			defer wg.Done()
			s.RegisterLabel("Hot")
			if _, err := s.RegisterProperty("shared", lpg.PropInt64); err != nil {
				t.Errorf("RegisterProperty: %v", err)
			}
			if w&1 == 0 {
				_ = s.Validate("shared", lpg.Int64Value(int64(w)))
			}
		}(w)
	}
	wg.Wait()
	if labels := s.Labels(); len(labels) != 1 {
		t.Fatalf("Labels len = %d, want 1", len(labels))
	}
}

func BenchmarkSchema_RegisterPropertyHot(b *testing.B) {
	s := New(nil, nil)
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.RegisterProperty("age", lpg.PropInt64)
	}
}
