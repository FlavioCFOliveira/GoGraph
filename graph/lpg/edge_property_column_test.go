package lpg

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// TestEdgePropCols_PerKindRoundTrip exercises set/get for each de-boxed kind on
// a single-slot block, asserting the value reads back with the exact kind and
// payload.
func TestEdgePropCols_PerKindRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  PropertyValue
		eq   func(PropertyValue) bool
	}{
		{"int64", Int64Value(2020), func(v PropertyValue) bool { i, ok := v.Int64(); return ok && i == 2020 }},
		{"int64-zero", Int64Value(0), func(v PropertyValue) bool { i, ok := v.Int64(); return ok && i == 0 }},
		{"float64", Float64Value(0.5), func(v PropertyValue) bool { f, ok := v.Float64(); return ok && f == 0.5 }},
		{"float64-nan", Float64Value(math.NaN()), func(v PropertyValue) bool { f, ok := v.Float64(); return ok && math.IsNaN(f) }},
		{"bool-true", BoolValue(true), func(v PropertyValue) bool { b, ok := v.Bool(); return ok && b }},
		{"bool-false", BoolValue(false), func(v PropertyValue) bool { b, ok := v.Bool(); return ok && !b }},
		{"string", StringValue("hello"), func(v PropertyValue) bool { s, ok := v.String(); return ok && s == "hello" }},
		{"string-empty", StringValue(""), func(v PropertyValue) bool { s, ok := v.String(); return ok && s == "" }},
		{"bytes", BytesValue([]byte{1, 2, 3}), func(v PropertyValue) bool { b, ok := v.Bytes(); return ok && len(b) == 3 && b[2] == 3 }},
		{"list", ListValue([]PropertyValue{Int64Value(1), StringValue("x")}), func(v PropertyValue) bool {
			l, ok := v.List()
			if !ok || len(l) != 2 {
				return false
			}
			i, _ := l[0].Int64()
			s, _ := l[1].String()
			return i == 1 && s == "x"
		}},
		{"date", StringValue("\x012020-01-15"), func(v PropertyValue) bool { s, ok := v.String(); return ok && s == "\x012020-01-15" }},
	}
	key := PropertyKeyID(7)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var block *edgePropCols
			block = block.set(key, 0, 1, tc.val)
			got, ok := block.get(key, 0)
			if !ok {
				t.Fatalf("get after set: not present")
			}
			if !tc.eq(got) {
				t.Fatalf("round-trip mismatch: got kind=%d v=%v", got.Kind(), got)
			}
			// A different key on the same slot must be absent.
			if _, ok := block.get(PropertyKeyID(99), 0); ok {
				t.Fatalf("unrelated key reported present")
			}
		})
	}
}

// TestEdgePropCols_DateFoldsToInt32 asserts that an SOH-tagged Date string is
// folded into the de-boxed int32 epoch-day column (not the boxed/string column)
// and reads back as the identical tagged string.
func TestEdgePropCols_DateFoldsToInt32(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	var block *edgePropCols
	block = block.set(key, 0, 1, StringValue("\x012020-01-15"))

	if len(block.cols) != 1 {
		t.Fatalf("expected exactly 1 column, got %d", len(block.cols))
	}
	col := block.cols[0]
	if col.kind != dateKind {
		t.Fatalf("date value stored under kind %d, want dateKind(%d)", col.kind, dateKind)
	}
	if col.days == nil {
		t.Fatalf("date column has no int32 epoch-day backing")
	}
	if col.i64 != nil || col.str != nil || col.boxed != nil {
		t.Fatalf("date column unexpectedly allocated a non-date backing")
	}
	// 2020-01-15 is 18276 days after the Unix epoch.
	if got := col.days[0]; got != 18276 {
		t.Fatalf("epoch-day = %d, want 18276", got)
	}
	v, ok := block.get(key, 0)
	if !ok {
		t.Fatalf("date not present")
	}
	if s, _ := v.String(); s != "\x012020-01-15" {
		t.Fatalf("date round-trip = %q, want SOH+2020-01-15", s)
	}
}

// TestEdgePropCols_NonDateStringStaysString asserts that a plain string (or a
// tagged non-Date temporal) does NOT fold into the date column.
func TestEdgePropCols_NonDateStringStaysString(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	for _, s := range []string{
		"2020-01-15",         // no SOH tag → plain string
		"\x012020-13-01",     // tagged but invalid month → not a date
		"\x012020-1-1",       // tagged but non-canonical → not a date
		"\x02localdatetime",  // different temporal tag → not a date
		"\x01not-a-date",     // tagged garbage
		"\x0199999999-01-01", // year overflows int32 epoch-day
	} {
		var block *edgePropCols
		block = block.set(key, 0, 1, StringValue(s))
		col := block.cols[0]
		if col.kind == dateKind {
			t.Fatalf("string %q wrongly folded into date column", s)
		}
		if col.kind != PropString {
			t.Fatalf("string %q stored under kind %d, want PropString", s, col.kind)
		}
		v, _ := block.get(key, 0)
		if got, _ := v.String(); got != s {
			t.Fatalf("string %q round-trip = %q", s, got)
		}
	}
}

