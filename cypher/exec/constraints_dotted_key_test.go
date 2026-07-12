package exec

// constraints_dotted_key_test.go — regression for #1916: with the old
// "label.prop" string key split on the last dot, ("A.b","c") and ("A","b.c")
// collided on the same key "A.b.c", mis-attributing a constraint. The struct
// key keeps label and property distinct so no such aliasing is possible. These
// pairs are only reachable via the Go API (the Cypher parser cannot emit a
// dotted property), but the ACID Consistency mandate requires them to be exact.

import "testing"

func TestConstraintRegistry_DottedKeyNoAlias_NotNull(t *testing.T) {
	t.Parallel()
	reg := NewConstraintRegistry()
	reg.RegisterNotNull("A.b", "c")
	reg.RegisterNotNull("A", "b.c")

	if reg.Count() != 2 {
		t.Fatalf("expected 2 distinct constraints, got %d", reg.Count())
	}
	if !reg.HasNotNull("A.b", "c") {
		t.Error(`HasNotNull("A.b","c") should be true`)
	}
	if !reg.HasNotNull("A", "b.c") {
		t.Error(`HasNotNull("A","b.c") should be true`)
	}
	// The label index must not conflate the two labels either.
	if got := reg.NotNullProperties("A.b"); len(got) != 1 || got[0] != "c" {
		t.Errorf(`NotNullProperties("A.b") = %v, want ["c"]`, got)
	}
	if got := reg.NotNullProperties("A"); len(got) != 1 || got[0] != "b.c" {
		t.Errorf(`NotNullProperties("A") = %v, want ["b.c"]`, got)
	}

	// Dropping one must leave the other intact.
	reg.UnregisterNotNull("A.b", "c")
	if reg.HasNotNull("A.b", "c") {
		t.Error(`("A.b","c") should be gone after Unregister`)
	}
	if !reg.HasNotNull("A", "b.c") {
		t.Error(`("A","b.c") must be unaffected by dropping ("A.b","c")`)
	}
}

func TestConstraintRegistry_DottedKeyNoAlias_Unique(t *testing.T) {
	t.Parallel()
	reg := NewConstraintRegistry()
	reg.RegisterUnique("A.b", "c", "idx1")
	reg.SetConstraintName(true, "A.b", "c", "cn1")
	reg.RegisterUnique("A", "b.c", "idx2")
	reg.SetConstraintName(true, "A", "b.c", "cn2")

	if n, ok := reg.UniqueIndexName("A.b", "c"); !ok || n != "idx1" {
		t.Errorf(`UniqueIndexName("A.b","c") = %q,%v, want "idx1",true`, n, ok)
	}
	if n, ok := reg.UniqueIndexName("A", "b.c"); !ok || n != "idx2" {
		t.Errorf(`UniqueIndexName("A","b.c") = %q,%v, want "idx2",true`, n, ok)
	}
	// ResolveByName maps each declared name to its exact (label, prop).
	if _, l, p, ok := reg.ResolveByName("cn1"); !ok || l != "A.b" || p != "c" {
		t.Errorf(`ResolveByName("cn1") = %q,%q,%v, want "A.b","c",true`, l, p, ok)
	}
	if _, l, p, ok := reg.ResolveByName("cn2"); !ok || l != "A" || p != "b.c" {
		t.Errorf(`ResolveByName("cn2") = %q,%q,%v, want "A","b.c",true`, l, p, ok)
	}
}
