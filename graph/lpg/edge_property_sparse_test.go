package lpg

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// edge_property_sparse_test.go — coverage for the dense<->sparse (COO)
// representation switch added in sprint 222 #1641. The sparse representation
// stores only present (slotIndex, value) entries for a column whose fill is
// below the per-kind break-even, so a sparse-within-a-high-degree-node key does
// not pay for the absent value cells a full-length dense backing would allocate.
//
// The highest correctness risk is the COO compaction transform
// (delete-if-present, then decrement every higher index), which is a DIFFERENT
// transform from the dense copy-down splice. These tests force the sparse
// representation and gate that transform against the proven-correct dense one.

// TestSparse_Thresholds pins the representation-policy thresholds against the
// break-even derivation, so a change to the constants is a deliberate, reviewed
// act. breakeven(v) = (v + 1/8)/(v + 4); promote at breakeven, demote a band
// below it (0.10 for the scalar kinds, 0.05 for string), bool always dense.
func TestSparse_Thresholds(t *testing.T) {
	t.Parallel()
	const eps = 1e-9
	cases := []struct {
		kind            PropertyKind
		wantBreakeven   float64
		wantPromote     float64
		wantDemote      float64
		describe        string
		alwaysDenseKind bool
	}{
		{PropInt64, 8.125 / 12, 8.125 / 12, 8.125/12 - 0.10, "int64 v=8", false},
		{PropFloat64, 8.125 / 12, 8.125 / 12, 8.125/12 - 0.10, "float64 v=8", false},
		{dateKind, 4.125 / 8, 4.125 / 8, 4.125/8 - 0.10, "date v=4", false},
		{PropString, 16.125 / 20, 16.125 / 20, 16.125/20 - 0.05, "string v=16", false},
		{PropBool, 0.25 / 4.125, 0, -1, "bool v=0.125 always dense", true},
	}
	for _, tc := range cases {
		if got := breakevenFill(tc.kind); math.Abs(got-tc.wantBreakeven) > eps {
			t.Errorf("%s: breakevenFill = %.6f, want %.6f", tc.describe, got, tc.wantBreakeven)
		}
		if got := promoteThreshold(tc.kind); math.Abs(got-tc.wantPromote) > eps {
			t.Errorf("%s: promoteThreshold = %.6f, want %.6f", tc.describe, got, tc.wantPromote)
		}
		if got := demoteThreshold(tc.kind); math.Abs(got-tc.wantDemote) > eps {
			t.Errorf("%s: demoteThreshold = %.6f, want %.6f", tc.describe, got, tc.wantDemote)
		}
		if tc.alwaysDenseKind {
			// A bool column at any fill in (0,1) must never demote: demote < 0.
			if demoteThreshold(tc.kind) >= 0 {
				t.Errorf("%s: demoteThreshold must be < 0 (never demote), got %.6f",
					tc.describe, demoteThreshold(tc.kind))
			}
		}
	}
	// String break-even (~0.806) and the dominant ~0.50 workload fill: a string
	// column at 0.50 fill must be below demote (so it goes sparse) — this is the
	// example-26 win.
	if 0.50 > demoteThreshold(PropString) {
		t.Fatalf("string demote threshold %.4f must be >= 0.50 so a 50%%-fill string column goes sparse",
			demoteThreshold(PropString))
	}
}