// TestEdgePropCols_ValidityLifecycle exercises set → get(present) → del →
// get(absent): the IS NULL / IS NOT NULL contract on a single key.
func TestEdgePropCols_ValidityLifecycle(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(3)
	var block *edgePropCols
	block = block.set(key, 0, 1, Int64Value(42))

	if !block.keyPresentAt(key, 0) {
		t.Fatalf("key not present after set")
	}
	if _, ok := block.get(key, 0); !ok {
		t.Fatalf("value not present after set")
	}

	next, changed := block.del(key, 0)
	if !changed {
		t.Fatalf("del reported no change")
	}
	block = next
	if block.keyPresentAt(key, 0) {
		t.Fatalf("key still present after del")
	}
	if _, ok := block.get(key, 0); ok {
		t.Fatalf("value still present after del")
	}
	// Deleting again is a no-op.
	if _, changed := block.del(key, 0); changed {
		t.Fatalf("double del reported a change")
	}
}

// TestEdgePropCols_ZeroAndNaNAreQueryable asserts that 0 (int64) and NaN
// (float64) are NOT confused with absence: they are present values, and only an
// explicit del makes them absent. This is the reason a sentinel cannot replace
// the validity bitmap.
func TestEdgePropCols_ZeroAndNaNAreQueryable(t *testing.T) {
	t.Parallel()
	ki, kf := PropertyKeyID(1), PropertyKeyID(2)
	var block *edgePropCols
	block = block.set(ki, 0, 1, Int64Value(0))
	block = block.set(kf, 0, 1, Float64Value(math.NaN()))

	if vi, ok := block.get(ki, 0); !ok {
		t.Fatalf("int64 zero reads as absent")
	} else if i, _ := vi.Int64(); i != 0 {
		t.Fatalf("int64 zero = %d", i)
	}
	if vf, ok := block.get(kf, 0); !ok {
		t.Fatalf("float64 NaN reads as absent")
	} else if f, _ := vf.Float64(); !math.IsNaN(f) {
		t.Fatalf("float64 NaN = %v", f)
	}
}

// TestEdgePropCols_GrowSlotAbsent asserts the highest-risk invariant: a slot
// created by GrowSlot is ABSENT even when the underlying backing array reuses a
// cell that previously held a value (validity drift). We force a dirty cell by
// setting a value, deleting it, and growing — the new slot must read absent.
func TestEdgePropCols_GrowSlotAbsent(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(5)

	// Start with a 1-slot block carrying a value, then grow to 3 slots.
	var block *edgePropCols
	block = block.set(key, 0, 1, Int64Value(777))
	grown := block.GrowSlot(1).(*edgePropCols) // now length 2; slot 1 absent
	grown = grown.GrowSlot(2).(*edgePropCols)  // now length 3; slots 1,2 absent

	if v, ok := grown.get(key, 0); !ok {
		t.Fatalf("slot 0 lost its value after grow")
	} else if i, _ := v.Int64(); i != 777 {
		t.Fatalf("slot 0 value = %d, want 777", i)
	}
	for _, slot := range []int{1, 2} {
		if _, ok := grown.get(key, slot); ok {
			t.Fatalf("freshly-grown slot %d reads as present (validity drift)", slot)
		}
		if grown.keyPresentAt(key, slot) {
			t.Fatalf("freshly-grown slot %d has its validity bit set", slot)
		}
	}
}

// TestEdgePropCols_CompactPreservesBinding asserts CompactSlot keeps the
// positional binding: removing a middle slot shifts the survivors and keeps
// every survivor's value.
func TestEdgePropCols_CompactPreservesBinding(t *testing.T) {
	t.Parallel()
	key := PropertyKeyID(1)
	// Build a length-4 block with distinct values at slots 0..3.
	var block *edgePropCols
	block = block.set(key, 0, 4, Int64Value(10))
	block = block.set(key, 1, 4, Int64Value(11))
	block = block.set(key, 2, 4, Int64Value(12))
	block = block.set(key, 3, 4, Int64Value(13))

	// Excise slot 1 (value 11). Survivors: [10, 12, 13].
	out := block.CompactSlot(1).(*edgePropCols)
	if out.length != 3 {
		t.Fatalf("compacted length = %d, want 3", out.length)
	}
	want := []int64{10, 12, 13}
	for i, w := range want {
		v, ok := out.get(key, i)
		if !ok {
			t.Fatalf("slot %d absent after compact", i)
		}
		if g, _ := v.Int64(); g != w {
			t.Fatalf("slot %d = %d, want %d", i, g, w)
		}
	}
}

// TestSpliceBits exercises the validity-bitmap compaction at the word-boundary
// values flagged by the design review (n in {1,63,64,65,127,128,129}), against
// an unpacked []bool oracle, and asserts no stale high bit survives.
func TestSpliceBits(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(0xB17B17))
	ns := []int{1, 2, 63, 64, 65, 127, 128, 129, 200}
	for _, n := range ns {
		// Random bit pattern of n bits.
		src := make([]uint64, words(n))
		ref := make([]bool, n)
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 1 {
				src[i>>6] |= 1 << (uint(i) & 63)
				ref[i] = true
			}
		}
		for idx := 0; idx < n; idx++ {
			got := spliceBits(src, idx, n)
			// Oracle: splice the unpacked []bool.
			wantBits := make([]bool, 0, n-1)
			wantBits = append(wantBits, ref[:idx]...)
			wantBits = append(wantBits, ref[idx+1:]...)
			// Compare bit-for-bit.
			for j := 0; j < n-1; j++ {
				gb := j>>6 < len(got) && got[j>>6]&(1<<(uint(j)&63)) != 0
				if gb != wantBits[j] {
					t.Fatalf("n=%d idx=%d bit %d: got %v want %v", n, idx, j, gb, wantBits[j])
				}
			}
			// No stale high bits: every bit at index >= n-1 must be zero.
			for w := 0; w < len(got); w++ {
				lo := w << 6
				for b := 0; b < 64; b++ {
					bitIdx := lo + b
					if bitIdx < n-1 {
						continue
					}
					if got[w]&(1<<(uint(b)&63)) != 0 {
						t.Fatalf("n=%d idx=%d: stale high bit at %d", n, idx, bitIdx)
					}
				}
			}
		}
	}
}

