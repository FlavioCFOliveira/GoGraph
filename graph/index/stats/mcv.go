package stats

import "container/heap"

// MCVEntry is one most-common-value: the value, its equality-consistent hash
// (the same hash folded into the [HLL]), and its EXACT row count.
type MCVEntry[T any] struct {
	Value T
	Hash  uint64
	Count int64
}

// MCVList is the exact top-k most-common values of a column, ordered by
// descending count (ties broken by ascending hash for determinism). It is
// immutable once built and safe for concurrent reads.
type MCVList[T any] struct {
	entries []MCVEntry[T]
}

// mcvHeap is a min-heap of MCVEntry whose root is the LEAST-preferred retained
// entry — the one evicted first while selecting the top k. Preference is (count
// desc, hash asc): a higher count wins, and on a tie the smaller hash wins. So
// the "worst" (heap minimum) is the lowest count, and among equal counts the
// LARGEST hash. Draining the heap ascending then reversing yields the retained
// set in (count desc, hash asc) order, deterministic regardless of input order.
type mcvHeap[T any] []MCVEntry[T]

func (h mcvHeap[T]) Len() int { return len(h) }
func (h mcvHeap[T]) Less(i, j int) bool {
	if h[i].Count != h[j].Count {
		return h[i].Count < h[j].Count
	}
	return h[i].Hash > h[j].Hash // larger hash is less preferred (evicted first)
}
func (h mcvHeap[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mcvHeap[T]) Push(x any)   { *h = append(*h, x.(MCVEntry[T])) }
func (h *mcvHeap[T]) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// BuildTopK selects the k highest-count entries from all with a bounded min-heap
// (O(n log k) time, O(k) space) and returns them ordered by descending count. It
// is exact — the entries carry true per-value counts from the rebuild scan, not a
// sketch estimate — because a safety gate needs the true count, and a Count-Min
// sketch's over-estimate bias is the wrong direction for such a gate. A k ≤ 0
// yields an empty list.
func BuildTopK[T any](all []MCVEntry[T], k int) *MCVList[T] {
	if k <= 0 || len(all) == 0 {
		return &MCVList[T]{}
	}
	if k > len(all) {
		k = len(all)
	}
	h := make(mcvHeap[T], 0, k)
	heap.Init(&h)
	for i := range all {
		if len(h) < k {
			heap.Push(&h, all[i])
			continue
		}
		// Retain all[i] only if it is more preferred than the least-preferred
		// retained entry (the root): higher count, or equal count and smaller hash.
		if h[0].Count < all[i].Count ||
			(h[0].Count == all[i].Count && h[0].Hash > all[i].Hash) {
			h[0] = all[i]
			heap.Fix(&h, 0)
		}
	}
	// Drain the heap (ascending) then reverse to descending count.
	out := make([]MCVEntry[T], len(h))
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(&h).(MCVEntry[T])
	}
	return &MCVList[T]{entries: out}
}

// Len reports the number of retained most-common values.
func (l *MCVList[T]) Len() int { return len(l.entries) }

// Entries returns the retained values in descending-count order. The slice is
// the list's own backing array; callers must not mutate it.
func (l *MCVList[T]) Entries() []MCVEntry[T] { return l.entries }

// Lookup returns the exact count of value in the most-common-values list and
// true when it is present. It matches on the hash first (cheap) and confirms with
// eq (the caller's equivalence relation) so a hash collision between two distinct
// values never returns a wrong count. eq is the equivalence relation consistent
// with the hashes the entries carry (the Cypher layer passes expr.Equivalent).
func (l *MCVList[T]) Lookup(hash uint64, value T, eq func(a, b T) bool) (int64, bool) {
	for i := range l.entries {
		if l.entries[i].Hash == hash && eq(l.entries[i].Value, value) {
			return l.entries[i].Count, true
		}
	}
	return 0, false
}
