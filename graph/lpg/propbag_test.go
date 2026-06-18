package lpg

import (
	"sort"
	"testing"
)

// collectBag returns the bag's (keyID -> value) contents as a map, the
// representation-independent reference every assertion compares against.
func collectBag(b *propBag) map[PropertyKeyID]PropertyValue {
	out := make(map[PropertyKeyID]PropertyValue)
	b.forEach(func(k PropertyKeyID, v PropertyValue) { out[k] = v })
	return out
}

// TestPropBag_SmallTierBasics covers get/set/del in the small (slice) tier.
func TestPropBag_SmallTierBasics(t *testing.T) {
	t.Parallel()
	var b propBag

	if _, ok := b.get(1); ok {
		t.Fatal("empty bag reported key 1 present")
	}
	if b.len() != 0 {
		t.Fatalf("empty bag len = %d, want 0", b.len())
	}

	b.set(1, Int64Value(10))
	b.set(2, StringValue("two"))
	if b.m != nil {
		t.Fatal("bag promoted to map at 2 entries; want small tier")
	}
	if b.len() != 2 {
		t.Fatalf("len = %d, want 2", b.len())
	}
	if v, ok := b.get(1); !ok {
		t.Fatal("key 1 missing")
	} else if i, _ := v.Int64(); i != 10 {
		t.Fatalf("key 1 = %d, want 10", i)
	}
	if v, ok := b.get(2); !ok {
		t.Fatal("key 2 missing")
	} else if s, _ := v.String(); s != "two" {
		t.Fatalf("key 2 = %q, want two", s)
	}

	// Overwrite is in-place, no growth.
	b.set(1, Int64Value(99))
	if b.len() != 2 {
		t.Fatalf("overwrite changed len to %d, want 2", b.len())
	}
	if v, _ := b.get(1); func() int64 { i, _ := v.Int64(); return i }() != 99 {
		t.Fatal("overwrite of key 1 did not take")
	}
}

// TestPropBag_DeleteEmptiesAndSwap confirms del reports emptiness and that the
// swap-delete keeps the remaining contents intact regardless of order.
func TestPropBag_DeleteEmptiesAndSwap(t *testing.T) {
	t.Parallel()
	var b propBag
	for i := PropertyKeyID(1); i <= 4; i++ {
		b.set(i, Int64Value(int64(i)))
	}
	// Delete a middle key: swap-delete must preserve the other three.
	if empty := b.del(2); empty {
		t.Fatal("del(2) reported empty with 3 keys remaining")
	}
	got := collectBag(&b)
	if len(got) != 3 {
		t.Fatalf("after del(2): len %d, want 3", len(got))
	}
	for _, k := range []PropertyKeyID{1, 3, 4} {
		if _, ok := got[k]; !ok {
			t.Errorf("after del(2): key %d missing", k)
		}
	}
	if _, ok := got[2]; ok {
		t.Error("after del(2): key 2 still present")
	}
	// Deleting an absent key is a no-op.
	if empty := b.del(99); empty {
		t.Fatal("del(absent) reported empty")
	}
	// Drain to empty.
	b.del(1)
	b.del(3)
	if empty := b.del(4); !empty {
		t.Fatal("del of last key did not report empty")
	}
	if b.len() != 0 {
		t.Fatalf("drained bag len = %d, want 0", b.len())
	}
}

// TestPropBag_PromotionThreshold confirms the bag promotes to the map tier on
// the (smallBagMax+1)th distinct key and never demotes thereafter.
func TestPropBag_PromotionThreshold(t *testing.T) {
	t.Parallel()
	var b propBag
	for i := 0; i < smallBagMax; i++ {
		b.set(PropertyKeyID(i), Int64Value(int64(i)))
	}
	if b.m != nil {
		t.Fatalf("promoted at %d entries; want small until %d", smallBagMax, smallBagMax)
	}
	// Cross the threshold.
	b.set(PropertyKeyID(smallBagMax), Int64Value(int64(smallBagMax)))
	if b.m == nil {
		t.Fatalf("did not promote at %d entries", smallBagMax+1)
	}
	if b.pairs != nil {
		t.Fatal("promoted bag still holds a pairs slice")
	}
	if b.len() != smallBagMax+1 {
		t.Fatalf("promoted len = %d, want %d", b.len(), smallBagMax+1)
	}
	// All contents survived the promotion.
	got := collectBag(&b)
	for i := 0; i <= smallBagMax; i++ {
		v, ok := got[PropertyKeyID(i)]
		if !ok {
			t.Errorf("promoted bag lost key %d", i)
			continue
		}
		if iv, _ := v.Int64(); iv != int64(i) {
			t.Errorf("promoted key %d = %d, want %d", i, iv, i)
		}
	}
	// Never demotes: deleting back down to one key keeps the map tier.
	for i := 0; i < smallBagMax; i++ {
		b.del(PropertyKeyID(i))
	}
	if b.m == nil {
		t.Fatal("bag demoted from map tier; promote-and-never-demote violated")
	}
	if b.len() != 1 {
		t.Fatalf("after draining: len %d, want 1", b.len())
	}
}

// TestPropBag_MapTierOverwriteAndGet confirms get/set/del semantics are
// identical once the bag is in the map tier.
func TestPropBag_MapTierOverwriteAndGet(t *testing.T) {
	t.Parallel()
	var b propBag
	for i := 0; i <= smallBagMax; i++ { // forces promotion
		b.set(PropertyKeyID(i), Int64Value(int64(i)))
	}
	if b.m == nil {
		t.Fatal("bag not promoted")
	}
	b.set(3, StringValue("overwritten"))
	if v, ok := b.get(3); !ok {
		t.Fatal("key 3 missing in map tier")
	} else if s, _ := v.String(); s != "overwritten" {
		t.Fatalf("key 3 = %q, want overwritten", s)
	}
	if _, ok := b.get(123456); ok {
		t.Fatal("map tier reported absent key present")
	}
}

// TestPropBag_AllKindsRoundTrip confirms every PropertyKind survives storage
// and retrieval unchanged through the bag — the bag is kind-agnostic.
func TestPropBag_AllKindsRoundTrip(t *testing.T) {
	t.Parallel()
	var b propBag
	want := map[PropertyKeyID]PropertyValue{
		1: StringValue("s"),
		2: Int64Value(-7),
		3: Float64Value(3.5),
		4: BoolValue(true),
		5: BytesValue([]byte{1, 2, 3}),
		6: ListValue([]PropertyValue{Int64Value(1), StringValue("x")}),
	}
	keys := make([]int, 0, len(want))
	for k := range want {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		b.set(PropertyKeyID(k), want[PropertyKeyID(k)])
	}
	for k, wv := range want {
		gv, ok := b.get(k)
		if !ok {
			t.Errorf("key %d missing", k)
			continue
		}
		if gv.Kind() != wv.Kind() {
			t.Errorf("key %d kind = %d, want %d", k, gv.Kind(), wv.Kind())
		}
	}
}