// edgePropOracle is a reference model of one source node's per-slot edge
// properties used by the property-based test: a map from slot index to a map of
// keyID→value. It is deliberately a different representation from the columnar
// block so a shared bug cannot mask a divergence.
type edgePropOracle struct {
	slots []map[PropertyKeyID]PropertyValue
}

func (o *edgePropOracle) grow() {
	o.slots = append(o.slots, nil)
}

func (o *edgePropOracle) compact(idx int) {
	o.slots = append(o.slots[:idx], o.slots[idx+1:]...)
}

func (o *edgePropOracle) set(slot int, key PropertyKeyID, v PropertyValue) {
	if o.slots[slot] == nil {
		o.slots[slot] = make(map[PropertyKeyID]PropertyValue)
	}
	o.slots[slot][key] = v
}

func (o *edgePropOracle) del(slot int, key PropertyKeyID) {
	if o.slots[slot] != nil {
		delete(o.slots[slot], key)
	}
}

// TestEdgePropCols_PropertyBasedOracle drives a randomized add/remove/re-add
// sequence against the columnar block and an independent oracle, asserting after
// every operation that (1) every (slot,key) value matches and (2) the per-slot
// CARDINALITY matches (number of present keys per slot). The cardinality clause
// is load-bearing: it catches a phantom key whose stale value coincidentally
// equals a real one — exactly the validity-drift bug. The generation strategy
// (few keys, grow-to-cap then oscillate, grow-without-set, distinctive
// sentinels) maximizes dirty-cell reuse, per the graph-theory review.
func TestEdgePropCols_PropertyBasedOracle(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= 12; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			runOracle(t, seed)
		})
	}
}

func runOracle(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	keys := []PropertyKeyID{1, 2, 3} // few keys → high reuse pressure
	values := func() PropertyValue {
		switch rng.Intn(5) {
		case 0:
			return Int64Value(int64(rng.Intn(7))) // small range incl 0 → coincidental equality
		case 1:
			return Float64Value(float64(rng.Intn(3)))
		case 2:
			return BoolValue(rng.Intn(2) == 1)
		case 3:
			return StringValue(fmt.Sprintf("s%d", rng.Intn(4)))
		default:
			// Tagged date, small day range.
			return StringValue(epochDayToString(int32(18000 + rng.Intn(5))))
		}
	}

	var block *edgePropCols
	oracle := &edgePropOracle{}
	const cap = 6 // oscillate just under this

	ops := 400
	for step := 0; step < ops; step++ {
		n := oracle.length()
		switch {
		case n == 0 || (n < cap && rng.Intn(3) == 0):
			// GROW (often without a subsequent set → exposes validity drift).
			block = growBlock(block, n)
			oracle.grow()
		case n > 1 && rng.Intn(3) == 0:
			// COMPACT a random slot (front / middle / back all reachable).
			idx := rng.Intn(n)
			block = block.CompactSlot(idx).(*edgePropCols)
			oracle.compact(idx)
		case rng.Intn(2) == 0:
			// SET on a random slot/key.
			slot := rng.Intn(n)
			key := keys[rng.Intn(len(keys))]
			v := values()
			block = block.set(key, slot, n, v)
			oracle.set(slot, key, normalizeForOracle(v))
		default:
			// DEL on a random slot/key.
			slot := rng.Intn(n)
			key := keys[rng.Intn(len(keys))]
			next, _ := block.del(key, slot)
			block = next
			oracle.del(slot, key)
		}
		assertBlockMatchesOracle(t, seed, step, block, oracle, keys)
	}
}

func (o *edgePropOracle) length() int { return len(o.slots) }

// growBlock appends one slot to the block, treating nil as empty. It mirrors the
// adjlist append: GrowSlot when a block exists, otherwise a length-1 empty block.
func growBlock(block *edgePropCols, oldLen int) *edgePropCols {
	if block == nil {
		return &edgePropCols{length: oldLen + 1}
	}
	return block.GrowSlot(oldLen).(*edgePropCols)
}

// normalizeForOracle mirrors the column's date-folding round-trip: a tagged
// Date string round-trips to itself, so the oracle stores the same value. (No
// transformation is needed because the column reconstitutes the identical
// string; this is here for clarity and future-proofing.)
func normalizeForOracle(v PropertyValue) PropertyValue { return v }

