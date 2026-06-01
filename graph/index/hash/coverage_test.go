package hash

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// TestIndex_NotFound covers the "value not in index" branches of
// Lookup, Cardinality, and Contains.
func TestIndex_NotFound(t *testing.T) {
	t.Parallel()
	idx := New[string]()

	// Lookup of unknown value → empty bitmap.
	bm := idx.Lookup("ghost")
	if bm.GetCardinality() != 0 {
		t.Errorf("Lookup(ghost) cardinality = %d, want 0", bm.GetCardinality())
	}

	// Cardinality of unknown value → 0.
	if c := idx.Cardinality("ghost"); c != 0 {
		t.Errorf("Cardinality(ghost) = %d, want 0", c)
	}

	// Contains with unknown value → false.
	if idx.Contains("ghost", graph.NodeID(1)) {
		t.Error("Contains(ghost) should be false for missing key")
	}
}

// TestIndex_Apply covers the no-op Apply method.
func TestIndex_Apply(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	idx.Apply(index.Change{}) // must not panic
}

// TestIndex_SerializeEmpty covers the serialize/deserialize path for
// a completely empty index (tests zero-value handling in Serialize).
func TestIndex_SerializeEmpty(t *testing.T) {
	t.Parallel()
	src := New[string]()
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize empty: %v", err)
	}
	dst := New[string]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize empty: %v", err)
	}
	if dst.DistinctValues() != 0 {
		t.Errorf("DistinctValues after empty round-trip = %d, want 0", dst.DistinctValues())
	}
}
