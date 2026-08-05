package mvcc

// depthhist_test.go — rmp #2312: the bucketing boundaries, and the reason the exact
// maximum is carried beside them.

import "testing"

// TestDepthHist_BucketBoundaries pins which bucket each depth falls in, at the edges.
//
// The boundaries are the whole content of a log2 histogram: an off-by-one in
// bits.Len would shift every reading by a factor of two and nothing else would
// notice.
func TestDepthHist_BucketBoundaries(t *testing.T) {
	cases := []struct {
		depth int
		want  int
	}{
		{1, 0},
		{2, 1}, {3, 1},
		{4, 2}, {7, 2},
		{8, 3}, {15, 3},
		{16, 4}, {31, 4},
		{32, 5}, {63, 5},
		{64, 6}, {127, 6},
		{128, 7}, {1 << 20, 7}, // the last bucket is unbounded above
	}
	for _, c := range cases {
		var h DepthHist
		h.Observe(c.depth)
		d := h.Load()
		if d.Buckets[c.want] != 1 {
			t.Errorf("depth %d did not land in bucket %d (%q); buckets=%v",
				c.depth, c.want, DepthBucketLabel(c.want), d.Buckets)
		}
		if d.Chains() != 1 {
			t.Errorf("depth %d produced %d chains, want 1", c.depth, d.Chains())
		}
		if lo := DepthBucketLow(c.want); c.depth < lo {
			t.Errorf("depth %d landed in a bucket whose floor is %d", c.depth, lo)
		}
	}
}

// TestDepthHist_ZeroIsNotAChain asserts a chain the reclaimer removed entirely is not
// counted.
//
// A depth of zero is not a short chain, it is an absent one, and counting it in the
// first bucket would make a store the sweep emptied look like a store full of
// one-record chains.
func TestDepthHist_ZeroIsNotAChain(t *testing.T) {
	var h DepthHist
	h.Observe(0)
	h.Observe(-1)
	if d := h.Load(); d.Chains() != 0 || d.Deepest != 0 {
		t.Errorf("a zero-depth observation was counted: chains=%d deepest=%d", d.Chains(), d.Deepest)
	}
}

// TestDepthHist_DeepestIsExact asserts the exact maximum survives the last bucket's
// unbounded top.
//
// Two chains of 130 and 5000 both fall in bucket 7, so without this the distribution
// cannot distinguish a mildly deep chain from a pathological one — which is the exact
// failure a mean has and the reason a distribution was chosen over one.
func TestDepthHist_DeepestIsExact(t *testing.T) {
	var h DepthHist
	h.Observe(130)
	h.Observe(5000)
	h.Observe(4)
	d := h.Load()
	if d.Deepest != 5000 {
		t.Errorf("Deepest = %d, want 5000", d.Deepest)
	}
	if d.Buckets[DepthBuckets-1] != 2 {
		t.Errorf("the unbounded bucket holds %d, want 2", d.Buckets[DepthBuckets-1])
	}
}

// TestDepthHist_ResetClearsEverything asserts a reset leaves a histogram
// indistinguishable from a fresh one, including the maximum.
//
// The reclaimer resets before each sweep of its store, so a maximum that survived a
// reset would latch: one deep chain in one pass would be reported for the life of the
// process.
func TestDepthHist_ResetClearsEverything(t *testing.T) {
	var h DepthHist
	h.Observe(300)
	h.Reset()
	d := h.Load()
	if d.Chains() != 0 {
		t.Errorf("Reset left %d chains", d.Chains())
	}
	if d.Deepest != 0 {
		t.Errorf("Reset left Deepest = %d: one deep chain would be reported for ever", d.Deepest)
	}
}

// TestDepths_AddCombinesStores asserts several stores' distributions sum into one, and
// that the combined maximum is the larger of the two rather than their sum.
func TestDepths_AddCombinesStores(t *testing.T) {
	a := Depths{Deepest: 5}
	a.Buckets[0] = 3
	b := Depths{Deepest: 40}
	b.Buckets[0] = 1
	b.Buckets[5] = 2
	a.Add(b)
	if a.Buckets[0] != 4 || a.Buckets[5] != 2 {
		t.Errorf("buckets did not sum: %v", a.Buckets)
	}
	if a.Chains() != 6 {
		t.Errorf("Chains() = %d, want 6", a.Chains())
	}
	if a.Deepest != 40 {
		t.Errorf("Deepest = %d, want 40: a combined maximum is the larger, never a sum", a.Deepest)
	}
}