// TestSparse_StringColumnGoesSparseAtHalfFill is the example-26-shaped guard: a
// high-degree string column ~50 % full must adopt the sparse representation, and
// it must drop the (length - P) absent string headers the dense backing would
// otherwise hold. We assert the representation and the backing size, and verify
// every present value still reads back exactly.
func TestSparse_StringColumnGoesSparseAtHalfFill(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	const length = 100
	var block *edgePropCols
	// Present on the even slots only: fill 50/100 = 0.50, deep below the string
	// demote threshold (~0.756), so the column must be sparse.
	want := map[int]string{}
	for slot := 0; slot < length; slot += 2 {
		s := fmt.Sprintf("2020-01-%02d", slot%28+1)
		block = block.set(key, slot, length, StringValue(s))
		want[slot] = s
	}
	col := findCol(t, block, key)
	if !col.sparse {
		t.Fatalf("50%%-fill string column should be sparse, got dense (valid=%v)", col.valid != nil)
	}
	if col.valid != nil {
		t.Fatalf("sparse column must not carry a validity bitmap")
	}
	if len(col.idx) != len(want) {
		t.Fatalf("sparse idx length = %d, want %d present entries", len(col.idx), len(want))
	}
	if len(col.str) != len(want) {
		t.Fatalf("sparse string backing holds %d headers, want only the %d present (no absent-slot headers)",
			len(col.str), len(want))
	}
	// idx must be strictly ascending and in range.
	assertIdxInvariant(t, col)
	// Every present value reads back; every absent slot reads null.
	for slot := 0; slot < length; slot++ {
		v, ok := block.get(key, slot)
		ws, present := want[slot]
		if ok != present {
			t.Fatalf("slot %d: presence got=%v want=%v", slot, ok, present)
		}
		if present {
			if gs, _ := v.String(); gs != ws {
				t.Fatalf("slot %d: value got=%q want=%q", slot, gs, ws)
			}
		}
	}
}

// TestSparse_PromotesWhenFilled asserts a sparse column promotes to dense once
// its fill rises to/over the promote threshold, and demotes back when emptied
// below the demote threshold — i.e. the hysteresis switch fires in both
// directions and the values survive each transition.
func TestSparse_PromotesWhenFilled(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	const length = 20 // int64: promote 0.677 -> 14 present; demote 0.577 -> 11 present
	var block *edgePropCols

	// Fill slots 0..2 (fill 3/20 = 0.15): sparse.
	for slot := 0; slot < 3; slot++ {
		block = block.set(key, slot, length, Int64Value(int64(slot)))
	}
	if col := findCol(t, block, key); !col.sparse {
		t.Fatalf("low-fill column should be sparse")
	}
	// Fill up to slot 17 (18 present, fill 0.90 >= promote): dense.
	for slot := 3; slot < 18; slot++ {
		block = block.set(key, slot, length, Int64Value(int64(slot)))
	}
	col := findCol(t, block, key)
	if col.sparse {
		t.Fatalf("90%%-fill column should have promoted to dense")
	}
	// Verify all 18 values survived the promotion.
	for slot := 0; slot < 18; slot++ {
		v, ok := block.get(key, slot)
		if !ok {
			t.Fatalf("slot %d lost after promotion", slot)
		}
		if i, _ := v.Int64(); i != int64(slot) {
			t.Fatalf("slot %d = %d after promotion", slot, i)
		}
	}
	// Delete back down to 2 present (fill 0.10 <= demote): demotes to sparse.
	for slot := 2; slot < 18; slot++ {
		next, _ := block.del(key, slot)
		block = next
	}
	col = findCol(t, block, key)
	if !col.sparse {
		t.Fatalf("10%%-fill column should have demoted to sparse")
	}
	for slot := 0; slot < 2; slot++ {
		v, ok := block.get(key, slot)
		if !ok {
			t.Fatalf("slot %d lost after demotion", slot)
		}
		if i, _ := v.Int64(); i != int64(slot) {
			t.Fatalf("slot %d = %d after demotion", slot, i)
		}
	}
}

// TestSparse_BoolNeverSparse asserts the bool kind is held dense at every fill,
// because its 4-byte COO index dwarfs the 0.125-byte packed value (break-even
// ~0.06). Even a 1-of-1000 fill bool column must remain bit-packed dense.
func TestSparse_BoolNeverSparse(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	const length = 1000
	var block *edgePropCols
	block = block.set(key, 0, length, BoolValue(true)) // fill 0.001
	col := findCol(t, block, key)
	if col.sparse {
		t.Fatalf("bool column must never be sparse (break-even ~0.06)")
	}
	if col.boolBits == nil {
		t.Fatalf("bool column must keep its bit-packed backing")
	}
	if v, ok := block.get(key, 0); !ok {
		t.Fatalf("bool slot 0 absent")
	} else if b, _ := v.Bool(); !b {
		t.Fatalf("bool slot 0 = false, want true")
	}
}