func assertBlockMatchesOracle(t *testing.T, seed int64, step int, block *edgePropCols, oracle *edgePropOracle, keys []PropertyKeyID) {
	t.Helper()
	if block.lenOrZero() != oracle.length() {
		t.Fatalf("seed=%d step=%d: block length %d != oracle %d", seed, step, block.lenOrZero(), oracle.length())
	}
	for slot := 0; slot < oracle.length(); slot++ {
		// Value match for every key.
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
		// CARDINALITY clause: the number of present keys the block reports on the
		// slot must equal the oracle's. This is the clause that catches a phantom
		// key whose stale value happens to equal a real one.
		blockCount := 0
		block.forEachAt(slot, func(_ PropertyKeyID, _ PropertyValue) { blockCount++ })
		if blockCount != presentCount {
			t.Fatalf("seed=%d step=%d slot=%d: block cardinality %d != oracle %d (phantom key)",
				seed, step, slot, blockCount, presentCount)
		}
	}
}

func oracleGet(o *edgePropOracle, slot int, key PropertyKeyID) (PropertyValue, bool) {
	if o.slots[slot] == nil {
		return PropertyValue{}, false
	}
	v, ok := o.slots[slot][key]
	return v, ok
}

// valuesEqual compares two PropertyValues for kind + payload equality (the
// "value identity" the durability review demands, not mere presence).
func valuesEqual(a, b PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case PropInt64:
		ai, _ := a.Int64()
		bi, _ := b.Int64()
		return ai == bi
	case PropFloat64:
		af, _ := a.Float64()
		bf, _ := b.Float64()
		return af == bf || (math.IsNaN(af) && math.IsNaN(bf))
	case PropBool:
		ab, _ := a.Bool()
		bb, _ := b.Bool()
		return ab == bb
	case PropString:
		as, _ := a.String()
		bs, _ := b.String()
		return as == bs
	case PropBytes:
		ab, _ := a.Bytes()
		bb, _ := b.Bytes()
		if len(ab) != len(bb) {
			return false
		}
		for i := range ab {
			if ab[i] != bb[i] {
				return false
			}
		}
		return true
	case PropList:
		al, _ := a.List()
		bl, _ := b.List()
		if len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !valuesEqual(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// findCol returns the column carrying key in block, failing the test when no
// such column exists. It is the white-box accessor the popcount test uses to
// reach the storage primitive directly.
func findCol(t *testing.T, block *edgePropCols, key PropertyKeyID) *edgePropColumn {
	t.Helper()
	for i := range block.cols {
		if block.cols[i].key == key {
			return &block.cols[i]
		}
	}
	t.Fatalf("no column for key %d", key)
	return nil
}

// poisonValues overwrites every value cell of the column with a distinctive
// garbage payload WITHOUT touching the presence plane (the validity bitmap in the
// dense form, idx in the sparse form). popcountValid must be blind to this: it
// counts presence from the bitmap / length (dense) or len(idx) (sparse), so a
// poisoned value can never change the count. Any divergence after poisoning
// proves the popcount illegally consulted a value cell. The number of value cells
// is len(idx) in the sparse form and length in the dense form.
func poisonValues(col *edgePropColumn) {
	n := col.length
	if col.sparse {
		n = len(col.idx)
	}
	for i := 0; i < n; i++ {
		switch col.kind {
		case PropInt64:
			col.i64[i] = math.MaxInt64
		case PropFloat64:
			col.f64[i] = math.NaN()
		case PropBool:
			col.boolBits[i>>6] = ^uint64(0)
		case dateKind:
			col.days[i] = math.MaxInt32
		case PropString:
			col.str[i] = "POISON"
		default:
			col.boxed[i] = StringValue("POISON")
		}
	}
}

// TestEdgePropCols_PopcountValid asserts the storage-layer IS NOT NULL primitive
// counts present slots from the validity bitmap alone, across the three states
// #1638 names — dense (validity-omitted), bitmap-present, and post-delete /
// post-compaction — and that it NEVER touches a value cell (proven by poisoning
// every value after the validity state is fixed and re-asserting the count).
func TestEdgePropCols_PopcountValid(t *testing.T) {
	t.Parallel()

	t.Run("dense-omitted-validity", func(t *testing.T) {
		t.Parallel()
		// A length-1 column with its single slot present is the dense case: no
		// validity bitmap is materialised, and popcount returns the length.
		key := PropertyKeyID(1)
		var block *edgePropCols
		block = block.set(key, 0, 1, Int64Value(0)) // value 0 is a real present value
		col := findCol(t, block, key)
		if col.valid != nil {
			t.Fatalf("length-1 present column unexpectedly carries a validity bitmap")
		}
		if got := col.popcountValid(); got != 1 {
			t.Fatalf("dense popcount = %d, want 1", got)
		}
		// Poison the (single) value cell: popcount must stay 1 because the dense
		// branch returns length and never reads i64.
		poisonValues(col)
		if got := col.popcountValid(); got != 1 {
			t.Fatalf("dense popcount after poison = %d, want 1", got)
		}
	})

	t.Run("partial-present-reads-presence-plane", func(t *testing.T) {
		t.Parallel()
		// A multi-slot column with only some slots present: the popcount must
		// equal the present count, read from the presence plane (the validity
		// bitmap when dense, idx when sparse), independent of the values. At fill
		// 3/5 = 0.6 an int64 column (demote 0.577, promote 0.677) stays sparse, so
		// presence is idx; the assertion is representation-agnostic on purpose.
		key := PropertyKeyID(2)
		var block *edgePropCols
		// Length-5 block; set slots 0, 2, 4 only (slots 1, 3 stay absent).
		for _, slot := range []int{0, 2, 4} {
			block = block.set(key, slot, 5, Int64Value(int64(slot)))
		}
		col := findCol(t, block, key)
		// A partial column must carry SOME presence plane: a validity bitmap when
		// dense, or the COO idx when sparse. It must not be a bare fully-dense
		// column (which would read every slot present).
		if !col.sparse && col.valid == nil {
			t.Fatalf("partial dense column must carry a validity bitmap")
		}
		if col.sparse && len(col.idx) != 3 {
			t.Fatalf("partial sparse column idx = %v, want 3 present entries", col.idx)
		}
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount = %d, want 3", got)
		}
		// Poison every value cell: popcount must stay 3 because it reads only the
		// presence plane.
		poisonValues(col)
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount after poison = %d, want 3", got)
		}
	})

	t.Run("post-delete", func(t *testing.T) {
		t.Parallel()
		// Start fully present (5/5), delete two slots, expect popcount 3.
		key := PropertyKeyID(3)
		var block *edgePropCols
		for slot := 0; slot < 5; slot++ {
			block = block.set(key, slot, 5, Int64Value(int64(slot)))
		}
		if got := findCol(t, block, key).popcountValid(); got != 5 {
			t.Fatalf("popcount before delete = %d, want 5", got)
		}
		for _, slot := range []int{1, 3} {
			next, changed := block.del(key, slot)
			if !changed {
				t.Fatalf("del(slot=%d) reported no change", slot)
			}
			block = next
		}
		col := findCol(t, block, key)
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount after delete = %d, want 3", got)
		}
		poisonValues(col)
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount after delete+poison = %d, want 3", got)
		}
	})

	t.Run("post-compaction", func(t *testing.T) {
		t.Parallel()
		// 4 present slots, compact a present slot, expect popcount 3; the validity
		// bitmap must compact under the same index transform as the values.
		key := PropertyKeyID(4)
		var block *edgePropCols
		for slot := 0; slot < 4; slot++ {
			block = block.set(key, slot, 4, Int64Value(int64(10+slot)))
		}
		out := block.CompactSlot(1).(*edgePropCols) // excise a present slot
		col := findCol(t, out, key)
		if col.length != 3 {
			t.Fatalf("compacted column length = %d, want 3", col.length)
		}
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount after compaction = %d, want 3", got)
		}
		poisonValues(col)
		if got := col.popcountValid(); got != 3 {
			t.Fatalf("popcount after compaction+poison = %d, want 3", got)
		}
	})

	t.Run("grow-then-compact-mixed", func(t *testing.T) {
		t.Parallel()
		// Grow introduces an absent trailing slot; popcount must ignore it, then a
		// compaction that removes a present slot leaves the absent-slot count intact.
		key := PropertyKeyID(5)
		var block *edgePropCols
		block = block.set(key, 0, 2, Int64Value(100)) // slot 0 present, slot 1 absent
		block = block.GrowSlot(2).(*edgePropCols)     // length 3, slot 2 absent
		col := findCol(t, block, key)
		if got := col.popcountValid(); got != 1 {
			t.Fatalf("popcount after grow = %d, want 1 (only slot 0 present)", got)
		}
		// Compact the present slot 0; nothing present remains, so the whole column
		// is dropped (dropEmptyColumns is not run by CompactSlot, but the bitmap
		// must report 0 present slots).
		out := block.CompactSlot(0).(*edgePropCols)
		// The column survives compaction (CompactSlot does not prune empties); its
		// popcount must now be 0.
		for i := range out.cols {
			if out.cols[i].key == key {
				if got := out.cols[i].popcountValid(); got != 0 {
					t.Fatalf("popcount after compacting the only present slot = %d, want 0", got)
				}
			}
		}
	})
}

