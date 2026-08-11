package lpg

import (
	"fmt"
	"testing"
)

// The instMap tests pin the behaviour the four per-edge side stores rely on
// after sprint 339 replaced their inner Go map with a tiered union. The store
// suites exercise the one-instance case heavily, so the cases that matter most
// here are the ones they do not reach: the promotion boundary, one-way
// promotion, and deletion from the small tier — which reorders it.

// bagOf builds a labelBag carrying exactly the ids given, so a test can assert
// on a value rather than on instMap's internals.
func bagOf(ids ...LabelID) labelBag {
	var b labelBag
	for _, id := range ids {
		b.add(id)
	}
	return b
}

func TestInstMap_ZeroValueIsUsable(t *testing.T) {
	var im instMap[int64, labelBag]
	if got := im.len(); got != 0 {
		t.Fatalf("len of zero instMap = %d, want 0", got)
	}
	if _, ok := im.get(1); ok {
		t.Fatal("get on a zero instMap reported a hit")
	}
	// A del on the zero value must not panic: the abort path calls it for a
	// pair that may never have existed.
	im.del(1)
	seen := 0
	im.forEachKey(func(int64) bool { seen++; return true })
	if seen != 0 {
		t.Fatalf("forEachKey visited %d keys on a zero instMap", seen)
	}
}

func TestInstMap_SmallTierGetSetDel(t *testing.T) {
	var im instMap[int64, labelBag]
	im.set(1, bagOf(10))
	im.set(2, bagOf(20, 21))

	if im.m != nil {
		t.Fatal("two instances promoted to the map tier; the small tier should hold them")
	}
	if got := im.len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}
	b, ok := im.get(2)
	if !ok || b.len() != 2 || !b.has(20) || !b.has(21) {
		t.Fatalf("get(2) = %v, %v; want the two-label bag", b, ok)
	}

	// Overwrite in place rather than append a duplicate.
	im.set(1, bagOf(11))
	if got := im.len(); got != 2 {
		t.Fatalf("len after overwrite = %d, want 2", got)
	}
	if b, _ := im.get(1); !b.has(11) || b.has(10) {
		t.Fatalf("get(1) after overwrite = %v; want only label 11", b)
	}

	im.del(1)
	if _, ok := im.get(1); ok {
		t.Fatal("get(1) reported a hit after del")
	}
	if got := im.len(); got != 1 {
		t.Fatalf("len after del = %d, want 1", got)
	}
	// Deleting from the small tier moves the last element into the hole, so
	// the SURVIVOR must still be readable — the case a shift-based removal
	// would get right by accident and a swap-based one can get wrong.
	if b, ok := im.get(2); !ok || !b.has(20) {
		t.Fatalf("get(2) after deleting a sibling = %v, %v; want the surviving bag", b, ok)
	}

	im.del(2)
	if got := im.len(); got != 0 {
		t.Fatalf("len after deleting every instance = %d, want 0", got)
	}
	// del of an absent key is a no-op, not a panic.
	im.del(99)
}

// TestInstMap_PromotionBoundary walks the small tier up to smallInstMax and one
// past it, checking every instance survives the promotion. This is the path a
// pair with many parallel edges takes and the one the store suites never reach.
func TestInstMap_PromotionBoundary(t *testing.T) {
	var im instMap[int64, labelBag]
	for i := 1; i <= smallInstMax; i++ {
		im.set(int64(i), bagOf(LabelID(i)))
	}
	if im.m != nil {
		t.Fatalf("promoted at %d instances; smallInstMax is %d", smallInstMax, smallInstMax)
	}
	if got := im.len(); got != smallInstMax {
		t.Fatalf("len = %d, want %d", got, smallInstMax)
	}

	im.set(int64(smallInstMax+1), bagOf(LabelID(smallInstMax+1)))
	if im.m == nil {
		t.Fatalf("did not promote at %d instances", smallInstMax+1)
	}
	if im.pairs != nil {
		t.Fatal("promotion left the small tier populated; it must be released")
	}
	if got := im.len(); got != smallInstMax+1 {
		t.Fatalf("len after promotion = %d, want %d", got, smallInstMax+1)
	}
	for i := 1; i <= smallInstMax+1; i++ {
		b, ok := im.get(int64(i))
		if !ok || !b.has(LabelID(i)) {
			t.Fatalf("instance %d lost across promotion: %v, %v", i, b, ok)
		}
	}

	// Promotion is one-way: shrinking below the threshold keeps the map tier,
	// so a hub pair never oscillates between representations.
	for i := 1; i <= smallInstMax; i++ {
		im.del(int64(i))
	}
	if im.m == nil {
		t.Fatal("demoted back to the small tier; promotion must be one-way")
	}
	if got := im.len(); got != 1 {
		t.Fatalf("len after shrinking = %d, want 1", got)
	}
}

func TestInstMap_ForEachKeyVisitsAllAndStopsEarly(t *testing.T) {
	for _, n := range []int{1, smallInstMax, smallInstMax + 4} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			var im instMap[uint64, propBag]
			for i := 1; i <= n; i++ {
				im.set(uint64(i), propBag{})
			}
			seen := map[uint64]bool{}
			im.forEachKey(func(k uint64) bool { seen[k] = true; return true })
			if len(seen) != n {
				t.Fatalf("forEachKey visited %d keys, want %d", len(seen), n)
			}
			for i := 1; i <= n; i++ {
				if !seen[uint64(i)] {
					t.Fatalf("forEachKey never visited key %d", i)
				}
			}
			// Early exit is load-bearing: the MVCC pre-image walks stop at the
			// first refusal rather than recording pre-images that can never be
			// published.
			visits := 0
			im.forEachKey(func(uint64) bool { visits++; return false })
			if visits != 1 {
				t.Fatalf("forEachKey made %d visits after fn returned false, want 1", visits)
			}
		})
	}
}

// TestInstMap_DelClearsTheVacatedSlot proves the removed entry is not left
// reachable behind the slice length. A bag can hold a promoted map, so a stale
// copy in the backing array would keep it alive for as long as the pair lives.
func TestInstMap_DelClearsTheVacatedSlot(t *testing.T) {
	var im instMap[int64, propBag]
	im.set(1, propBag{})
	im.set(2, propBag{})
	// Give instance 2 a bag whose identity is observable.
	b, _ := im.get(2)
	b.set(PropertyKeyID(7), Int64Value(42))
	im.set(2, b)

	im.del(2)
	if got := len(im.pairs); got != 1 {
		t.Fatalf("len(pairs) after del = %d, want 1", got)
	}
	// Reach past the length into the backing array; the vacated slot must be
	// the zero entry, not a copy of the deleted instance.
	full := im.pairs[:cap(im.pairs)]
	if len(full) < 2 {
		t.Skip("backing array did not outlive the truncation; nothing to assert")
	}
	if full[1].key != 0 {
		t.Fatalf("vacated slot still holds key %d; del must clear it", full[1].key)
	}
	if full[1].val.len() != 0 {
		t.Fatal("vacated slot still holds a populated bag; del must clear it")
	}
}
