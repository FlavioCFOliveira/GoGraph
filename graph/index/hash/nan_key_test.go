package hash

// nan_key_test.go — regression gate for task #1408: NaN float64 keys
// in hash.Index must not accumulate as unreachable map entries.
//
// A Go map's == operator follows IEEE 754: NaN != NaN always, so any
// NaN key inserted into a plain map[float64]* creates an entry that no
// subsequent Lookup or Delete can ever reach. Repeated NaN inserts would
// grow the map without bound. The fix: Insert, Delete and Lookup are
// no-ops when the key is a NaN (float64 or float32).
//
// Layer: short.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// nanBitPatterns returns every canonical IEEE 754 NaN bit pattern
// variant used in the property test: quiet and signalling NaN for
// both signs, plus a handful of payload-varying quiet NaNs.
func nanBitPatterns() []float64 {
	pats := []uint64{
		0x7FF8000000000000,           // canonical quiet NaN (+)
		0xFFF8000000000000,           // canonical quiet NaN (-)
		0x7FF0000000000001,           // signalling NaN (+)
		0xFFF0000000000001,           // signalling NaN (-)
		0x7FFFFFFFFFFFFFFF,           // all-ones payload NaN
		0x7FF8000DEADBEEF0,           // arbitrary quiet NaN
		math.Float64bits(math.NaN()), // runtime math.NaN()
	}
	out := make([]float64, len(pats))
	for i, p := range pats {
		out[i] = math.Float64frombits(p)
	}
	return out
}

// TestIndex_NaN_NoAccumulation is the primary gate: repeated NaN inserts
// must not grow the index (task #1408).
func TestIndex_NaN_NoAccumulation(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	const nodeID = graph.NodeID(1)

	nans := nanBitPatterns()
	for _, nan := range nans {
		for range 100 {
			idx.Insert(nan, nodeID)
		}
	}

	// All shards must be empty: no NaN entry should exist in any shard.
	total := idx.DistinctValues()
	if total != 0 {
		t.Errorf("NaN inserts created %d distinct values; want 0 (no accumulation)", total)
	}
}

// TestIndex_NaN_LookupEmpty confirms Lookup(NaN) returns an empty bitmap.
func TestIndex_NaN_LookupEmpty(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	idx.Insert(1.0, graph.NodeID(10))
	idx.Insert(math.NaN(), graph.NodeID(42))

	bm := idx.Lookup(math.NaN())
	if bm.GetCardinality() != 0 {
		t.Errorf("Lookup(NaN) cardinality = %d; want 0", bm.GetCardinality())
	}
	// The finite insert must still be reachable.
	if idx.Lookup(1.0).GetCardinality() != 1 {
		t.Error("Lookup(1.0) returned empty after NaN insert")
	}
}

// TestIndex_NaN_DeleteNoOp confirms Delete(NaN) is a no-op (no panic).
func TestIndex_NaN_DeleteNoOp(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	idx.Insert(2.0, graph.NodeID(5))
	// Delete NaN should not panic or corrupt finite keys.
	idx.Delete(math.NaN(), graph.NodeID(5))
	if idx.Lookup(2.0).GetCardinality() != 1 {
		t.Error("finite key corrupted after Delete(NaN)")
	}
}

// TestIndex_NaN_FiniteKeyUnaffected verifies that all non-NaN operations
// remain correct after interleaved NaN operations.
func TestIndex_NaN_FiniteKeyUnaffected(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	nodes := []graph.NodeID{1, 2, 3}
	for _, id := range nodes {
		idx.Insert(3.14, id)
		idx.Insert(math.NaN(), id) // interleaved NaN insert
	}

	bm := idx.Lookup(3.14)
	if bm.GetCardinality() != uint64(len(nodes)) {
		t.Errorf("Lookup(3.14) cardinality = %d; want %d", bm.GetCardinality(), len(nodes))
	}
	if idx.DistinctValues() != 1 {
		t.Errorf("DistinctValues = %d; want 1 (only 3.14)", idx.DistinctValues())
	}
}

// TestIndex_NaN_BitPatternVariants tests all NaN bit pattern variants
// from nanBitPatterns.
func TestIndex_NaN_BitPatternVariants(t *testing.T) {
	t.Parallel()
	nans := nanBitPatterns()
	for _, nan := range nans {
		nan := nan
		name := "nan_" + uintToHex(math.Float64bits(nan))
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			idx := New[float64]()
			for range 50 {
				idx.Insert(nan, graph.NodeID(99))
			}
			if idx.DistinctValues() != 0 {
				t.Errorf("NaN bit pattern %x accumulated %d entries", math.Float64bits(nan), idx.DistinctValues())
			}
			if idx.Lookup(nan).GetCardinality() != 0 {
				t.Errorf("Lookup(NaN %x) returned non-empty bitmap", math.Float64bits(nan))
			}
		})
	}
}

// TestIndex_NaN_Float32 tests float32 NaN variants in Index[float32].
func TestIndex_NaN_Float32(t *testing.T) {
	t.Parallel()
	idx := New[float32]()
	f32NaNs := []float32{
		math.Float32frombits(0x7FC00000), // canonical quiet NaN
		math.Float32frombits(0xFFC00000), // negative quiet NaN
		math.Float32frombits(0x7F800001), // signalling NaN
	}
	for _, nan := range f32NaNs {
		for range 100 {
			idx.Insert(nan, graph.NodeID(1))
		}
	}
	if idx.DistinctValues() != 0 {
		t.Errorf("float32 NaN accumulated %d entries; want 0", idx.DistinctValues())
	}
}

// uintToHex converts a uint64 to a hex string for test naming.
func uintToHex(v uint64) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = hexDigits[v&0xF]
		v >>= 4
	}
	// Trim leading zeros.
	start := 0
	for start < 15 && b[start] == '0' {
		start++
	}
	return string(b[start:])
}
