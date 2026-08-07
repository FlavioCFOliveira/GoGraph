package mvcc

// stores_test.go — the conflict-store table is the thing that bounds the per-store
// series' cardinality, so its two invariants are pinned here (rmp #2312).

import (
	"strings"
	"testing"
)

// TestConflictStores_EveryNameHasAnIndex asserts that every store CONSTANT resolves
// to its own entry, and that none of them silently lands in the other bucket.
//
// A constant that is not in the table would be counted under StoreOther, which loses
// the attribution without losing the count — a failure that is invisible in
// production because the total still adds up. This is the test that makes it visible.
func TestConflictStores_EveryNameHasAnIndex(t *testing.T) {
	names := []string{
		StoreNodeLabels, StoreNodeProperties, StoreNodeExistence, StoreAdjacency,
		StoreEdgeTypes, StoreEdgeTypesHandle, StoreEdgeTypesOrd,
		StoreEdgePropsHandle, StoreEdgePropsOrd,
	}
	other := ConflictStoreIndex(StoreOther)
	seen := make(map[int]string, len(names))
	for _, n := range names {
		i := ConflictStoreIndex(n)
		if i == other {
			t.Errorf("store %q resolves to the StoreOther bucket: its per-store series would "+
				"report under %q and the attribution is lost", n, StoreOther)
			continue
		}
		if prev, dup := seen[i]; dup {
			t.Errorf("stores %q and %q share index %d: two structures' contention would be "+
				"reported as one", prev, n, i)
		}
		seen[i] = n
		if got := ConflictStoreName(i); got != n {
			t.Errorf("ConflictStoreName(%d) = %q, want %q", i, got, n)
		}
	}
	if len(seen) != ConflictStoreCount-1 {
		t.Errorf("%d stores have their own bucket but the table has %d non-other entries: a "+
			"bucket nothing can reach, or a store this test does not know about",
			len(seen), ConflictStoreCount-1)
	}
}

// TestConflictStores_MetricSuffixIsNameSafe asserts the metric suffix carries no
// character a Prometheus metric name may not hold.
//
// The human names have spaces — "node labels" — and a name that only becomes valid
// after a backend sanitises it is a name nobody can predict from the source. Two
// suffixes must also stay distinct, or two stores collapse into one series.
func TestConflictStores_MetricSuffixIsNameSafe(t *testing.T) {
	seen := make(map[string]int, ConflictStoreCount)
	for i := 0; i < ConflictStoreCount; i++ {
		m := ConflictStoreMetric(i)
		if m == "" {
			t.Errorf("bucket %d (%q) has an empty metric suffix", i, ConflictStoreName(i))
			continue
		}
		for _, r := range m {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("metric suffix %q for %q holds %q, which a Prometheus metric name "+
					"may not carry", m, ConflictStoreName(i), r)
				break
			}
		}
		if prev, dup := seen[m]; dup {
			t.Errorf("buckets %d and %d both publish under suffix %q", prev, i, m)
		}
		seen[m] = i
	}
	// The one that motivated the split: the human name has a space, the suffix does not.
	if !strings.Contains(StoreNodeLabels, " ") {
		t.Fatalf("StoreNodeLabels = %q no longer has a space, so this test no longer covers "+
			"the case the two spellings exist for", StoreNodeLabels)
	}
}

// TestConflictStores_UnknownNameIsBucketed asserts an unrecognised store is counted
// under StoreOther rather than panicking or being dropped.
func TestConflictStores_UnknownNameIsBucketed(t *testing.T) {
	if got, want := ConflictStoreIndex("a store nobody declared"), ConflictStoreCount-1; got != want {
		t.Errorf("an unknown store resolved to index %d, want the StoreOther bucket at %d", got, want)
	}
}