// TestEpochDayCodec round-trips a span of dates through the epoch-day codec and
// cross-checks against a slow reference using the proleptic-Gregorian formula.
func TestEpochDayCodec(t *testing.T) {
	t.Parallel()
	// Known anchors.
	anchors := map[string]int32{
		"1970-01-01": 0,
		"1970-01-02": 1,
		"1969-12-31": -1,
		"2000-01-01": 10957,
		"2020-01-15": 18276,
	}
	for s, want := range anchors {
		ed, ok := stringToEpochDay("\x01" + s)
		if !ok {
			t.Fatalf("stringToEpochDay(%q) failed", s)
		}
		if ed != want {
			t.Fatalf("epoch-day(%s) = %d, want %d", s, ed, want)
		}
		if back := epochDayToString(ed); back != "\x01"+s {
			t.Fatalf("epochDayToString(%d) = %q, want SOH+%s", ed, back, s)
		}
	}
	// Round-trip a dense range of epoch-days.
	for ed := int32(-50000); ed <= 50000; ed += 37 {
		s := epochDayToString(ed)
		back, ok := stringToEpochDay(s)
		if !ok {
			t.Fatalf("round-trip parse failed for epoch-day %d (%q)", ed, s)
		}
		if back != ed {
			t.Fatalf("epoch-day round-trip %d -> %q -> %d", ed, s, back)
		}
	}
}

// --- frame-of-reference (FOR) bit-packed date column -----------------------

// dateVal builds the SOH-tagged canonical-string PropertyValue for an epoch-day,
// the exact shape a Cypher date write delivers to the property layer. The column
// folds it into the int32 epoch-day backing (and, at Compact, the FOR form).
func dateVal(ed int32) PropertyValue { return StringValue(epochDayToString(ed)) }