// TestSparse_CompactionCollisionCase is the targeted regression for the COO
// compaction landmine the design review flagged: dropping the entry at idx0 is
// LOAD-BEARING. With present slots {idx0, idx0+1}, compacting idx0 must drop
// idx0's entry AND decrement idx0+1 to idx0, yielding {idx0} — not the duplicate
// {idx0, idx0} that omitting the drop would produce. We force the sparse
// representation and check the survivor's value and the strict-ascending idx.
func TestSparse_CompactionCollisionCase(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	// length 10, present at two ADJACENT slots 3 and 4 (fill 0.20): sparse int64.
	const length = 10
	var block *edgePropCols
	block = block.set(key, 3, length, Int64Value(33))
	block = block.set(key, 4, length, Int64Value(44))
	col := findCol(t, block, key)
	if !col.sparse {
		t.Fatalf("two-of-ten column should be sparse")
	}
	// Compact slot 3 (present). Drop 3's entry; 4 -> 3. Result: {3 -> 44}.
	out := block.CompactSlot(3).(*edgePropCols)
	col = findCol(t, out, key)
	assertIdxInvariant(t, col)
	if out.length != length-1 {
		t.Fatalf("compacted length = %d, want %d", out.length, length-1)
	}
	if got := col.popcountValid(); got != 1 {
		t.Fatalf("present count after compacting one of two adjacent slots = %d, want 1 (collision drop)", got)
	}
	if v, ok := out.get(key, 3); !ok {
		t.Fatalf("survivor slot 3 (was 4) absent after compaction")
	} else if i, _ := v.Int64(); i != 44 {
		t.Fatalf("survivor slot 3 = %d, want 44", i)
	}
	// The old slot 3 value (33) must be gone, slot 4 now absent (only 1 present).
	for _, slot := range []int{0, 4} {
		if _, ok := out.get(key, slot); ok {
			t.Fatalf("slot %d wrongly present after compaction", slot)
		}
	}
}

// TestSparse_CompactAbsentSlot covers the three index positions of an ABSENT
// compaction target relative to the present entries: below all, between two, and
// above all. In each, the present entries must shift correctly and no entry may
// be dropped (the absent target has no entry to drop).
func TestSparse_CompactAbsentSlot(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	build := func() (*edgePropCols, int) {
		// length 8, present at slots 2, 5 (fill 0.25): sparse int64.
		const length = 8
		var b *edgePropCols
		b = b.set(key, 2, length, Int64Value(200))
		b = b.set(key, 5, length, Int64Value(500))
		return b, length
	}
	cases := []struct {
		name    string
		compact int
		want    map[int]int64 // surviving slot -> value, after the shift
	}{
		{"below-all", 0, map[int]int64{1: 200, 4: 500}}, // 2->1, 5->4
		{"between", 3, map[int]int64{2: 200, 4: 500}},   // 2 stays, 5->4
		{"above-all", 7, map[int]int64{2: 200, 5: 500}}, // both stay
		{"present-low", 2, map[int]int64{4: 500}},       // drop 2, 5->4
		{"present-high", 5, map[int]int64{2: 200}},      // drop 5, 2 stays
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, length := build()
			out := b.CompactSlot(tc.compact).(*edgePropCols)
			col := findCol(t, out, key)
			assertIdxInvariant(t, col)
			if out.length != length-1 {
				t.Fatalf("length = %d, want %d", out.length, length-1)
			}
			// Every slot of the result must match want (present iff in want).
			for slot := 0; slot < out.length; slot++ {
				v, ok := out.get(key, slot)
				wv, present := tc.want[slot]
				if ok != present {
					t.Fatalf("slot %d: presence got=%v want=%v (idx=%v)", slot, ok, present, col.idx)
				}
				if present {
					if i, _ := v.Int64(); i != wv {
						t.Fatalf("slot %d: value got=%d want=%d", slot, i, wv)
					}
				}
			}
		})
	}
}

