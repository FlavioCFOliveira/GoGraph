package index

import (
	"testing"
	"unsafe"
)

// nodeset_layout_test.go — pins the 16-byte two-word layout of NodeSet that
// is the entire justification for the #1596 packing. A stray field or a
// non-pointer-width tag would silently bloat the struct and the per-entry
// memory win would evaporate, so the size and alignment are asserted as
// evidence rather than assumed.

func TestNodeSet_Layout16Bytes(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(NodeSet{}); got != 16 {
		t.Fatalf("unsafe.Sizeof(NodeSet{}) = %d, want 16 (two machine words)", got)
	}
	if got := unsafe.Alignof(NodeSet{}); got != 8 {
		t.Fatalf("unsafe.Alignof(NodeSet{}) = %d, want 8", got)
	}
}

// TestNodeSet_ZeroValueEmpty confirms the zero value is the empty state, so a
// map miss / slice grow yields a usable empty set with no pointer set.
func TestNodeSet_ZeroValueEmpty(t *testing.T) {
	t.Parallel()
	var s NodeSet
	if !s.IsEmpty() || s.Cardinality() != 0 || s.tag() != stateEmpty {
		t.Fatalf("zero NodeSet not empty: empty=%v card=%d tag=%d", s.IsEmpty(), s.Cardinality(), s.tag())
	}
}

// TestNodeSet_SingletonIDZeroDistinctFromEmpty guards the empty/singleton-id-0
// disambiguation: a singleton holding id 0 must not collapse to the empty
// state (the tag, not meta==0, decides).
func TestNodeSet_SingletonIDZeroDistinctFromEmpty(t *testing.T) {
	t.Parallel()
	var s NodeSet
	s.Add(0)
	if s.IsEmpty() {
		t.Fatal("singleton holding id 0 reported empty")
	}
	if s.tag() != stateSingleton {
		t.Fatalf("tag = %d, want stateSingleton", s.tag())
	}
	if !s.Contains(0) || s.Cardinality() != 1 || s.Minimum() != 0 {
		t.Fatalf("singleton(0) wrong: contains=%v card=%d min=%d", s.Contains(0), s.Cardinality(), s.Minimum())
	}
}

// TestNodeSet_SingletonHighIDRange exercises the 62-bit singleton id cap: an id
// at the cap stays inline (singleton), while an id above it is held in a
// 1-element backing array rather than being truncated.
func TestNodeSet_SingletonHighIDRange(t *testing.T) {
	t.Parallel()
	var atCap NodeSet
	atCap.Add(maxSingletonID)
	if atCap.tag() != stateSingleton || !atCap.Contains(maxSingletonID) || atCap.Minimum() != maxSingletonID {
		t.Fatalf("id at cap not an exact singleton: tag=%d min=%d", atCap.tag(), atCap.Minimum())
	}

	var overCap NodeSet
	big := maxSingletonID + 1
	overCap.Add(big)
	if overCap.tag() != stateSmall {
		t.Fatalf("over-cap id tag = %d, want stateSmall", overCap.tag())
	}
	if !overCap.Contains(big) || overCap.Cardinality() != 1 || overCap.Minimum() != big {
		t.Fatalf("over-cap id mis-stored: contains=%v card=%d min=%d", overCap.Contains(big), overCap.Cardinality(), overCap.Minimum())
	}
}

// TestNodeSet_COWBackingIndependence verifies the copy-on-write discipline: a
// by-value copy of a small-state NodeSet must NOT observe a mutation applied to
// the original (and vice versa). A shared, mutated backing would be an
// aliasing/ACID-Isolation defect.
func TestNodeSet_COWBackingIndependence(t *testing.T) {
	t.Parallel()
	var orig NodeSet
	orig.Add(10)
	orig.Add(20)
	orig.Add(30) // small state, backing {10,20,30}

	cp := orig // by-value copy: same backing pointer transiently shared

	// Mutate the copy; the original must be unchanged.
	cp.Add(15)
	if orig.Contains(15) {
		t.Fatal("mutation of copy leaked into original (backing not copy-on-write)")
	}
	if !cp.Contains(15) {
		t.Fatal("copy did not record its own insert")
	}
	// Original still exactly {10,20,30}.
	if got := orig.ToArray(); len(got) != 3 || got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Fatalf("original mutated by copy's insert: %v", got)
	}
	// Mutate the original; the copy must be unchanged.
	orig.Remove(20)
	if !cp.Contains(20) {
		t.Fatal("removal from original leaked into copy")
	}
}