// buildFullDenseDateColumn returns a block with a single date column of `length`
// fully-present slots, slot i carrying epoch-day days[i]. The block is grown
// slot-by-slot via set, the realistic way a fully-dense date column arises, so
// the resulting column is dense (fill 1.0) and not yet packed.
func buildFullDenseDateColumn(t *testing.T, key PropertyKeyID, days []int32) *edgePropCols {
	t.Helper()
	var block *edgePropCols
	for i, ed := range days {
		block = block.set(key, i, i+1, dateVal(ed))
	}
	col := findCol(t, block, key)
	if col.sparse {
		t.Fatalf("fully-present date column unexpectedly sparse")
	}
	if col.packedDate {
		t.Fatalf("date column packed before Compact")
	}
	return block
}

// TestEdgePropCols_DatePacksAtCompact asserts a dense narrow-range date column is
// FOR bit-packed at Compact, that every slot round-trips to the identical tagged
// date string, and that the packed backing genuinely uses fewer bytes than the
// plain int32 backing.
func TestEdgePropCols_DatePacksAtCompact(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(3)
	const n = 400
	// A 2192-day window anchored at an arbitrary epoch-day, the ex26 shape: the
	// residual max-min fits in 12 bits.
	const base = int32(18276) // 2020-01-15
	days := make([]int32, n)
	for i := range days {
		days[i] = base + int32((i*53)%2193) // spread across [base, base+2192]
	}
	block := buildFullDenseDateColumn(t, key, days)

	plainBytes := datesColBytes(findCol(t, block, key))

	packed := block.Compact().(*edgePropCols)
	pc := findCol(t, packed, key)
	if !pc.packedDate {
		t.Fatalf("dense narrow-range date column was not packed at Compact")
	}
	if pc.days != nil {
		t.Fatalf("packed column still carries a days[] backing")
	}
	if pc.forWidth != 12 {
		t.Fatalf("forWidth = %d, want 12 (bits.Len(2192))", pc.forWidth)
	}
	// Every slot reads back the identical tagged date string.
	for i := 0; i < n; i++ {
		got, ok := pc.slotValue(i)
		if !ok {
			t.Fatalf("slot %d absent after pack", i)
		}
		want := dateVal(days[i])
		gs, _ := got.String()
		ws, _ := want.String()
		if gs != ws {
			t.Fatalf("slot %d round-trip mismatch: got %q want %q", i, gs, ws)
		}
	}
	// The packed backing must cost fewer bytes than the plain int32 backing.
	packedBytes := datesColBytes(pc)
	if packedBytes >= plainBytes {
		t.Fatalf("packed column not smaller: packed=%d plain=%d bytes", packedBytes, plainBytes)
	}
	t.Logf("date column bytes: plain=%d packed=%d (%.2f -> %.2f B/slot)",
		plainBytes, packedBytes, float64(plainBytes)/n, float64(packedBytes)/n)
}

// datesColBytes returns the resident bytes of a date column's value backing
// (size-class effects ignored: the logical backing bytes are the honest
// per-slot comparison the acceptance criterion asks for). For the packed form it
// is the packed words plus the fixed FOR header; for the plain form it is the
// int32 backing.
func datesColBytes(col *edgePropColumn) int {
	if col.packedDate {
		return cap(col.packed)*8 + forHeaderBytes
	}
	return cap(col.days) * 4
}

// TestEdgePropCols_DatePackRoundTripWithNulls asserts FOR packing preserves the
// validity plane: absent slots stay absent through pack/read, and present slots
// round-trip, with the reference min computed only over present values.
func TestEdgePropCols_DatePackRoundTripWithNulls(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(4)
	const n = 200
	const base = int32(10957) // 2000-01-01
	var block *edgePropCols
	present := make(map[int]int32)
	for i := 0; i < n; i++ {
		if i%3 == 0 {
			continue // leave every third slot absent
		}
		ed := base + int32((i*17)%1000)
		present[i] = ed
		block = block.set(key, i, n, dateVal(ed))
	}
	col := findCol(t, block, key)
	if col.sparse {
		// At ~2/3 fill an int32-width column promotes to dense; if it were sparse the
		// test would not exercise the packed path, so force the dense representation.
		dense := col.toDense()
		block.cols[0] = dense
	}
	packed := block.Compact().(*edgePropCols)
	pc := findCol(t, packed, key)
	if !pc.packedDate {
		t.Fatalf("date column with nulls was not packed (fill too low for the gate?)")
	}
	for i := 0; i < n; i++ {
		got, ok := pc.slotValue(i)
		want, wantPresent := present[i]
		if ok != wantPresent {
			t.Fatalf("slot %d presence = %v, want %v", i, ok, wantPresent)
		}
		if !ok {
			continue
		}
		gs, _ := got.String()
		if gs != epochDayToString(want) {
			t.Fatalf("slot %d = %q, want %q", i, gs, epochDayToString(want))
		}
	}
}