// TestSparse_DenseDifferentialRoundTrip is the highest-leverage guard from the
// graph-theory review: for a column forced sparse, materialising its dense
// equivalent and re-sparsifying must reproduce the identical (idx, value) set,
// and a dense column toSparse->toDense must reproduce itself. This directly tests
// the Q1 representation equivalence: the two forms are observationally identical.
func TestSparse_DenseDifferentialRoundTrip(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	const length = 40
	rng := rand.New(rand.NewSource(99))
	// Build a sparse int64 column at ~25% fill.
	var block *edgePropCols
	present := map[int]int64{}
	for slot := 0; slot < length; slot++ {
		if rng.Intn(4) != 0 {
			continue
		}
		v := int64(rng.Intn(1000))
		block = block.set(key, slot, length, Int64Value(v))
		present[slot] = v
	}
	col := findCol(t, block, key)
	if !col.sparse {
		t.Fatalf("~25%%-fill column should be sparse")
	}
	// sparse -> dense -> sparse must be a fixed point on the (slot,value) set.
	dense := col.toDense()
	roundtrip := dense.toSparse()
	assertSameReadSemantics(t, col, &dense, length)
	assertSameReadSemantics(t, col, &roundtrip, length)
	assertIdxInvariant(t, &roundtrip)
	// And the explicit present map matches all three.
	for _, c := range []*edgePropColumn{col, &dense, &roundtrip} {
		for slot := 0; slot < length; slot++ {
			v, ok := c.slotValue(slot)
			wv, want := present[slot]
			if ok != want {
				t.Fatalf("presence mismatch slot %d: got=%v want=%v", slot, ok, want)
			}
			if want {
				if i, _ := v.Int64(); i != wv {
					t.Fatalf("value mismatch slot %d: got=%d want=%d", slot, i, wv)
				}
			}
		}
	}
}

// TestSparse_PropertyBasedOracle drives a randomized add/remove/re-add sequence
// against the columnar block and an independent oracle on a HIGH-DEGREE node with
// MANY DISTINCT KEYS each set on FEW slots, so the columns are forced sparse and
// the COO transform is exercised. It asserts after every operation that (1) every
// (slot,key) value matches, (2) the per-slot cardinality matches (catches a
// phantom present slot), (3) the raw sparse idx invariant holds, and (4) for any
// sparse column the dense-differential round-trip reproduces it (catches a
// mis-shift that keeps cardinality but moves a value). The compaction targets are
// biased toward present and adjacent-to-present slots to hit the COO collision
// case.
func TestSparse_PropertyBasedOracle(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= 16; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			runSparseOracle(t, seed)
		})
	}
}

func runSparseOracle(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	// Many distinct keys -> each is set on few slots of the wide node -> every
	// column is sparse. A mix of kinds covers the de-boxed sparse backings.
	keys := []PropertyKeyID{1, 2, 3, 4, 5, 6, 7, 8}
	values := func() PropertyValue {
		switch rng.Intn(5) {
		case 0:
			return Int64Value(int64(rng.Intn(7)))
		case 1:
			return Float64Value(float64(rng.Intn(3)))
		case 2:
			return BoolValue(rng.Intn(2) == 1)
		case 3:
			return StringValue(fmt.Sprintf("s%d", rng.Intn(4)))
		default:
			return StringValue(epochDayToString(int32(18000 + rng.Intn(5))))
		}
	}

	var block *edgePropCols
	oracle := &edgePropOracle{}
	const cap = 40 // wide node: keep degree high so per-key fill stays low

	ops := 600
	for step := 0; step < ops; step++ {
		n := oracle.length()
		switch {
		case n == 0 || (n < cap && rng.Intn(2) == 0):
			block = growBlock(block, n)
			oracle.grow()
		case n > 1 && rng.Intn(4) == 0:
			// COMPACT, biased toward a present/adjacent slot to hit the collision
			// transform.
			idx := biasedCompactTarget(rng, block, n)
			block = block.CompactSlot(idx).(*edgePropCols)
			oracle.compact(idx)
		case rng.Intn(2) == 0:
			slot := rng.Intn(n)
			key := keys[rng.Intn(len(keys))]
			v := values()
			block = block.set(key, slot, n, v)
			oracle.set(slot, key, v)
		default:
			slot := rng.Intn(n)
			key := keys[rng.Intn(len(keys))]
			next, _ := block.del(key, slot)
			block = next
			oracle.del(slot, key)
		}
		assertSparseBlockMatchesOracle(t, seed, step, block, oracle, keys)
	}
}

