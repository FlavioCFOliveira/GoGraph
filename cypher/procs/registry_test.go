package procs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
)

// stubImpl is a no-op procedure implementation used across tests.
func stubImpl(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
	return nil, nil
}

// sig returns a minimal Signature with the given namespace and name.
func sig(ns []string, name string, outputs ...procs.NamedType) procs.Signature {
	return procs.Signature{
		Namespace: ns,
		Name:      name,
		Outputs:   outputs,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Register
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_Register_Basic(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	s := sig([]string{"db"}, "labels", procs.NamedType{Name: "label", Kind: expr.KindString})
	if err := reg.Register(s, stubImpl); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestRegistry_Register_Duplicate(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	s := sig([]string{"db"}, "labels")
	_ = reg.Register(s, stubImpl)
	err := reg.Register(s, stubImpl)
	if err == nil {
		t.Fatal("expected ErrProcAlreadyExists, got nil")
	}
	if !errors.Is(err, procs.ErrProcAlreadyExists) {
		t.Errorf("error does not wrap ErrProcAlreadyExists: %v", err)
	}
}

func TestRegistry_Register_NoNamespace(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	s := sig(nil, "myProc")
	if err := reg.Register(s, stubImpl); err != nil {
		t.Fatalf("Register with no namespace: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Lookup
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_Lookup_Found(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	s := sig([]string{"db"}, "indexes",
		procs.NamedType{Name: "name", Kind: expr.KindString},
		procs.NamedType{Name: "type", Kind: expr.KindString},
	)
	_ = reg.Register(s, stubImpl)

	entry, err := reg.Lookup([]string{"db"}, "indexes")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry.Sig.Name != "indexes" {
		t.Errorf("Sig.Name = %q, want %q", entry.Sig.Name, "indexes")
	}
	if len(entry.Sig.Outputs) != 2 {
		t.Errorf("Outputs len = %d, want 2", len(entry.Sig.Outputs))
	}
}

func TestRegistry_Lookup_NotFound(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	_, err := reg.Lookup([]string{"db"}, "unknown")
	if err == nil {
		t.Fatal("expected ErrProcNotFound, got nil")
	}
	if !errors.Is(err, procs.ErrProcNotFound) {
		t.Errorf("error does not wrap ErrProcNotFound: %v", err)
	}
}

func TestRegistry_Lookup_WrongNamespace(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	_ = reg.Register(sig([]string{"db"}, "labels"), stubImpl)
	_, err := reg.Lookup([]string{"apoc"}, "labels")
	if !errors.Is(err, procs.ErrProcNotFound) {
		t.Errorf("expected ErrProcNotFound for wrong namespace, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_List_Empty(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	sigs := reg.List()
	if len(sigs) != 0 {
		t.Errorf("expected empty list, got %d entries", len(sigs))
	}
}

func TestRegistry_List_SortedOrder(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	_ = reg.Register(sig([]string{"db"}, "schema"), stubImpl)
	_ = reg.Register(sig([]string{"db"}, "labels"), stubImpl)
	_ = reg.Register(sig([]string{"db"}, "indexes"), stubImpl)

	sigs := reg.List()
	if len(sigs) != 3 {
		t.Fatalf("expected 3 signatures, got %d", len(sigs))
	}
	want := []string{"db.indexes", "db.labels", "db.schema"}
	for i, s := range sigs {
		got := s.Namespace[0] + "." + s.Name
		if got != want[i] {
			t.Errorf("sigs[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Custom registration
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_CustomRegistration(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	called := false
	impl := func(_ context.Context, args []expr.Value) ([][]expr.Value, error) {
		called = true
		return [][]expr.Value{{expr.StringValue("hello")}}, nil
	}
	s := sig([]string{"custom"}, "echo", procs.NamedType{Name: "val", Kind: expr.KindString})
	if err := reg.Register(s, impl); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, err := reg.Lookup([]string{"custom"}, "echo")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("Impl: %v", err)
	}
	if !called {
		t.Error("impl was not called")
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Errorf("unexpected rows: %v", rows)
	}
}