// TestEdgePropCols_DateConstantColumnWidth0 asserts a date column whose present
// values are all equal packs to the constant form (forWidth 0, nil packed slice)
// and reads back the constant value.
func TestEdgePropCols_DateConstantColumnWidth0(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(5)
	const n = 64
	const ed = int32(18276)
	days := make([]int32, n)
	for i := range days {
		days[i] = ed
	}
	block := buildFullDenseDateColumn(t, key, days)
	packed := block.Compact().(*edgePropCols)
	pc := findCol(t, packed, key)
	if !pc.packedDate {
		t.Fatalf("constant date column was not packed")
	}
	if pc.forWidth != 0 {
		t.Fatalf("forWidth = %d, want 0 for a constant column", pc.forWidth)
	}
	if pc.packed != nil {
		t.Fatalf("constant column allocated a packed slice (%d words), want nil", len(pc.packed))
	}
	for i := 0; i < n; i++ {
		got, ok := pc.slotValue(i)
		if !ok {
			t.Fatalf("slot %d absent", i)
		}
		gs, _ := got.String()
		if gs != epochDayToString(ed) {
			t.Fatalf("slot %d = %q, want %q", i, gs, epochDayToString(ed))
		}
	}
}

// TestEdgePropCols_WideRangeDateNotPacked asserts the byte gate rejects a date
// column whose residual range is too wide to save bytes (>= 32-bit residual), so
// it stays a plain int32 column.
func TestEdgePropCols_WideRangeDateNotPacked(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(6)
	const n = 64
	// Two dates 32+ bits apart force a >= 32-bit residual; the rest fill the span.
	days := make([]int32, n)
	days[0] = math.MinInt32
	days[1] = math.MaxInt32
	for i := 2; i < n; i++ {
		days[i] = int32(i)
	}
	block := buildFullDenseDateColumn(t, key, days)
	packed := block.Compact().(*edgePropCols)
	pc := findCol(t, packed, key)
	if pc.packedDate {
		t.Fatalf("a full int32-range date column was packed; the byte gate should reject it")
	}
	if pc.days == nil {
		t.Fatalf("rejected column lost its days[] backing")
	}
}

// TestEdgePropCols_ShortDateColumnNotPacked asserts the minPackLength floor: a
// column shorter than the floor is left plain even when its range is narrow,
// because the FOR header would erase the saving.
func TestEdgePropCols_ShortDateColumnNotPacked(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(7)
	const base = int32(18276)
	days := make([]int32, minPackLength-1)
	for i := range days {
		days[i] = base + int32(i)
	}
	block := buildFullDenseDateColumn(t, key, days)
	packed := block.Compact().(*edgePropCols)
	pc := findCol(t, packed, key)
	if pc.packedDate {
		t.Fatalf("a length-%d column was packed below the minPackLength floor (%d)", len(days), minPackLength)
	}
}

// TestEdgePropCols_MutateAfterPackUnpacks asserts every copy-on-write mutation on
// a packed date column transparently unpacks it to a plain dense column and that
// the mutation's result is correct: set overwrites, set of a new slot, del clears,
// and the unpacked column carries no packed state.
func TestEdgePropCols_MutateAfterPackUnpacks(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(8)
	const n = 64
	const base = int32(18276)
	days := make([]int32, n)
	for i := range days {
		days[i] = base + int32(i)
	}

	t.Run("set-overwrite", func(t *testing.T) {
		t.Parallel()
		packed := buildFullDenseDateColumn(t, key, days).Compact().(*edgePropCols)
		if !findCol(t, packed, key).packedDate {
			t.Fatalf("setup: column not packed")
		}
		newED := base + 5000 // outside the original 64-day span
		out := packed.set(key, 10, n, dateVal(newED))
		oc := findCol(t, out, key)
		if oc.packedDate {
			t.Fatalf("column still packed after set")
		}
		got, ok := oc.slotValue(10)
		if !ok {
			t.Fatalf("slot 10 absent after set")
		}
		gs, _ := got.String()
		if gs != epochDayToString(newED) {
			t.Fatalf("slot 10 = %q, want %q", gs, epochDayToString(newED))
		}
		// A neighbouring slot keeps its original value.
		got2, _ := oc.slotValue(11)
		gs2, _ := got2.String()
		if gs2 != epochDayToString(days[11]) {
			t.Fatalf("slot 11 = %q, want %q (unchanged)", gs2, epochDayToString(days[11]))
		}
	})

	t.Run("del-clears", func(t *testing.T) {
		t.Parallel()
		packed := buildFullDenseDateColumn(t, key, days).Compact().(*edgePropCols)
		out, changed := packed.del(key, 20)
		if !changed {
			t.Fatalf("del reported no change")
		}
		oc := findCol(t, out, key)
		if oc.packedDate {
			t.Fatalf("column still packed after del")
		}
		if _, ok := oc.slotValue(20); ok {
			t.Fatalf("slot 20 present after del")
		}
		// Untouched slots survive.
		if got, ok := oc.slotValue(21); !ok {
			t.Fatalf("slot 21 wrongly cleared")
		} else if gs, _ := got.String(); gs != epochDayToString(days[21]) {
			t.Fatalf("slot 21 = %q, want %q", gs, epochDayToString(days[21]))
		}
	})

	t.Run("grow-and-compact-splice", func(t *testing.T) {
		t.Parallel()
		packed := buildFullDenseDateColumn(t, key, days).Compact().(*edgePropCols)
		// GrowSlot then CompactSlot both reach the packed column via grown/compacted.
		grown := packed.GrowSlot(n).(*edgePropCols)
		gc := findCol(t, grown, key)
		if gc.packedDate {
			t.Fatalf("column still packed after GrowSlot")
		}
		if _, ok := gc.slotValue(n); ok {
			t.Fatalf("the grown slot %d should be absent", n)
		}
		if got, ok := gc.slotValue(0); !ok {
			t.Fatalf("slot 0 lost after grow")
		} else if gs, _ := got.String(); gs != epochDayToString(days[0]) {
			t.Fatalf("slot 0 = %q, want %q", gs, epochDayToString(days[0]))
		}

		packed2 := buildFullDenseDateColumn(t, key, days).Compact().(*edgePropCols)
		comp := packed2.CompactSlot(0).(*edgePropCols)
		cc := findCol(t, comp, key)
		if cc.packedDate {
			t.Fatalf("column still packed after CompactSlot")
		}
		// After excising slot 0, the old slot 1 shifts down to index 0.
		if got, ok := cc.slotValue(0); !ok {
			t.Fatalf("slot 0 absent after compaction")
		} else if gs, _ := got.String(); gs != epochDayToString(days[1]) {
			t.Fatalf("post-compaction slot 0 = %q, want old-slot-1 %q", gs, epochDayToString(days[1]))
		}
	})
}

