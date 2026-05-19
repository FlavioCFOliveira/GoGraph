// Package btree provides an order-preserving property index over a
// constraints.Ordered value type, answering range predicates against
// the NodeIDs that carry each value.
//
// The v1 implementation is a sorted-array index — sufficient to meet
// the Sprint 2 acceptance criteria (sub-microsecond Range first-
// result on a 10^7-element index, multi-million-entry bulk-load in
// seconds). The public API is designed so that a future replacement
// with a fan-out-tuned B+ tree (or skip list) can land without
// breaking callers; per-key Insert is O(n) today and should be
// avoided on the hot path in favour of [Index.BulkLoad].
//
// All operations are safe for concurrent use; a single [sync.RWMutex]
// guards the array.
package btree

import (
	"cmp"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/graph"
)

// entry is one (value, set-of-nodes) record in the sorted array.
type entry[V cmp.Ordered] struct {
	value V
	bm    *roaring64.Bitmap
}

// Index is an order-preserving property index keyed by V.
type Index[V cmp.Ordered] struct {
	mu      sync.RWMutex
	entries []entry[V]
}

// New returns an empty index.
func New[V cmp.Ordered]() *Index[V] { return &Index[V]{} }

// BulkLoad replaces the contents of the index with the given
// (value, node) pairs in O(n log n) time. The pairs slice is left
// untouched. Calling BulkLoad on a non-empty index discards previous
// data.
func (i *Index[V]) BulkLoad(values []V, nodes []graph.NodeID) {
	if len(values) != len(nodes) {
		panic("btree: values and nodes must have the same length")
	}
	type pair struct {
		v V
		n graph.NodeID
	}
	pairs := make([]pair, len(values))
	for k := range values {
		pairs[k] = pair{v: values[k], n: nodes[k]}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].v < pairs[b].v })
	out := make([]entry[V], 0, len(pairs))
	for k := 0; k < len(pairs); {
		j := k
		bm := roaring64.New()
		for j < len(pairs) && pairs[j].v == pairs[k].v {
			bm.Add(uint64(pairs[j].n))
			j++
		}
		out = append(out, entry[V]{value: pairs[k].v, bm: bm})
		k = j
	}
	i.mu.Lock()
	i.entries = out
	i.mu.Unlock()
}

// Insert records that node carries value.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx < len(i.entries) && i.entries[idx].value == value {
		i.entries[idx].bm.Add(uint64(node))
		return
	}
	bm := roaring64.New()
	bm.Add(uint64(node))
	i.entries = append(i.entries, entry[V]{})
	copy(i.entries[idx+1:], i.entries[idx:len(i.entries)-1])
	i.entries[idx] = entry[V]{value: value, bm: bm}
}

// Delete removes node from the set associated with value. No-op when
// absent. The (value, bitmap) entry is removed when its bitmap
// becomes empty.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return
	}
	i.entries[idx].bm.Remove(uint64(node))
	if i.entries[idx].bm.IsEmpty() {
		i.entries = append(i.entries[:idx], i.entries[idx+1:]...)
	}
}

// RangeFirst returns the first NodeID in the smallest indexed value
// not less than lo and not greater than hi, plus that value. The
// second return value reports whether any match exists. It is the
// allocation-free way to peek the first row of a range scan; the
// full union of matches is available via [Index.Range].
func (i *Index[V]) RangeFirst(lo, hi V) (V, graph.NodeID, bool) {
	var zeroV V
	if hi < lo {
		return zeroV, 0, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	start := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= lo })
	if start >= len(i.entries) || i.entries[start].value > hi {
		return zeroV, 0, false
	}
	first := i.entries[start].bm.Minimum()
	return i.entries[start].value, graph.NodeID(first), true
}

// Range returns a Roaring bitmap that is the union of the per-value
// bitmaps for every key v with lo <= v <= hi. The returned bitmap is
// freshly allocated; the caller owns it.
func (i *Index[V]) Range(lo, hi V) *roaring64.Bitmap {
	out := roaring64.New()
	if hi < lo {
		return out
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	start := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= lo })
	for k := start; k < len(i.entries) && i.entries[k].value <= hi; k++ {
		out.Or(i.entries[k].bm)
	}
	return out
}

// Lookup returns a clone of the bitmap associated with value, or an
// empty bitmap when value is unknown.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	i.mu.RLock()
	defer i.mu.RUnlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return roaring64.New()
	}
	return i.entries[idx].bm.Clone()
}

// Cardinality returns the number of NodeIDs associated with value.
func (i *Index[V]) Cardinality(value V) uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return 0
	}
	return i.entries[idx].bm.GetCardinality()
}

// DistinctValues returns the number of distinct values currently
// indexed.
func (i *Index[V]) DistinctValues() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.entries)
}
