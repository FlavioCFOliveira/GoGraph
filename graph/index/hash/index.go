// Package hash provides a sharded hash index from arbitrary
// comparable property values to the set of NodeIDs that carry them,
// represented as a 64-bit Roaring bitmap.
//
// The structure answers exact-match property predicates (for example
// "every node where email == 'x@y.com'") in O(1) average time. For
// range predicates use the B+ tree index in package
// gograph/graph/index/btree (Sprint 2, T19).
//
// Index is safe for concurrent use by any number of goroutines; the
// shard sharding aligns with [graph.NodeID]'s low-bit shard scheme.
package hash

import (
	"hash/maphash"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/graph"
)

const (
	shardCount = 256
	shardMask  = shardCount - 1
)

var seed = maphash.MakeSeed()

// Index maps property values of type V to the NodeIDs that carry
// them.
type Index[V comparable] struct {
	shards [shardCount]hashShard[V]
}

type hashShard[V comparable] struct {
	mu      sync.RWMutex
	entries map[V]*roaring64.Bitmap
}

// New returns an empty hash index.
func New[V comparable]() *Index[V] {
	idx := &Index[V]{}
	for i := range idx.shards {
		idx.shards[i].entries = make(map[V]*roaring64.Bitmap)
	}
	return idx
}

func (i *Index[V]) shard(v V) *hashShard[V] {
	return &i.shards[maphash.Comparable(seed, v)&shardMask]
}

// Insert records that node carries the given value.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	s := i.shard(value)
	s.mu.Lock()
	bm, ok := s.entries[value]
	if !ok {
		bm = roaring64.New()
		s.entries[value] = bm
	}
	bm.Add(uint64(node))
	s.mu.Unlock()
}

// Delete removes node from the set associated with value. No-op if
// absent.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	s := i.shard(value)
	s.mu.Lock()
	if bm, ok := s.entries[value]; ok {
		bm.Remove(uint64(node))
		if bm.IsEmpty() {
			delete(s.entries, value)
		}
	}
	s.mu.Unlock()
}

// Lookup returns a clone of the Roaring bitmap of NodeIDs that carry
// the given value, or an empty bitmap when the value is unknown.
// Clone avoids returning the live bitmap to the caller, which could
// otherwise be mutated by concurrent writers.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return roaring64.New()
	}
	out := bm.Clone()
	s.mu.RUnlock()
	return out
}

// Cardinality returns the number of NodeIDs associated with value.
// It is exposed for query planners to choose between index lookup
// and full-scan plans.
func (i *Index[V]) Cardinality(value V) uint64 {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return 0
	}
	c := bm.GetCardinality()
	s.mu.RUnlock()
	return c
}

// Contains reports whether node is in the set associated with value.
// Faster than Lookup when only existence matters.
func (i *Index[V]) Contains(value V, node graph.NodeID) bool {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	c := bm.Contains(uint64(node))
	s.mu.RUnlock()
	return c
}

// DistinctValues returns the number of distinct values currently
// indexed. Exposed for cardinality estimation by the query planner.
func (i *Index[V]) DistinctValues() uint64 {
	var n uint64
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		n += uint64(len(s.entries))
		s.mu.RUnlock()
	}
	return n
}
