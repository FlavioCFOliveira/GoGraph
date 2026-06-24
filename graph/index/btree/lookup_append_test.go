package btree

// lookup_append_test.go — LookupAppend (#1722): equality-seek parity with
// Lookup and the allocation-light (zero-alloc for singleton/small) contract.

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestIndex_LookupAppend asserts LookupAppend returns the same id set as Lookup
// for singleton, multi-node, and unknown keys, and that it appends to (never
// overwrites) the caller's buffer.
func TestIndex_LookupAppend(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(5, graph.NodeID(55))
	idx.Insert(5, graph.NodeID(50))
	idx.Insert(7, graph.NodeID(70))

	for _, v := range []int{5, 7, 99 /* unknown */} {
		want := idx.Lookup(v).ToArray() // sorted ascending
		got := idx.LookupAppend(v, nil)
		slices.Sort(got) // AppendTo preserves inline insertion order; sort to compare
		if !slices.Equal(got, want) {
			t.Fatalf("LookupAppend(%d) = %v, want %v", v, got, want)
		}
	}

	// dst is appended to, not overwritten: a non-empty prefix survives.
	dst := []uint64{1, 2}
	dst = idx.LookupAppend(7, dst)
	if !slices.Equal(dst, []uint64{1, 2, 70}) {
		t.Fatalf("LookupAppend did not append to dst: %v", dst)
	}
}

// TestIndex_LookupAppend_NaN verifies the btree's total-order NaN contract:
// unlike the hash index, the btree indexes NaN, so LookupAppend(NaN) must
// return the NaN entry exactly as Lookup does.
func TestIndex_LookupAppend_NaN(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	idx := New[float64]()
	idx.Insert(nan, graph.NodeID(7))
	idx.Insert(1.0, graph.NodeID(1))

	want := idx.Lookup(nan).ToArray()
	got := idx.LookupAppend(nan, nil)
	slices.Sort(got)
	if !slices.Equal(got, want) || len(got) != 1 || got[0] != 7 {
		t.Fatalf("LookupAppend(NaN) = %v, want [7]", got)
	}
}

// BenchmarkIndex_LookupAppendHot mirrors BenchmarkIndex_LookupHot but drives the
// allocation-light path with a reused seek buffer: a singleton equality seek
// appends from the set's inline fields, so it allocates nothing where Lookup
// clones a roaring bitmap (the LookupHot 10-allocs/op shape).
func BenchmarkIndex_LookupAppendHot(b *testing.B) {
	const n = 1_000_000
	idx := buildSeq(b, n)
	r := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // deterministic bench RNG
	dst := make([]uint64, 0, 8)      // reused seek buffer
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.LookupAppend(int64(r.IntN(n)), dst[:0])
	}
	_ = dst
}
