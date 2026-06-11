package btree_test

// TestBTree_Float covers float64 edge cases for the BTree index:
//
//   - IEEE 754 special values: -Inf, +Inf, ±0.0, denormal extremes.
//   - Range queries with -Inf and +Inf as inclusive bounds.
//   - NaN insertion and Range/Lookup behaviour (observed, not assumed).
//
// All assertions are empirically grounded; see inline comments for
// the reasoning behind each expected value.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// corpus is the canonical set of float64 values used across all sub-tests.
// NodeID assignments are fixed to make cross-subtest assertions readable.
//
//	nodeID 0  → math.Inf(-1)
//	nodeID 1  → math.Inf(+1)
//	nodeID 2  → math.Copysign(0, -1)   (negative zero)
//	nodeID 3  → 0.0                    (positive zero)
//	nodeID 4  → math.SmallestNonzeroFloat64
//	nodeID 5  → math.MaxFloat64
//	nodeID 6  → -1.0
//	nodeID 7  → 1.0
//	nodeID 8  → 42.0
//
// Key observation: in Go, math.Copysign(0,-1) == 0.0 is true — negative
// and positive zero are identical under ==, <, and >.  The BTree index
// uses == for deduplication and < for ordering, so both zeros map to the
// same entry; the entry bitmap holds both nodeID 2 and nodeID 3.
// Consequently DistinctValues() == 8 (not 9), while the full-range bitmap
// cardinality is still 9 (both nodeIDs are present in the merged entry).
type corpusEntry struct {
	value  float64
	nodeID graph.NodeID
}

var baseCorpus = []corpusEntry{
	{math.Inf(-1), 0},
	{math.Inf(+1), 1},
	{math.Copysign(0, -1), 2}, // negative zero — shares BTree entry with nodeID 3
	{0.0, 3},                  // positive zero — shares BTree entry with nodeID 2
	{math.SmallestNonzeroFloat64, 4},
	{math.MaxFloat64, 5},
	{-1.0, 6},
	{1.0, 7},
	{42.0, 8},
}

// buildBaseIndex constructs a fresh Index[float64] from baseCorpus.
func buildBaseIndex(t *testing.T) *btree.Index[float64] {
	t.Helper()
	idx := btree.New[float64]()
	for _, e := range baseCorpus {
		idx.Insert(e.value, e.nodeID)
	}
	return idx
}

// TestBTree_Float_PosNegZeroDedup verifies that ±0.0 collapse to a single
// BTree entry because Go's == treats them as equal.
func TestBTree_Float_PosNegZeroDedup(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	// 9 values, but ±0.0 deduplicate → 8 distinct entries.
	if got := idx.DistinctValues(); got != 8 {
		t.Fatalf("DistinctValues() = %d, want 8 (±0.0 must deduplicate)", got)
	}

	// The merged zero entry must contain both nodeID 2 and nodeID 3.
	bm := idx.Lookup(0.0)
	if bm.GetCardinality() != 2 {
		t.Fatalf("Lookup(0.0) cardinality = %d, want 2 (nodeIDs 2 and 3)", bm.GetCardinality())
	}
	if !bm.Contains(2) {
		t.Error("Lookup(0.0): missing nodeID 2 (negative zero)")
	}
	if !bm.Contains(3) {
		t.Error("Lookup(0.0): missing nodeID 3 (positive zero)")
	}

	// Lookup with negative-zero key must resolve the same entry.
	bm2 := idx.Lookup(math.Copysign(0, -1))
	if bm2.GetCardinality() != 2 {
		t.Fatalf("Lookup(negZero) cardinality = %d, want 2", bm2.GetCardinality())
	}
}

// TestBTree_Float_FullRange verifies that Range(math.Inf(-1), math.Inf(+1))
// returns all 9 nodeIDs from the base corpus.
func TestBTree_Float_FullRange(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	bm := idx.Range(math.Inf(-1), math.Inf(+1))

	// All 9 nodeIDs (0..8) must be present.  The ±0.0 merged entry
	// contributes both nodeID 2 and nodeID 3, so cardinality is 9.
	const wantCard = 9
	if got := bm.GetCardinality(); got != wantCard {
		t.Fatalf("Range(-Inf,+Inf) cardinality = %d, want %d", got, wantCard)
	}
	for id := uint64(0); id < 9; id++ {
		if !bm.Contains(id) {
			t.Errorf("Range(-Inf,+Inf): missing nodeID %d", id)
		}
	}
}

// TestBTree_Float_SubRange verifies that Range(-1.0, 1.0) includes exactly
// the values in [-1.0, 1.0] and excludes values outside that window.
//
// Expected inclusions (by nodeID):
//   - 6 (-1.0)
//   - 2 and 3 (±0.0, merged entry)
//   - 4 (SmallestNonzeroFloat64 — positive, < 1.0)
//   - 7 (1.0)
//
// Expected exclusions:
//   - 0 (-Inf), 1 (+Inf), 5 (MaxFloat64), 8 (42.0)
func TestBTree_Float_SubRange(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	bm := idx.Range(-1.0, 1.0)

	const wantCard = 5 // nodeIDs: 6, 2, 3, 4, 7
	if got := bm.GetCardinality(); got != wantCard {
		t.Fatalf("Range(-1.0, 1.0) cardinality = %d, want %d", got, wantCard)
	}

	included := []uint64{6, 2, 3, 4, 7}
	for _, id := range included {
		if !bm.Contains(id) {
			t.Errorf("Range(-1.0, 1.0): missing nodeID %d (expected in range)", id)
		}
	}

	excluded := []uint64{0, 1, 5, 8}
	for _, id := range excluded {
		if bm.Contains(id) {
			t.Errorf("Range(-1.0, 1.0): unexpected nodeID %d (expected outside range)", id)
		}
	}
}

