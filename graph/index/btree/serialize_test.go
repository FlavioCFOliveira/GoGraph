package btree

import (
	"bytes"
	"cmp"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// TestIndexes_BtreeSerializeRoundtrip seeds an Index[string] with a
// known population, round-trips it, and asserts both per-value
// cardinality and a range scan match the original. The range scan
// also asserts that the deserialised form preserves ascending key
// order — a contract the bulk-load path depends on.
func TestIndexes_BtreeSerializeRoundtrip(t *testing.T) {
	t.Parallel()
	src := New[string]()
	keys := []string{"apple", "banana", "cherry", "date", "elderberry"}
	for ki, k := range keys {
		for n := uint64(0); n < 8; n++ {
			src.Insert(k, graph.NodeID(uint64(ki*100)+n))
		}
	}

	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	dst := New[string]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	if src.DistinctValues() != dst.DistinctValues() {
		t.Fatalf("DistinctValues src=%d dst=%d",
			src.DistinctValues(), dst.DistinctValues())
	}
	for _, k := range keys {
		if a, b := src.Cardinality(k), dst.Cardinality(k); a != b {
			t.Fatalf("Cardinality(%q) src=%d dst=%d", k, a, b)
		}
	}
	// Range from the first to the last key returns every NodeID in
	// ascending key order — exercising the reader's bulk-load path.
	got := dst.Range("apple", "elderberry")
	want := src.Range("apple", "elderberry")
	if got.GetCardinality() != want.GetCardinality() {
		t.Fatalf("Range cardinality src=%d dst=%d",
			want.GetCardinality(), got.GetCardinality())
	}
	// RangeFirst must return the smallest key bucket's first NodeID;
	// confirms the keys were loaded in ascending order on the reader
	// side. The reader rejects out-of-order keys, so a successful
	// Deserialize already proves order; we still test the public
	// RangeFirst path end-to-end.
	v, _, ok := dst.RangeFirst("apple", "elderberry")
	if !ok || v != "apple" {
		t.Fatalf("RangeFirst smallest = (%q, %v), want (\"apple\", true)", v, ok)
	}
}

// TestIndexes_BtreeSerializeEmpty asserts the empty index roundtrips
// cleanly.
func TestIndexes_BtreeSerializeEmpty(t *testing.T) {
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
		t.Fatalf("DistinctValues after empty roundtrip = %d, want 0",
			dst.DistinctValues())
	}
}

// TestIndexes_BtreeCorruptedCRC asserts CRC mismatch surfaces as
// [index.ErrIndexCorrupted].
func TestIndexes_BtreeCorruptedCRC(t *testing.T) {
	t.Parallel()
	src := New[string]()
	src.Insert("k", graph.NodeID(1))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	corrupt := buf.Bytes()
	corrupt[len(corrupt)-1] ^= 0xFF
	dst := New[string]()
	err := dst.Deserialize(bytes.NewReader(corrupt))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("CRC tamper = %v, want ErrIndexCorrupted", err)
	}
}

// TestIndexes_BtreeKindAndApply locks the subscriber identifier and
// confirms Apply is a documented no-op for the generic index.
func TestIndexes_BtreeKindAndApply(t *testing.T) {
	t.Parallel()
	if got := New[string]().Kind(); got != "btree" {
		t.Fatalf("Kind = %q, want \"btree\"", got)
	}
	idx := New[string]()
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, NewValue: "x"})
	if idx.DistinctValues() != 0 {
		t.Fatalf("Apply must be a no-op for the generic btree index; got %d distinct values",
			idx.DistinctValues())
	}
}

// TestIndexes_BtreeInt64Roundtrip exercises encodeOrdered for a
// non-string V.
func TestIndexes_BtreeInt64Roundtrip(t *testing.T) {
	t.Parallel()
	src := New[int64]()
	values := []int64{-100, -1, 0, 1, 42, 100}
	for _, v := range values {
		src.Insert(v, graph.NodeID(uint64(v+1000)))
	}
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize int64: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize int64: %v", err)
	}
	// Full-range scan must contain every original key.
	bm := dst.Range(-100, 100)
	if bm.GetCardinality() != uint64(len(values)) {
		t.Fatalf("Range cardinality = %d, want %d", bm.GetCardinality(), len(values))
	}
}

// TestIndexes_BtreeUnsupportedV asserts an unsupported V surfaces
// [index.ErrIndexValueTypeUnsupported].
func TestIndexes_BtreeUnsupportedV(t *testing.T) {
	t.Parallel()
	// uintptr is cmp.Ordered but not in the supported encode set;
	// confirm the writer rejects it. (Keeps the API honest about
	// which value types persist cleanly.)
	src := New[uintptr]()
	src.Insert(uintptr(42), graph.NodeID(1))
	var buf bytes.Buffer
	err := src.Serialize(&buf)
	if !errors.Is(err, index.ErrIndexValueTypeUnsupported) {
		t.Fatalf("Serialize uintptr = %v, want ErrIndexValueTypeUnsupported", err)
	}
}

// btreeRoundtrip exercises a single (V, value) pair through the
// Serialize/Deserialize pair and asserts membership. Generic over V
// so each kind-specific subtest stays a one-liner.
func btreeRoundtrip[V cmp.Ordered](t *testing.T, v V) {
	t.Helper()
	src := New[V]()
	src.Insert(v, graph.NodeID(1))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[V]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !dst.Lookup(v).Contains(1) {
		t.Fatalf("roundtrip lost entry for value %v", v)
	}
}

// TestIndexes_BtreeAllSupportedTypesRoundtrip exercises every
// supported V kind end-to-end so the type switch in encodeOrdered /
// decodeOrdered stays covered.
func TestIndexes_BtreeAllSupportedTypesRoundtrip(t *testing.T) {
	t.Parallel()
	t.Run("int", func(t *testing.T) { btreeRoundtrip[int](t, 42) })
	t.Run("int32", func(t *testing.T) { btreeRoundtrip[int32](t, 42) })
	t.Run("uint", func(t *testing.T) { btreeRoundtrip[uint](t, 42) })
	t.Run("uint32", func(t *testing.T) { btreeRoundtrip[uint32](t, 42) })
	t.Run("uint64", func(t *testing.T) { btreeRoundtrip[uint64](t, 42) })
	t.Run("float64", func(t *testing.T) { btreeRoundtrip[float64](t, 3.14) })
}