// biasedCompactTarget returns a compaction index biased toward present and
// adjacent-to-present slots of an arbitrary column, so the COO
// delete-if-present and the {idx,idx+1} collision cases are exercised, rather
// than mostly hitting absent slots on a sparse node.
func biasedCompactTarget(rng *rand.Rand, block *edgePropCols, n int) int {
	if block != nil && rng.Intn(2) == 0 {
		for i := range block.cols {
			c := &block.cols[i]
			if c.sparse && len(c.idx) > 0 {
				j := int(c.idx[rng.Intn(len(c.idx))])
				// Half the time target the slot just above a present one (collision).
				if rng.Intn(2) == 0 && j+1 < n {
					return j + 1
				}
				if j < n {
					return j
				}
			}
		}
	}
	return rng.Intn(n)
}

func assertSparseBlockMatchesOracle(t *testing.T, seed int64, step int, block *edgePropCols, oracle *edgePropOracle, keys []PropertyKeyID) {
	t.Helper()
	if block.lenOrZero() != oracle.length() {
		t.Fatalf("seed=%d step=%d: block length %d != oracle %d", seed, step, block.lenOrZero(), oracle.length())
	}
	for slot := 0; slot < oracle.length(); slot++ {
		presentCount := 0
		for _, key := range keys {
			wantV, wantOK := oracleGet(oracle, slot, key)
			gotV, gotOK := block.get(key, slot)
			if gotOK != wantOK {
				t.Fatalf("seed=%d step=%d slot=%d key=%d: presence got=%v want=%v",
					seed, step, slot, key, gotOK, wantOK)
			}
			if wantOK {
				presentCount++
				if !valuesEqual(gotV, wantV) {
					t.Fatalf("seed=%d step=%d slot=%d key=%d: value got=%v want=%v",
						seed, step, slot, key, gotV, wantV)
				}
			}
		}
		blockCount := 0
		block.forEachAt(slot, func(_ PropertyKeyID, _ PropertyValue) { blockCount++ })
		if blockCount != presentCount {
			t.Fatalf("seed=%d step=%d slot=%d: block cardinality %d != oracle %d (phantom key)",
				seed, step, slot, blockCount, presentCount)
		}
	}
	// Structural + dense-differential guards on every column.
	for i := range block.cols {
		c := &block.cols[i]
		if c.sparse {
			assertIdxInvariant(t, c)
			// Dense-differential: re-materialise dense, re-sparsify, must reproduce
			// the same read semantics. Catches a mis-shift that preserves
			// cardinality but moves a value to the wrong slot.
			dense := c.toDense()
			back := dense.toSparse()
			assertSameReadSemantics(t, c, &back, c.length)
		}
	}
}

// assertIdxInvariant asserts a sparse column's idx is strictly ascending, has no
// duplicates, every index is in [0, length), and the value backing is the same
// length as idx.
func assertIdxInvariant(t *testing.T, col *edgePropColumn) {
	t.Helper()
	if !col.sparse {
		return
	}
	if got := sparseBackingLen(col); got != len(col.idx) {
		t.Fatalf("sparse value backing len %d != idx len %d", got, len(col.idx))
	}
	prev := int32(-1)
	for _, ix := range col.idx {
		if ix <= prev {
			t.Fatalf("idx not strictly ascending: %v", col.idx)
		}
		if int(ix) >= col.length {
			t.Fatalf("idx %d out of range [0,%d): %v", ix, col.length, col.idx)
		}
		prev = ix
	}
}

// sparseBackingLen returns the length of the in-use sparse value backing slice.
func sparseBackingLen(col *edgePropColumn) int {
	switch col.kind {
	case PropInt64:
		return len(col.i64)
	case PropFloat64:
		return len(col.f64)
	case dateKind:
		return len(col.days)
	case PropString:
		return len(col.str)
	default:
		return len(col.boxed)
	}
}

// assertSameReadSemantics asserts two columns read identically over every slot in
// [0, length): same presence and, where present, equal value.
func assertSameReadSemantics(t *testing.T, a, b *edgePropColumn, length int) {
	t.Helper()
	for slot := 0; slot < length; slot++ {
		av, aok := a.slotValue(slot)
		bv, bok := b.slotValue(slot)
		if aok != bok {
			t.Fatalf("slot %d: presence a=%v b=%v", slot, aok, bok)
		}
		if aok && !valuesEqual(av, bv) {
			t.Fatalf("slot %d: value a=%v b=%v", slot, av, bv)
		}
	}
}