// TestBTree_Float_InfLookup verifies point lookups for ±Inf.
func TestBTree_Float_InfLookup(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	t.Run("negative-inf", func(t *testing.T) {
		t.Parallel()
		bm := idx.Lookup(math.Inf(-1))
		if bm.GetCardinality() != 1 {
			t.Fatalf("Lookup(-Inf) cardinality = %d, want 1", bm.GetCardinality())
		}
		if !bm.Contains(0) {
			t.Fatal("Lookup(-Inf): missing nodeID 0")
		}
	})

	t.Run("positive-inf", func(t *testing.T) {
		t.Parallel()
		bm := idx.Lookup(math.Inf(+1))
		if bm.GetCardinality() != 1 {
			t.Fatalf("Lookup(+Inf) cardinality = %d, want 1", bm.GetCardinality())
		}
		if !bm.Contains(1) {
			t.Fatal("Lookup(+Inf): missing nodeID 1")
		}
	})
}

// TestBTree_Float_NaNBehaviour verifies the NaN key contract of the
// BTree index (task #1354). Keys are ordered by the cmp.Compare total
// order, under which a NaN is a first-class key:
//
//  1. Insert(NaN, 99) stores a regular entry that sorts before every
//     other value, including math.Inf(-1). The sorted invariant the
//     binary searches depend on holds, so the finite and infinite
//     keys inserted before and after remain fully queryable.
//
//  2. Lookup(NaN) addresses that entry: every NaN bit pattern
//     compares equal to every other NaN under cmp.Compare, so the key
//     is retrievable, and repeated NaN inserts deduplicate into the
//     single entry instead of growing without bound.
//
//  3. Range(math.Inf(-1), math.Inf(+1)) does NOT include NaN: the NaN
//     entry sorts below the -Inf lower bound. All 9 base nodeIDs are
//     returned; the NaN-keyed nodes are absent.
func TestBTree_Float_NaNBehaviour(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	// Step 1: insert NaN — a regular key under the total order.
	nan := math.NaN()
	idx.Insert(nan, 99)

	// Base corpus has 8 distinct entries (±0.0 merged); NaN adds one.
	if got := idx.DistinctValues(); got != 9 {
		t.Fatalf("DistinctValues after NaN insert = %d, want 9", got)
	}

	// Step 2: Lookup(NaN) addresses the NaN entry.
	bm := idx.Lookup(nan)
	if bm.GetCardinality() != 1 || !bm.Contains(99) {
		t.Fatalf("Lookup(NaN) = cardinality %d, want exactly nodeID 99", bm.GetCardinality())
	}

	// Repeated NaN inserts (any bit pattern) deduplicate into the entry.
	idx.Insert(math.Float64frombits(0x7FF8000000000001), 100)
	if got := idx.DistinctValues(); got != 9 {
		t.Fatalf("DistinctValues after second NaN insert = %d, want 9 (dedup)", got)
	}
	if got := idx.Cardinality(nan); got != 2 {
		t.Fatalf("Cardinality(NaN) = %d, want 2", got)
	}

	// Step 3: Range(-Inf, +Inf) returns all 9 base nodeIDs and no
	// NaN-keyed node — NaN sorts below the -Inf lower bound.
	full := idx.Range(math.Inf(-1), math.Inf(+1))
	for id := uint64(0); id < 9; id++ {
		if !full.Contains(id) {
			t.Errorf("Range(-Inf,+Inf) after NaN insert: missing nodeID %d", id)
		}
	}
	if full.Contains(99) || full.Contains(100) {
		t.Error("Range(-Inf,+Inf): NaN-keyed nodes must be absent (NaN < -Inf in the total order)")
	}
}

// TestBTree_Float_DenormalOrdering verifies that math.SmallestNonzeroFloat64
// (the smallest positive denormal) is ordered correctly between 0.0 and 1.0.
func TestBTree_Float_DenormalOrdering(t *testing.T) {
	t.Parallel()

	idx := buildBaseIndex(t)

	denormal := math.SmallestNonzeroFloat64

	// Exclusive lower bound: Range just above 0.0 must include denormal.
	// We use a range [SmallestNonzeroFloat64, SmallestNonzeroFloat64] — point lookup via Range.
	bm := idx.Range(denormal, denormal)
	if bm.GetCardinality() != 1 {
		t.Fatalf("Range(SmallestNonzero, SmallestNonzero) cardinality = %d, want 1", bm.GetCardinality())
	}
	if !bm.Contains(4) {
		t.Fatal("Range(SmallestNonzero, SmallestNonzero): missing nodeID 4")
	}

	// Denormal must NOT appear in a range that ends at -epsilon (below zero).
	bmNeg := idx.Range(-math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64)
	if bmNeg.Contains(4) {
		t.Fatal("Range(-SmallestNonzero, -SmallestNonzero): nodeID 4 must be absent")
	}
}
