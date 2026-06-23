package index

import (
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// nodeset_test.go — behavioural parity tests for the NodeSet small-set tier
// (sprint 206, #1584/#1585). Every NodeSet operation must produce results
// identical to a plain *roaring64.Bitmap holding the same logical set, across
// all three states (singleton, small, bitmap) and every cardinality.

// reference is an oracle: a plain roaring bitmap mirroring the same mutations,
// against which the NodeSet's membership, cardinality, and sorted iteration
// are checked after each step.
func assertParity(t *testing.T, s *NodeSet, ref *roaring64.Bitmap) {
	t.Helper()
	if got, want := s.Cardinality(), ref.GetCardinality(); got != want {
		t.Fatalf("Cardinality = %d, want %d", got, want)
	}
	if got, want := s.IsEmpty(), ref.IsEmpty(); got != want {
		t.Fatalf("IsEmpty = %v, want %v", got, want)
	}
	want := ref.ToArray()
	got := s.ToArray()
	if len(got) != len(want) {
		t.Fatalf("ToArray len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ToArray[%d] = %d, want %d (full got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
	// Sorted-ascending invariant.
	if !sort.SliceIsSorted(got, func(a, b int) bool { return got[a] < got[b] }) {
		t.Fatalf("ToArray not ascending: %v", got)
	}
	// Membership parity on every present id plus a couple of absentees.
	for _, id := range want {
		if !s.Contains(id) {
			t.Fatalf("Contains(%d) = false, want true", id)
		}
	}
	if !ref.IsEmpty() {
		max := want[len(want)-1]
		if s.Contains(max+1) != ref.Contains(max+1) {
			t.Fatalf("Contains(%d) parity broke", max+1)
		}
	}
	// OrInto parity: folding the set into a fresh bitmap equals ref.
	into := roaring64.New()
	s.OrInto(into)
	if !into.Equals(ref) {
		t.Fatalf("OrInto mismatch: got %v want %v", into.ToArray(), ref.ToArray())
	}
	// Bitmap() parity.
	bm, _ := s.Bitmap()
	if !bm.Equals(ref) {
		t.Fatalf("Bitmap() mismatch: got %v want %v", bm.ToArray(), ref.ToArray())
	}
}

func TestNodeSet_AddRemoveParity(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(0x1584, 0x1585)) //nolint:gosec // deterministic test RNG
	for trial := 0; trial < 200; trial++ {
		var s NodeSet
		ref := roaring64.New()
		// Mix adds and removes over a small id space so churn drives every
		// tier transition (empty<->singleton<->small<->bitmap).
		ops := r.IntN(40)
		for op := 0; op < ops; op++ {
			id := uint64(r.IntN(30))
			if r.IntN(3) == 0 && !ref.IsEmpty() {
				s.Remove(id)
				ref.Remove(id)
			} else {
				s.Add(id)
				ref.Add(id)
			}
			assertParity(t, &s, ref)
		}
	}
}

func TestNodeSet_PromotionThreshold(t *testing.T) {
	t.Parallel()
	// Adding distinct ids up to smallSetMax stays inline; the (smallSetMax+1)th
	// promotes to a bitmap. After promotion, removing down to one node does NOT
	// demote (promote-and-never-demote).
	var s NodeSet
	for i := 0; i < smallSetMax; i++ {
		s.Add(uint64(i))
	}
	if s.tag() == stateBitmap {
		t.Fatalf("set promoted to bitmap at cardinality %d, want inline", s.Cardinality())
	}
	s.Add(uint64(smallSetMax)) // crosses the threshold
	if s.tag() != stateBitmap {
		t.Fatalf("set did not promote to bitmap at cardinality %d", s.Cardinality())
	}
	for i := 0; i < smallSetMax; i++ {
		s.Remove(uint64(i))
	}
	if s.tag() != stateBitmap {
		t.Fatalf("set demoted from bitmap after removals — must never demote")
	}
	if s.Cardinality() != 1 {
		t.Fatalf("cardinality = %d, want 1", s.Cardinality())
	}
}

func TestNodeSet_StateRepresentations(t *testing.T) {
	t.Parallel()
	// Empty: the zero value is ptr==nil, meta==0 (stateEmpty).
	var s NodeSet
	if !s.IsEmpty() || s.tag() != stateEmpty || s.ptr != nil || s.meta != 0 {
		t.Fatalf("zero NodeSet not empty: tag=%d ptr=%v meta=%d", s.tag(), s.ptr, s.meta)
	}
	// Singleton: id packed inline in meta, ptr stays nil.
	s.Add(42)
	if s.tag() != stateSingleton || s.ptr != nil || s.Cardinality() != 1 || s.Minimum() != 42 {
		t.Fatalf("singleton state wrong: tag=%d ptr=%v card=%d min=%d", s.tag(), s.ptr, s.Cardinality(), s.Minimum())
	}
	if got := s.Minimum(); got != 42 {
		t.Fatalf("Minimum = %d, want 42", got)
	}
	// Small: backing array, ptr non-nil, sorted ascending.
	s.Add(7)
	if s.tag() != stateSmall || s.ptr == nil {
		t.Fatalf("small state wrong: tag=%d ptr=%v", s.tag(), s.ptr)
	}
	if ids := s.ToArray(); len(ids) != 2 || ids[0] != 7 || ids[1] != 42 {
		t.Fatalf("small state not sorted: %v", s.ToArray())
	}
	if got := s.Minimum(); got != 7 {
		t.Fatalf("Minimum = %d, want 7", got)
	}
}

func TestNodeSet_AddRangePromotesAndStaysBitmap(t *testing.T) {
	t.Parallel()
	// AddRange must always land on the bitmap tier (the dense fast path) and
	// fold in any pre-existing inline ids as a union.
	var s NodeSet
	s.Add(100)
	s.Add(5)
	s.AddRange(10, 20) // inclusive
	if s.tag() != stateBitmap {
		t.Fatalf("AddRange did not promote to bitmap")
	}
	ref := roaring64.New()
	ref.Add(5)
	ref.Add(100)
	ref.AddRange(10, 21)
	if bm, _ := s.Bitmap(); !bm.Equals(ref) {
		got, _ := s.Bitmap()
		t.Fatalf("AddRange union mismatch: got %v want %v", got.ToArray(), ref.ToArray())
	}
	// A subsequent Add keeps it a bitmap.
	s.Add(7)
	if s.tag() != stateBitmap {
		t.Fatalf("Add after AddRange demoted the set")
	}
}

func TestNodeSet_DenseAddRangeStaysRunContainerTiny(t *testing.T) {
	t.Parallel()
	// A label covering a contiguous band of millions of NodeIDs must stay a
	// run-container-tiny roaring bitmap, not blow up via the inline tier.
	var s NodeSet
	const n = 10_000_000
	s.AddRange(0, n-1)
	if s.tag() != stateBitmap {
		t.Fatalf("dense AddRange not on bitmap tier")
	}
	if got := s.Cardinality(); got != n {
		t.Fatalf("cardinality = %d, want %d", got, n)
	}
	bm, _ := s.Bitmap()
	bm.RunOptimize()
	if sz := bm.GetSerializedSizeInBytes(); sz > 4096 {
		t.Fatalf("dense band serialized size = %d bytes, expected run-container-tiny", sz)
	}
}

func TestNodeSet_FromSorted(t *testing.T) {
	t.Parallel()
	cases := [][]uint64{
		nil,
		{1},
		{1, 2, 3},
		{1, 2, 3, 4, 5, 6, 7, 8},    // == smallSetMax
		{1, 2, 3, 4, 5, 6, 7, 8, 9}, // > smallSetMax -> bitmap
		{10, 20, 30, 40, 50, 60, 70, 80, 90, 100},
	}
	for _, ids := range cases {
		in := append([]uint64(nil), ids...)
		s := NodeSetFromSorted(in)
		ref := roaring64.New()
		ref.AddMany(ids)
		assertParity(t, &s, ref)
		if len(ids) > smallSetMax && s.tag() != stateBitmap {
			t.Fatalf("FromSorted(%d ids) did not promote", len(ids))
		}
		if len(ids) > 0 && len(ids) <= smallSetMax && s.tag() == stateBitmap {
			t.Fatalf("FromSorted(%d ids) over-promoted to bitmap", len(ids))
		}
	}
}

func TestNodeSet_FromBitmap_DownConvert(t *testing.T) {
	t.Parallel()
	// A small bitmap down-converts to the inline tier; a dense one stays.
	small := roaring64.New()
	small.AddMany([]uint64{3, 1, 2})
	s := NodeSetFromBitmap(small)
	if s.tag() == stateBitmap {
		t.Fatalf("small bitmap did not down-convert: tag=%d", s.tag())
	}
	assertParity(t, &s, small)

	dense := roaring64.New()
	dense.AddRange(0, 1000)
	d := NodeSetFromBitmap(dense)
	if d.tag() != stateBitmap {
		t.Fatalf("dense bitmap was down-converted, must stay a bitmap")
	}
	assertParity(t, &d, dense)
}

func TestNodeSet_RemoveRangeParity(t *testing.T) {
	t.Parallel()
	// Inline-tier RemoveRange.
	var s NodeSet
	ref := roaring64.New()
	for _, id := range []uint64{1, 5, 9, 13} {
		s.Add(id)
		ref.Add(id)
	}
	s.RemoveRange(4, 10)
	ref.RemoveRange(4, 11)
	assertParity(t, &s, ref)

	// Bitmap-tier RemoveRange.
	var b NodeSet
	bref := roaring64.New()
	b.AddRange(0, 100)
	bref.AddRange(0, 101)
	b.RemoveRange(40, 60)
	bref.RemoveRange(40, 61)
	assertParity(t, &b, bref)
}
