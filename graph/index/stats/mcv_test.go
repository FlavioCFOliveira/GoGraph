package stats

import (
	"sort"
	"testing"
)

func intEq(a, b int) bool { return a == b }

// buildEntries turns a value→count map into MCVEntry slices, using the value
// itself as a stand-in hash (distinct values get distinct hashes).
func buildEntries(freq map[int]int64) []MCVEntry[int] {
	out := make([]MCVEntry[int], 0, len(freq))
	for v, c := range freq {
		out = append(out, MCVEntry[int]{Value: v, Hash: uint64(v), Count: c})
	}
	return out
}

// TestMCV_ExactTopK asserts BuildTopK returns exactly the k highest-count values
// with their EXACT counts, matched against a full sort of the ground truth.
func TestMCV_ExactTopK(t *testing.T) {
	freq := map[int]int64{}
	// 400 distinct values with a spread of counts.
	for v := 0; v < 400; v++ {
		freq[v] = int64((v*37)%101 + 1)
	}
	// A few deliberate spikes.
	freq[1000] = 5000
	freq[1001] = 4999
	freq[1002] = 4998

	entries := buildEntries(freq)
	const k = 32
	got := BuildTopK(entries, k)
	if got.Len() != k {
		t.Fatalf("top-k len = %d, want %d", got.Len(), k)
	}

	// Ground truth: sort all by (count desc, hash asc), take first k.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Hash < entries[j].Hash
	})
	want := entries[:k]

	gotEntries := got.Entries()
	for i := 0; i < k; i++ {
		if gotEntries[i].Value != want[i].Value || gotEntries[i].Count != want[i].Count {
			t.Errorf("rank %d: got {v=%d c=%d} want {v=%d c=%d}",
				i, gotEntries[i].Value, gotEntries[i].Count, want[i].Value, want[i].Count)
		}
	}

	// The three spikes must be the top three, with exact counts.
	if gotEntries[0].Value != 1000 || gotEntries[0].Count != 5000 {
		t.Errorf("top value = {v=%d c=%d}, want {1000, 5000}", gotEntries[0].Value, gotEntries[0].Count)
	}
}

// TestMCV_LookupExactAndCollision confirms Lookup returns the exact stored count
// and never returns a wrong count when two distinct values share a hash.
func TestMCV_LookupExactAndCollision(t *testing.T) {
	// Two distinct values (7 and 99) forced to share hash 42; eq must disambiguate.
	entries := []MCVEntry[int]{
		{Value: 7, Hash: 42, Count: 100},
		{Value: 99, Hash: 42, Count: 55},
		{Value: 5, Hash: 5, Count: 200},
	}
	l := BuildTopK(entries, 32)

	if c, ok := l.Lookup(42, 7, intEq); !ok || c != 100 {
		t.Errorf("lookup(7) = (%d,%v), want (100,true)", c, ok)
	}
	if c, ok := l.Lookup(42, 99, intEq); !ok || c != 55 {
		t.Errorf("lookup(99) = (%d,%v), want (55,true) — collision must be resolved by eq", c, ok)
	}
	if c, ok := l.Lookup(5, 5, intEq); !ok || c != 200 {
		t.Errorf("lookup(5) = (%d,%v), want (200,true)", c, ok)
	}
	// A value absent from the list must miss.
	if _, ok := l.Lookup(1234, 1234, intEq); ok {
		t.Error("lookup of absent value returned present")
	}
	// A hash hit whose eq fails (collision with a non-stored value) must miss.
	if _, ok := l.Lookup(42, 12345, intEq); ok {
		t.Error("hash collision with non-stored value must miss after eq check")
	}
}

func TestMCV_Degenerate(t *testing.T) {
	if l := BuildTopK[int](nil, 32); l.Len() != 0 {
		t.Errorf("nil entries: len %d, want 0", l.Len())
	}
	if l := BuildTopK([]MCVEntry[int]{{Value: 1, Hash: 1, Count: 1}}, 0); l.Len() != 0 {
		t.Errorf("k=0: len %d, want 0", l.Len())
	}
	// k larger than the input is clamped to the input size.
	l := BuildTopK([]MCVEntry[int]{{Value: 1, Hash: 1, Count: 1}}, 32)
	if l.Len() != 1 {
		t.Errorf("k>len: len %d, want 1", l.Len())
	}
}
