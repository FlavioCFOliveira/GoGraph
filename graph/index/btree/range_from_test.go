package btree_test

// range_from_test.go — regression for F-CY1: an unbounded-above string range
// must return every key >= lo, including a key that sorts ABOVE the fixed
// 32-byte 0xFF sentinel the old string range-seek path capped the scan at. A
// variable-length string key has no representable greatest value, so the fixed
// sentinel silently excluded any such key; RangeFrom scans open-ended and is a
// genuine superset.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// oldSentinel mirrors the fixed 32-byte upper sentinel the pre-fix string range
// seek used. A key sorting above it exercises the exact dropped-row bug.
const oldSentinel = "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
	"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"

func TestRangeFrom_ReturnsKeyAboveOldSentinel(t *testing.T) {
	idx := btree.New[string]()
	// A key sorting strictly above the 32-byte sentinel (33 leading 0xFF bytes).
	aboveSentinel := strings.Repeat("\xff", 33)
	idx.Insert("apple", graph.NodeID(1))
	idx.Insert("mango", graph.NodeID(2))
	idx.Insert(aboveSentinel, graph.NodeID(3))

	// Demonstrate the OLD bug: a bounded scan capped at the sentinel excludes
	// the above-sentinel key.
	capped := idx.Range("", oldSentinel)
	if capped.Contains(3) {
		t.Fatalf("precondition: key above sentinel should NOT be in the sentinel-capped range")
	}

	// The FIX: RangeFrom is open-ended and returns every key >= lo, including
	// the above-sentinel key.
	got := idx.RangeFrom("")
	for _, want := range []uint64{1, 2, 3} {
		if !got.Contains(want) {
			t.Errorf("RangeFrom(\"\"): missing node %d", want)
		}
	}
	// A lower bound of "a" still includes every real key here and the
	// above-sentinel key (0xFF bytes sort after any ASCII).
	got2 := idx.RangeFrom("a")
	if !got2.Contains(3) {
		t.Errorf("RangeFrom(\"a\"): above-sentinel key (node 3) must be present")
	}
}

func TestRangeCountFrom_MatchesRangeFrom(t *testing.T) {
	idx := btree.New[string]()
	idx.Insert("a", graph.NodeID(1))
	idx.Insert("b", graph.NodeID(2))
	idx.Insert("b", graph.NodeID(3)) // duplicate key, distinct node
	idx.Insert(strings.Repeat("\xff", 40), graph.NodeID(4))

	bm := idx.RangeFrom("a")
	count, exact := idx.RangeCountFrom("a", 1<<20)
	if !exact {
		t.Fatalf("RangeCountFrom under a generous budget must be exact")
	}
	if count != bm.GetCardinality() {
		t.Errorf("RangeCountFrom=%d disagrees with RangeFrom cardinality=%d", count, bm.GetCardinality())
	}
	if count != 4 {
		t.Errorf("expected 4 nodes >= \"a\", got %d", count)
	}
}

func TestRangeCountFrom_BudgetEarlyExit(t *testing.T) {
	idx := btree.New[string]()
	for i := 0; i < 100; i++ {
		idx.Insert("k", graph.NodeID(i+1))
	}
	count, exact := idx.RangeCountFrom("a", 10)
	if exact {
		t.Errorf("expected early-exit (not exact) when count exceeds budget")
	}
	if count != 11 { // budget+1
		t.Errorf("expected budget+1 (11) on early exit, got %d", count)
	}
}