// TestForBitKernel exercises the LSB-first pack/unpack kernels directly across
// widths that do and do not divide 64 (so the uint64-boundary straddle is hit),
// asserting every residual round-trips for a fully-present run.
func TestForBitKernel(t *testing.T) {
	t.Parallel()
	allValid := func(int) bool { return true }
	for _, width := range []uint8{1, 3, 7, 12, 17, 31} {
		t.Run("w"+itoa(int(width)), func(t *testing.T) {
			t.Parallel()
			const length = 300
			maxRes := int32((uint64(1) << width) - 1)
			vals := make([]int32, length)
			for i := range vals {
				vals[i] = int32((int64(i) * 2654435761) & int64(maxRes)) // pseudo-random residual in range
			}
			packed := packResidualsLSB(vals, length, 0, width, allValid)
			if got, want := len(packed), wordsForBits(length*int(width)); got != want {
				t.Fatalf("packed words = %d, want %d", got, want)
			}
			for i := 0; i < length; i++ {
				if got := int32(unpackResidualLSB(packed, i, width)); got != vals[i] {
					t.Fatalf("width %d slot %d: unpack = %d, want %d", width, i, got, vals[i])
				}
			}
		})
	}
}

// TestBitsForRange asserts the residual bit width matches bits.Len of the span.
func TestBitsForRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mn, mx int32
		want   uint8
	}{
		{0, 0, 0},                          // constant
		{5, 5, 0},                          // constant non-zero
		{0, 1, 1},                          // 1-bit span
		{10, 13, 2},                        // span 3 -> 2 bits
		{18276, 18276 + 2192, 12},          // the ex26 window
		{math.MinInt32, math.MaxInt32, 32}, // full int32 span
	}
	for _, tc := range cases {
		if got := bitsForRange(tc.mn, tc.mx); got != tc.want {
			t.Fatalf("bitsForRange(%d,%d) = %d, want %d", tc.mn, tc.mx, got, tc.want)
		}
	}
}

// itoa is a tiny strconv.Itoa replacement kept local so the test file's import
// set is unchanged.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// BenchmarkDateColumnPacking measures the resident bytes per slot of a dense
// narrow-range date column before and after FOR bit-packing, the acceptance
// evidence for the ~4 -> ~1.5 B/slot column-tier reduction (#1663). It reports a
// custom B/slot metric for each. The whole-graph reduction is smaller (~10%) once
// the irreducible Go-runtime per-edge overhead is included; this benchmark
// isolates the date column's own value backing.
func BenchmarkDateColumnPacking(b *testing.B) {
	const key = PropertyKeyID(9)
	const n = 4096
	const base = int32(18276)
	days := make([]int32, n)
	for i := range days {
		days[i] = base + int32((i*53)%2193) // ex26 shape: 2192-day window, 12-bit residual
	}
	var plain *edgePropCols
	for i, ed := range days {
		plain = plain.set(key, i, i+1, dateVal(ed))
	}
	plainCol := &plain.cols[0]
	packed := plain.Compact().(*edgePropCols)
	packedCol := &packed.cols[0]
	if !packedCol.packedDate {
		b.Fatalf("date column not packed; benchmark would be meaningless")
	}

	plainBytes := cap(plainCol.days) * 4
	packedBytes := cap(packedCol.packed)*8 + forHeaderBytes
	// Log the headline before/after so the evidence is visible even when the
	// benchmark formatter suppresses same-unit custom metric columns.
	b.Logf("date column value backing: plain=%d B (%.3f B/slot) -> packed=%d B (%.3f B/slot), %.1f%% smaller",
		plainBytes, float64(plainBytes)/n, packedBytes, float64(packedBytes)/n,
		100*(1-float64(packedBytes)/float64(plainBytes)))
	b.ReportMetric(float64(plainBytes)/n, "plainB/slot")
	b.ReportMetric(float64(packedBytes)/n, "packedB/slot")

	// Drive the random-access unpack read path so the timed region reflects the
	// per-slot decode cost (the string allocation is epochDayToString, not the
	// unpack, which is alloc-free).
	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		v, ok := packedCol.slotValue(i % n)
		if ok {
			s, _ := v.String()
			sink += len(s)
		}
	}
	_ = sink
}
